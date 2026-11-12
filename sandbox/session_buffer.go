package sandbox

// ============================================================================
// Output governance for interactive sessions
//
// Everything in this file is OS-independent and side-effect free with respect
// to the process: render(raw) is a pure function of the raw byte stream, so a
// screen can be rebuilt idempotently from the ring buffer (docs §5 幂等性).
//
// Two watermarks are maintained:
//   Seq        absolute raw byte count, only for raw replay / diagnostics
//   SettledSeq bytes covered by semantic delta records; sinceSeq must use this
// Using Seq as sinceSeq would silently drop content that arrived after the last
// settlement but before the current poll (docs §5 双水位线).
// ============================================================================

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ----------------------------------------------------------------------------
// raw ring buffer
// ----------------------------------------------------------------------------

type ringBuffer struct {
	mu        sync.RWMutex
	buf       []byte
	capacity  int
	head      int // index of the oldest byte
	length    int
	total     uint64 // absolute bytes ever written == Seq
	truncated bool
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity <= 0 {
		capacity = 1 << 20
	}
	return &ringBuffer{buf: make([]byte, capacity), capacity: capacity}
}

// Write never blocks and never rejects: old bytes are overwritten once full.
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	r.total += uint64(n)
	if n >= r.capacity {
		copy(r.buf, p[n-r.capacity:])
		r.head = 0
		r.length = r.capacity
		r.truncated = true
		return n, nil
	}
	free := r.capacity - r.length
	if n > free {
		over := n - free
		r.head = (r.head + over) % r.capacity
		r.length -= over
		r.truncated = true
	}
	for _, b := range p {
		idx := (r.head + r.length) % r.capacity
		r.buf[idx] = b
		r.length++
	}
	return n, nil
}

func (r *ringBuffer) Seq() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total
}

func (r *ringBuffer) Truncated() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.truncated
}

// Snapshot returns the oldest available byte through the newest, in order.
func (r *ringBuffer) Snapshot() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]byte, r.length)
	for i := 0; i < r.length; i++ {
		out[i] = r.buf[(r.head+i)%r.capacity]
	}
	return out
}

// Since returns raw bytes after absolute position seq. The bool reports a GAP
// (seq predates the retained window), not "the ring has ever overflowed":
// Truncated() is sticky by design, and using it here would make every web
// console frame after the first overflow look like a resync (docs §10.1).
func (r *ringBuffer) Since(seq uint64) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	oldest := r.total - uint64(r.length)
	if seq > r.total {
		seq = r.total
	}
	gap := seq < oldest
	start := uint64(0)
	if seq > oldest {
		start = seq - oldest
	}
	if start >= uint64(r.length) {
		return nil, gap
	}
	out := make([]byte, r.length-int(start))
	for i := range out {
		out[i] = r.buf[(r.head+int(start)+i)%r.capacity]
	}
	return out, gap
}

// ----------------------------------------------------------------------------
// incremental ANSI / UTF-8 decoder -> terminal events
// ----------------------------------------------------------------------------

type termKind uint8

const (
	tkText termKind = iota
	tkNewline
	tkCarriageReturn
	tkBackspace
	tkTab
	tkCursorMove // a = dRow, b = dCol
	tkCursorPos  // a = row (1-based), b = col (1-based)
	tkEraseDisplay
	tkEraseLine
	tkAltScreen // a = 1 on, 0 off
	tkSaveCursor
	tkRestoreCursor
	tkReverseIndex
	tkIgnore
)

type termEvent struct {
	kind termKind
	ch   rune
	a, b int
}

// parseToken parses exactly one token from b. It is a pure function: an
// incomplete UTF-8 rune or escape sequence returns complete=false and the
// caller keeps those bytes for the next Feed. This is what makes arbitrary
// chunk boundaries produce the same event stream.
func parseToken(b []byte) (n int, evs []termEvent, complete bool) {
	if len(b) == 0 {
		return 0, nil, true
	}
	c := b[0]
	switch c {
	case 0x1b: // ESC
		return parseEscape(b)
	case '\n', 0x0b, 0x0c:
		return 1, []termEvent{{kind: tkNewline}}, true
	case '\r':
		return 1, []termEvent{{kind: tkCarriageReturn}}, true
	case '\b':
		return 1, []termEvent{{kind: tkBackspace}}, true
	case '\t':
		return 1, []termEvent{{kind: tkTab}}, true
	case 0x07, 0x00: // BEL / NUL
		return 1, []termEvent{{kind: tkIgnore}}, true
	}
	if c < 0x20 || c == 0x7f {
		return 1, []termEvent{{kind: tkIgnore}}, true
	}
	if c < utf8.RuneSelf {
		return 1, []termEvent{{kind: tkText, ch: rune(c)}}, true
	}
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError && size == 1 {
		if !utf8.FullRune(b) {
			return 0, nil, false
		}
		return 1, []termEvent{{kind: tkText, ch: utf8.RuneError}}, true
	}
	return size, []termEvent{{kind: tkText, ch: r}}, true
}

func parseEscape(b []byte) (n int, evs []termEvent, complete bool) {
	if len(b) < 2 {
		return 0, nil, false
	}
	switch b[1] {
	case '[': // CSI
		i := 2
		for i < len(b) {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return i + 1, csiEvents(string(b[2:i]), b[i]), true
			}
			i++
		}
		return 0, nil, false
	case ']': // OSC: terminated by BEL or ST (ESC \)
		for i := 2; i < len(b); i++ {
			if b[i] == 0x07 {
				return i + 1, []termEvent{{kind: tkIgnore}}, true
			}
			if b[i] == 0x1b {
				if i+1 >= len(b) {
					return 0, nil, false
				}
				if b[i+1] == '\\' {
					return i + 2, []termEvent{{kind: tkIgnore}}, true
				}
			}
		}
		return 0, nil, false
	case '(', ')', '*', '+': // charset designation: one more byte
		if len(b) < 3 {
			return 0, nil, false
		}
		return 3, []termEvent{{kind: tkIgnore}}, true
	case '7':
		return 2, []termEvent{{kind: tkSaveCursor}}, true
	case '8':
		return 2, []termEvent{{kind: tkRestoreCursor}}, true
	case 'M':
		return 2, []termEvent{{kind: tkReverseIndex}}, true
	case 'D':
		return 2, []termEvent{{kind: tkCursorMove, a: 1}}, true
	case 'E':
		return 2, []termEvent{{kind: tkNewline}}, true
	default:
		return 2, []termEvent{{kind: tkIgnore}}, true
	}
}

func csiEvents(params string, final byte) []termEvent {
	private := strings.HasPrefix(params, "?")
	body := strings.TrimPrefix(params, ">")
	nums := make([]int, 0, 4)
	if body != "" {
		for _, part := range strings.Split(body, ";") {
			v := 0
			for _, ch := range part {
				if ch < '0' || ch > '9' {
					v = 0
					break
				}
				v = v*10 + int(ch-'0')
			}
			nums = append(nums, v)
		}
	}
	arg := func(i, def int) int {
		if i < len(nums) && nums[i] > 0 {
			return nums[i]
		}
		return def
	}
	first := func(def int) int {
		if len(nums) > 0 {
			return nums[0]
		}
		return def
	}
	switch final {
	case 'm', 'h', 'l':
		if private && (final == 'h' || final == 'l') && len(nums) > 0 && nums[0] == 1049 {
			on := final == 'h'
			v := 0
			if on {
				v = 1
			}
			return []termEvent{{kind: tkAltScreen, a: v}}
		}
		return []termEvent{{kind: tkIgnore}}
	case 'H', 'f':
		return []termEvent{{kind: tkCursorPos, a: arg(0, 1), b: arg(1, 1)}}
	case 'A':
		return []termEvent{{kind: tkCursorMove, a: -first(1)}}
	case 'B':
		return []termEvent{{kind: tkCursorMove, a: first(1)}}
	case 'C':
		return []termEvent{{kind: tkCursorMove, b: first(1)}}
	case 'D':
		return []termEvent{{kind: tkCursorMove, b: -first(1)}}
	case 'G':
		return []termEvent{{kind: tkCursorPos, a: 0, b: first(1)}}
	case 'd':
		return []termEvent{{kind: tkCursorPos, a: first(1), b: 0}}
	case 'J':
		return []termEvent{{kind: tkEraseDisplay, a: first(0)}}
	case 'K':
		return []termEvent{{kind: tkEraseLine, a: first(0)}}
	case 'r': // DECSTBM: remember the region but keep scrolling simple
		return []termEvent{{kind: tkIgnore}}
	case 's':
		return []termEvent{{kind: tkSaveCursor}}
	case 'u':
		return []termEvent{{kind: tkRestoreCursor}}
	default:
		return []termEvent{{kind: tkIgnore}}
	}
}

type ansiDecoder struct {
	pending []byte
}

func newAnsiDecoder() *ansiDecoder { return &ansiDecoder{} }

// Feed consumes a chunk and returns every event that is now fully determined.
func (d *ansiDecoder) Feed(p []byte) []termEvent {
	buf := append(d.pending, p...)
	var evs []termEvent
	i := 0
	for i < len(buf) {
		n, one, ok := parseToken(buf[i:])
		if !ok {
			break
		}
		evs = append(evs, one...)
		i += n
	}
	d.pending = append(d.pending[:0], buf[i:]...)
	return evs
}

// Flush resolves a truncated tail at end of stream. Incomplete escape
// sequences carry no meaning; an incomplete UTF-8 rune becomes U+FFFD.
func (d *ansiDecoder) Flush() []termEvent {
	tail := d.pending
	d.pending = nil
	if len(tail) == 0 {
		return nil
	}
	if tail[0] == 0x1b || (tail[0] < 0x20 && tail[0] != '\r' && tail[0] != '\n') {
		return nil
	}
	if !utf8.FullRune(tail) {
		return []termEvent{{kind: tkText, ch: utf8.RuneError}}
	}
	n, evs, ok := parseToken(tail)
	if ok && n > 0 {
		return evs
	}
	return nil
}

// ----------------------------------------------------------------------------
// VT screen grid
// ----------------------------------------------------------------------------

type vtCell struct {
	ch   rune
	cont bool // right half of a double-width rune
}

func (c vtCell) empty() bool { return c.ch == 0 || c.ch == ' ' }

type vtScreen struct {
	cols, rows int
	grid       [][]vtCell
	curR, curC int
	top, bot   int
	alt        bool
	mainGrid   [][]vtCell
	mainR      int
	mainC      int
	savedR     int
	savedC     int
	scrollback [][]vtCell
	writes     map[int]int // row -> times written since last settlement
}

func newVTScreen(cols, rows int) *vtScreen {
	if cols < 20 {
		cols = 20
	}
	if rows < 5 {
		rows = 5
	}
	v := &vtScreen{cols: cols, rows: rows, bot: rows - 1, writes: map[int]int{}}
	v.grid = v.blankGrid()
	return v
}

func (v *vtScreen) blankGrid() [][]vtCell {
	g := make([][]vtCell, v.rows)
	for i := range g {
		g[i] = make([]vtCell, v.cols)
		for j := range g[i] {
			g[i][j].ch = ' '
		}
	}
	return g
}

func (v *vtScreen) Apply(ev termEvent) {
	switch ev.kind {
	case tkText:
		v.put(ev.ch)
	case tkNewline:
		v.curC = 0
		v.lineFeed()
	case tkCarriageReturn:
		v.curC = 0
	case tkBackspace:
		if v.curC > 0 {
			v.curC--
		}
	case tkTab:
		v.curC = ((v.curC / 8) + 1) * 8
		if v.curC >= v.cols {
			v.curC = v.cols - 1
		}
	case tkCursorMove:
		v.curR = clamp(v.curR+ev.a, 0, v.rows-1)
		v.curC = clamp(v.curC+ev.b, 0, v.cols-1)
	case tkCursorPos:
		if ev.a > 0 {
			v.curR = clamp(ev.a-1, 0, v.rows-1)
		}
		if ev.b > 0 {
			v.curC = clamp(ev.b-1, 0, v.cols-1)
		}
	case tkEraseDisplay:
		if ev.a == 2 || ev.a == 3 {
			v.grid = v.blankGrid()
			v.curR, v.curC = 0, 0
		} else if ev.a == 0 {
			v.eraseRowFrom(v.curR, v.curC)
			for r := v.curR + 1; r < v.rows; r++ {
				v.grid[r] = blankRow(v.cols)
			}
		}
	case tkEraseLine:
		switch ev.a {
		case 0:
			v.eraseRowFrom(v.curR, v.curC)
		case 1:
			for c := 0; c <= v.curC && c < v.cols; c++ {
				v.grid[v.curR][c] = vtCell{ch: ' '}
			}
		default:
			v.grid[v.curR] = blankRow(v.cols)
		}
	case tkAltScreen:
		if ev.a == 1 && !v.alt {
			v.mainGrid, v.mainR, v.mainC = v.grid, v.curR, v.curC
			v.grid = v.blankGrid()
			v.curR, v.curC = 0, 0
			v.alt = true
		} else if ev.a == 0 && v.alt {
			v.grid, v.curR, v.curC = v.mainGrid, v.mainR, v.mainC
			v.alt = false
		}
	case tkSaveCursor:
		v.savedR, v.savedC = v.curR, v.curC
	case tkRestoreCursor:
		v.curR, v.curC = clamp(v.savedR, 0, v.rows-1), clamp(v.savedC, 0, v.cols-1)
	case tkReverseIndex:
		if v.curR == v.top {
			v.scrollDown()
		} else {
			v.curR--
		}
	}
}

func (v *vtScreen) eraseRowFrom(r, c int) {
	if r < 0 || r >= v.rows {
		return
	}
	for i := c; i < v.cols; i++ {
		v.grid[r][i] = vtCell{ch: ' '}
	}
}

func blankRow(cols int) []vtCell {
	row := make([]vtCell, cols)
	for i := range row {
		row[i].ch = ' '
	}
	return row
}

func clamp(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func runeWidth(r rune) int {
	if r == 0 || r < 0x20 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	switch {
	case r >= 0x1100 && r <= 0x115f,
		r >= 0x2e80 && r <= 0xa4cf,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe10 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

func (v *vtScreen) put(r rune) {
	w := runeWidth(r)
	if w == 0 {
		return
	}
	if v.curC+w > v.cols {
		v.curC = 0
		v.lineFeed()
	}
	if v.curR < 0 || v.curR >= v.rows {
		return
	}
	if w == 2 && v.curC+1 < v.cols {
		if v.grid[v.curR][v.curC+1].cont {
			v.grid[v.curR][v.curC+1] = vtCell{ch: ' '}
		}
		v.grid[v.curR][v.curC] = vtCell{ch: r}
		v.grid[v.curR][v.curC+1] = vtCell{ch: 0, cont: true}
	} else {
		v.grid[v.curR][v.curC] = vtCell{ch: r}
	}
	v.writes[v.curR]++
	v.curC += w
	if v.curC >= v.cols {
		v.curC = 0
		v.lineFeed()
	}
}

func (v *vtScreen) lineFeed() {
	if v.curR >= v.bot {
		v.scrollUp()
		v.curR = v.bot
		return
	}
	v.curR++
}

func (v *vtScreen) scrollUp() {
	top := v.grid[v.top]
	if !v.alt {
		cp := make([]vtCell, len(top))
		copy(cp, top)
		v.scrollback = append(v.scrollback, cp)
		if len(v.scrollback) > 200 {
			v.scrollback = v.scrollback[len(v.scrollback)-200:]
		}
	}
	for r := v.top; r < v.bot; r++ {
		v.grid[r] = v.grid[r+1]
	}
	v.grid[v.bot] = blankRow(v.cols)
}

func (v *vtScreen) scrollDown() {
	for r := v.bot; r > v.top; r-- {
		v.grid[r] = v.grid[r-1]
	}
	v.grid[v.top] = blankRow(v.cols)
}

// Lines returns the visible screen with trailing blanks removed. Trailing
// empty lines are dropped as well, which keeps the LLM-facing view compact.
func (v *vtScreen) Lines() []string {
	out := make([]string, 0, v.rows)
	for _, row := range v.grid {
		last := -1
		for i, c := range row {
			if !c.empty() && !c.cont {
				last = i
			}
		}
		if last < 0 {
			out = append(out, "")
			continue
		}
		var sb strings.Builder
		for i := 0; i <= last; i++ {
			c := row[i]
			if c.cont {
				continue
			}
			if c.ch == 0 {
				sb.WriteByte(' ')
			} else {
				sb.WriteRune(c.ch)
			}
		}
		out = append(out, sb.String())
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (v *vtScreen) Screen() string { return strings.Join(v.Lines(), "\n") }

func (v *vtScreen) TakeWrites() map[int]int {
	w := v.writes
	v.writes = map[int]int{}
	return w
}

// Render rebuilds a screen from a raw byte stream. Pure function: calling it
// twice on the same bytes, or feeding the same bytes in arbitrary chunks,
// yields the same result (docs §5 幂等性).
func RenderScreen(raw []byte, cols, rows int) *vtScreen {
	d := newAnsiDecoder()
	s := newVTScreen(cols, rows)
	for _, ev := range d.Feed(raw) {
		s.Apply(ev)
	}
	for _, ev := range d.Flush() {
		s.Apply(ev)
	}
	return s
}

// FeedEvents applies already-decoded events (used by the live readLoop, which
// keeps one decoder alive across chunks).
func (s *vtScreen) FeedEvents(evs []termEvent) {
	for _, ev := range evs {
		s.Apply(ev)
	}
}

// ----------------------------------------------------------------------------
// semantic delta log (stable window + collapse)
// ----------------------------------------------------------------------------

type deltaRecord struct {
	ToSeq     uint64
	Lines     []string
	Collapsed map[int]int // line index within this record -> rewrite count
}

type deltaLog struct {
	mu           sync.Mutex
	records      []deltaRecord
	settledLines []string
	settledSeq   uint64
	stableWindow time.Duration
	lastByte     time.Time
	seqAtByte    uint64
	windowWrites map[int]int
	collapseK    int
	maxLines     int
}

func newDeltaLog(cfg SessionConfig) *deltaLog {
	return &deltaLog{
		stableWindow: cfg.StableWindow,
		windowWrites: map[int]int{},
		collapseK:    cfg.CollapseThreshold,
		maxLines:     cfg.DeltaMaxLines,
	}
}

func (d *deltaLog) OnBytes(seq uint64, writes map[int]int, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastByte = now
	d.seqAtByte = seq
	for row, n := range writes {
		d.windowWrites[row] += n
	}
}

// MaybeSettle publishes a semantic delta once the stream has been quiet for
// stableWindow. Intermediate repaints collapse into the final state of a row.
func (d *deltaLog) MaybeSettle(lines []string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastByte.IsZero() || d.seqAtByte <= d.settledSeq {
		return false
	}
	if now.Sub(d.lastByte) < d.stableWindow {
		return false
	}
	prev := d.settledLines
	max := len(lines)
	if len(prev) > max {
		max = len(prev)
	}
	changed := make([]string, 0, 8)
	collapsed := map[int]int{}
	for i := 0; i < max; i++ {
		var a, b string
		if i < len(prev) {
			a = prev[i]
		}
		if i < len(lines) {
			b = lines[i]
		}
		if a == b {
			continue
		}
		if b == "" {
			continue // disappearing rows are not informative noise
		}
		if n := d.windowWrites[i]; n >= d.collapseK {
			collapsed[len(changed)] = n
		}
		changed = append(changed, b)
	}
	if len(changed) == 0 {
		// No semantic change (pure cursor movement / SGR). Still advance the
		// watermark so sinceSeq never stalls.
		d.settledSeq = d.seqAtByte
		d.settledLines = append([]string(nil), lines...)
		d.windowWrites = map[int]int{}
		return true
	}
	if len(changed) > d.maxLines {
		changed = changed[len(changed)-d.maxLines:]
	}
	d.records = append(d.records, deltaRecord{
		ToSeq:     d.seqAtByte,
		Lines:     changed,
		Collapsed: collapsed,
	})
	if len(d.records) > 256 {
		d.records = d.records[len(d.records)-256:]
	}
	d.settledSeq = d.seqAtByte
	d.settledLines = append([]string(nil), lines...)
	d.windowWrites = map[int]int{}
	return true
}

func (d *deltaLog) SettledSeq() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.settledSeq
}

// Delta returns all settled records after sinceSeq. Repeating the same
// sinceSeq returns the same string (idempotent cursor).
func (d *deltaLog) Delta(sinceSeq uint64) (string, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var parts []string
	for _, rec := range d.records {
		if rec.ToSeq <= sinceSeq {
			continue
		}
		for i, line := range rec.Lines {
			if n, ok := rec.Collapsed[i]; ok {
				parts = append(parts, fmt.Sprintf("collapsed:%d %s", n, line))
			} else {
				parts = append(parts, line)
			}
		}
	}
	return strings.Join(parts, "\n"), d.settledSeq
}

// ----------------------------------------------------------------------------
// URL extraction (always on the full raw snapshot, never per chunk)
// ----------------------------------------------------------------------------

var urlPattern = regexp.MustCompile(`https?://[^\s\x00-\x1f\x7f<>\"'\x1b]+`)

func extractURLs(raw []byte, limit int) []string {
	if limit <= 0 {
		limit = 20
	}
	matches := urlPattern.FindAll(raw, -1)
	seen := map[string]bool{}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		u := strings.TrimRight(string(m), ".,;:)]}>")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// semantic normalization (view only; never applied to raw/urls/bootDiag)
// ----------------------------------------------------------------------------

var (
	numPattern     = regexp.MustCompile(`\d+(\.\d+)?`)
	durationPat    = regexp.MustCompile(`\d+(\.\d+)?\s*(ms|s|m|h)(?:\s|·|$)`)
	spinnerPattern = regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏|/\\\-]`)
)

func semanticLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		s := durationPat.ReplaceAllString(l, "<t>")
		s = numPattern.ReplaceAllString(s, "N")
		s = spinnerPattern.ReplaceAllString(s, "")
		out[i] = strings.Join(strings.Fields(s), " ")
	}
	return out
}

func semanticHash(lines []string) string {
	h := sha256.New()
	for _, l := range semanticLines(lines) {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ----------------------------------------------------------------------------
// health tracker (daemon-internal signals only)
// ----------------------------------------------------------------------------

type healthSample struct {
	at      time.Time
	bytes   int
	changed int
}

var (
	actionablePattern = regexp.MustCompile(`(?i)(do you want to|are you sure|yes,\s*(i|allow)|\[y/n\]|\(yes/no|allow once|always allow|reject|deny\b|select an option|press enter to (confirm|allow)|continue connecting)`)
	// Credential prompts are NOT "actionable": the correct next move is a
	// secret write, and a normal send would put the literal credential in the
	// tool call history. Keep them in their own verdict.
	secretPattern = regexp.MustCompile(`(?i)(password\s*:|passphrase|verification code|\bpin\s*[?:]|authenticator code|enter.*(?:otp|one[- ]time))`)
	// ssh / network failures. Must not match a normal "Connection to host
	// closed." on a clean exit.
	failurePattern = regexp.MustCompile(`(?i)(permission denied|authentication failed|access denied|connection (?:refused|timed out|reset|closed by)|no route to host|host key verification failed|operation timed out|too many authentication failures|network is unreachable)`)
	waitingPattern = regexp.MustCompile(`(?i)(working|waking|thinking|loading|waiting|processing|preparing|running\.\.\.)`)
	// A remote shell prompt is an input-ready signal too. The old pattern only
	// recognised a bare "> " / "❯ " / "» ", so ssh's "user@host:~# " never
	// registered as idle-and-waiting.
	inputPattern = regexp.MustCompile(`(?m)(^|\s)[>❯»]\s|[@:][^\n]*[#$]\s*$|(?:^|\n)\s*\$\s*$`)
	// A remote shell prompt is the strongest "I am idle, it is your turn"
	// signal. Deliberately NARROWER than inputPattern: a TUI menu option like
	// "> 1. Yes" also matches inputPattern but is a pending decision, not an
	// idle shell.
	shellPromptPattern = regexp.MustCompile(`(?:^|[\s\[])[\w.-]+@[\w.-]+[^\n]*[#$]\s*$|(?:^|\n)\s*\$\s*$`)
)

// healthTailLines bounds context detection to the ACTIVE tail of the VT screen.
// A full-screen match is wrong for real TUI/ssh sessions: the first-connect
// "Are you sure you want to continue connecting" stays in the 30/40-row grid
// long after it was answered, and made every later verdict awaiting_action
// forever (measured on a real ssh login: 8/8 stages misclassified).
// healthPromptLines is a tighter window for connection failures, so a stale
// "Permission denied" from a previous attempt does not poison a later login.
// Blank rows below the cursor are dropped first so they do not dilute the tail.
const (
	healthTailLines   = 8
	healthPromptLines = 3
)

func healthNonEmptyTail(lines []string, n int) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	lines = lines[:end]
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func healthActiveTail(lines []string) []string {
	return healthNonEmptyTail(lines, healthTailLines)
}

// healthLastLine is the CURRENT prompt row. Anything above it is scrollback,
// so a historical confirmation cannot outrank what the program asks now.
func healthLastLine(lines []string) string {
	t := healthNonEmptyTail(lines, 1)
	if len(t) == 0 {
		return ""
	}
	return t[len(t)-1]
}

type healthTracker struct {
	mu       sync.Mutex
	window   time.Duration
	samples  []healthSample
	semHash  string
	semSince time.Time
	beat     time.Time
	seqRate  float64
	ratioMin float64
	beatMax  time.Duration
}

func newHealthTracker(cfg SessionConfig) *healthTracker {
	return &healthTracker{
		window:   cfg.HealthWindow,
		ratioMin: cfg.ChangeRatioMin,
		seqRate:  cfg.SeqRateThreshold,
		beatMax:  cfg.ReadLoopHeartbeat,
		beat:     time.Now(),
		semSince: time.Now(),
	}
}

func (h *healthTracker) Feed(nBytes, changedLines int, lines []string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples = append(h.samples, healthSample{at: now, bytes: nBytes, changed: changedLines})
	cut := now.Add(-h.window)
	i := 0
	for i < len(h.samples) && h.samples[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		h.samples = h.samples[i:]
	}
	h.beat = now
	hash := semanticHash(lines)
	if hash != h.semHash {
		h.semHash = hash
		h.semSince = now
	}
}

func (h *healthTracker) Heartbeat(now time.Time) {
	h.mu.Lock()
	h.beat = now
	h.mu.Unlock()
}

// Verdict applies the ordering fixed by the PoC (docs §3.5): actionable must
// beat progressing; changedPerChunk must beat screenStableMs; inputReady must
// not be vetoed by a static "Waking..." line.
func (h *healthTracker) Verdict(state SessionState, procAlive bool, lines []string, now time.Time) SessionHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	var totalBytes, totalChanged, chunks int
	for _, s := range h.samples {
		totalBytes += s.bytes
		totalChanged += s.changed
		chunks++
	}
	span := h.window.Seconds()
	if span <= 0 {
		span = 1
	}
	rate := float64(totalBytes) / span
	ratio := 0.0
	if chunks > 0 {
		ratio = float64(totalChanged) / float64(chunks)
	}
	tail := healthActiveTail(lines)
	raw := strings.Join(tail, "\n")
	last := healthLastLine(lines)
	actionable := actionablePattern.MatchString(raw)
	needSecret := secretPattern.MatchString(raw)
	// Failures use a tighter window: a "Permission denied" from a previous
	// attempt must not keep a later successful login in connect_failed.
	connectFailed := failurePattern.MatchString(strings.Join(healthNonEmptyTail(lines, healthPromptLines), "\n"))
	waiting := waitingPattern.MatchString(raw)
	inputReady := inputPattern.MatchString(raw)
	secretOnLast := secretPattern.MatchString(last)
	actionOnLast := actionablePattern.MatchString(last)
	failureOnLast := failurePattern.MatchString(last)
	// A shell prompt on the current row means the program is idle NOW. This
	// must outrank "actionable" / "password" text still visible in scrollback
	// (real ssh: after login, the host-key question stayed on screen forever).
	shellPrompt := shellPromptPattern.MatchString(last) &&
		!secretOnLast && !actionOnLast && !failureOnLast
	stableMs := now.Sub(h.semSince).Milliseconds()
	readLoopAlive := now.Sub(h.beat) <= h.beatMax

	v := SessionHealth{
		SeqRate:         rate,
		ChangedPerChunk: ratio,
		ScreenStableMs:  stableMs,
		Actionable:      actionable,
		InputReady:      inputReady,
		Waiting:         waiting,
		ReadLoopAlive:   readLoopAlive,
		ProcAlive:       procAlive,
	}
	switch {
	case state != SessionRunning || !procAlive:
		v.Verdict, v.Evidence = HealthExited, "process already exited"
	case !readLoopAlive:
		v.Verdict, v.Evidence = HealthUnhealthyDaemon, "readLoop heartbeat stopped"
	case failureOnLast || (connectFailed && !shellPrompt):
		v.Verdict, v.Evidence = HealthConnectFailed, "authentication or connection failure on the current prompt"
	case secretOnLast:
		v.Verdict, v.Evidence = HealthAwaitingSecret, "credential prompt on the current row; use a secret placeholder"
	case shellPrompt:
		v.Verdict, v.Evidence = HealthIdleAwaitingAgent, "shell prompt on the current row, no pending decision"
	case actionable:
		v.Verdict, v.Evidence = HealthAwaitingAction, "screen shows an actionable prompt"
	case needSecret:
		v.Verdict, v.Evidence = HealthAwaitingSecret, "credential prompt visible; use a secret placeholder"
	case rate > h.seqRate && ratio >= h.ratioMin:
		v.Verdict = HealthProgressing
		v.Evidence = fmt.Sprintf("seq growing and %.2f lines changed/chunk", ratio)
	case rate > h.seqRate && ratio < 1.3:
		v.Verdict = HealthSpinning
		v.Evidence = "output without semantic progress"
		if !waiting {
			v.Verdict = HealthSuspectedStuckNoProgress
			v.Evidence = "output but no semantic progress and no waiting indicator"
		}
	case inputReady:
		v.Verdict, v.Evidence = HealthIdleAwaitingAgent, "input prompt visible, no active output"
	case stableMs > 3000:
		v.Verdict, v.Evidence = HealthSuspectedStuck, "screen unchanged and no actionable UI"
	default:
		v.Verdict, v.Evidence = HealthIdle, "recent activity, nothing actionable"
	}
	return v
}

// ----------------------------------------------------------------------------
// diagnostic tail (side channel, never written into the PTY)
// ----------------------------------------------------------------------------

type diagTail struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	maxSize int
}

func newDiagTail(maxSize int) *diagTail {
	if maxSize <= 0 {
		maxSize = 4096
	}
	return &diagTail{maxSize: maxSize}
}

func (d *diagTail) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	d.buf.Write(p)
	if d.buf.Len() > d.maxSize {
		extra := d.buf.Len() - d.maxSize
		kept := append([]byte(nil), d.buf.Bytes()[extra:]...)
		d.buf.Reset()
		d.buf.Write(kept)
	}
	return n, nil
}

func (d *diagTail) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

// ----------------------------------------------------------------------------
// env allowlist (docs §3.3: never inherit os.Environ() verbatim)
// ----------------------------------------------------------------------------

var sessionEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "SHELL": true,
	"LANG": true, "LC_ALL": true, "TERM": true, "TZ": true,
	"TMPDIR": true,
}

var sessionEnvDenyParts = []string{
	"_PRODUCT_CONFIG", "CLIENT_INFO_", "WORKBUDDY_", "CODEBUDDY_",
	"_TOKEN", "_SECRET", "_API_KEY", "_PASSWORD", "PROXY",
}

// buildSessionEnv assembles a whitelist env. It returns the env plus the
// number of dropped host variables (count only — never log names or values).
func buildSessionEnv(extra []string, term string, cols, rows uint16) ([]string, int) {
	out := make([]string, 0, 12)
	for _, kv := range extra {
		key := strings.SplitN(kv, "=", 2)[0]
		if key == "" {
			continue
		}
		upper := strings.ToUpper(key)
		if sessionEnvAllowlist[upper] {
			continue // injected below, avoid duplicates
		}
		denied := false
		for _, part := range sessionEnvDenyParts {
			if strings.Contains(upper, part) {
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range []string{"PATH", "HOME", "USER", "SHELL", "LANG", "LC_ALL", "TZ"} {
		if v, ok := lookupEnv(extra, kv); ok {
			out = append(out, kv+"="+v)
			continue
		}
		if v := safeHostEnv(kv); v != "" {
			// Host PATH/HOME are inherited deliberately; product-config and
			// credential-like variables are never taken from the host.
			out = append(out, kv+"="+v)
		}
	}
	out = append(out,
		"TERM="+term,
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
	)
	// Count host-only product/credential variables that were skipped, without
	// recording their names or values.
	return out, countDroppedHostEnv()
}

// buildContainerSessionEnv is the channel-backend counterpart. It must NOT
// copy host PATH/HOME/SHELL: the daemon's childEnv() already starts from the
// container's own os.Environ() and scrubs credential-like keys. Injecting a
// macOS host PATH or /Users/... HOME into a Linux container silently breaks
// shell lookup and CLI config discovery.
func buildContainerSessionEnv(extra []string, term string, cols, rows uint16) ([]string, int) {
	out := make([]string, 0, len(extra)+3)
	for _, kv := range extra {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		upper := strings.ToUpper(key)
		denied := false
		for _, part := range sessionEnvDenyParts {
			if strings.Contains(upper, part) {
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"TERM="+term,
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
	)
	return out, countDroppedHostEnv()
}

func lookupEnv(env []string, key string) (string, bool) {
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], key) {
			return parts[1], true
		}
	}
	return "", false
}

// safeHostEnv only exposes the small set of neutral variables a shell needs.
// It deliberately never walks os.Environ() into the child.
func safeHostEnv(key string) string {
	return os.Getenv(key)
}

// countDroppedHostEnv reports how many host variables look like product
// configuration or credentials. Only the count is logged; names and values
// never leave this function (docs §9 env 脱敏).
func countDroppedHostEnv() int {
	n := 0
	for _, kv := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		for _, part := range sessionEnvDenyParts {
			if strings.Contains(key, part) {
				n++
				break
			}
		}
	}
	return n
}
