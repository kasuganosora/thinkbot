package sandbox

// ============================================================================
// sessionCore — the OS/backend-independent half of an interactive session
//
// The backend (local PTY or channel connection) owns the process and the byte
// source; the core owns the ring buffer, the VT screen, the semantic delta
// log, the Seq/SettledSeq double watermark, the health verdict and the view
// contract. Both backends must produce byte-identical views for the same raw
// stream, so this logic exists exactly once (docs §5 / §9 复用).
// ============================================================================

import (
	"io"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type sessionCore struct {
	id      string
	botID   string
	cfg     SessionConfig
	logger  *zap.SugaredLogger
	command string
	backend string
	pid     int // host pid for local; container-namespace pid for channel
	pgid    int // 0 for channel: the host cannot signal a container pgid directly

	// bufMu guards the decoder/screen/delta/health graph. consume() runs on the
	// readLoop goroutine while view()/RawSince() run on poll / web-console
	// goroutines; a winsize change swaps the screen pointer underneath them.
	// Lock order is always mu -> bufMu, never the reverse.
	bufMu sync.Mutex
	cols  int
	rows  int

	mu        sync.RWMutex
	state     SessionState
	reason    string
	lastSeq   uint64
	exitCode  *int
	orphaned  bool
	startedAt time.Time
	exitAt    time.Time
	lastTouch time.Time

	diag   *diagTail // structured diagnostics / boot failures
	stderr *diagTail // out-of-band stderr tail (never mixed into the PTY)

	ring    *ringBuffer
	decoder *ansiDecoder
	screen  *vtScreen
	delta   *deltaLog
	health  *healthTracker
	// mask is the single ingress point for credential masking. Both the VT
	// screen and the WS binary replay are built from the bytes it returns, so
	// they can never disagree about whether a credential was masked.
	mask *SecretStream

	// alive is supplied by the backend: local checks the child process,
	// channel checks the dedicated connection/session state.
	alive func() bool

	// changeHook is installed by SessionRegistry at Spawn time. It lets the
	// registry broadcast a wake-up to every long-poll waiter without polling
	// the sessions. The callback MUST NOT call back into sessionCore (it only
	// closes a channel under the registry's own mutex), so firing it while
	// holding bufMu is deadlock-free.
	hookMu     sync.RWMutex
	changeHook func()
}

// SetChangeHook is part of the optional changeHookTarget interface. Both
// backends embed *sessionCore, so every SessionHandle inherits it.
func (s *sessionCore) SetChangeHook(fn func()) {
	s.hookMu.Lock()
	s.changeHook = fn
	s.hookMu.Unlock()
}

func (s *sessionCore) fireChange() {
	s.hookMu.RLock()
	fn := s.changeHook
	s.hookMu.RUnlock()
	if fn != nil {
		fn()
	}
}

func newSessionCore(cfg SessionConfig, logger *zap.SugaredLogger,
	id, command, backend string, pid, pgid int, cols, rows uint16) *sessionCore {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	now := time.Now()
	return &sessionCore{
		id: id, cfg: cfg, logger: logger, command: command, backend: backend,
		pid: pid, pgid: pgid, cols: int(cols), rows: int(rows),
		state: SessionRunning, startedAt: now, lastTouch: now,
		diag:    newDiagTail(cfg.DiagTailBytes),
		stderr:  newDiagTail(cfg.DiagTailBytes),
		ring:    newRingBuffer(cfg.RawBufferBytes),
		decoder: newAnsiDecoder(),
		screen:  newVTScreen(int(cols), int(rows)),
		delta:   newDeltaLog(cfg),
		health:  newHealthTracker(cfg),
		mask:    &SecretStream{},
	}
}

// consume is the single ingress for raw terminal bytes.
func (s *sessionCore) consume(chunk []byte) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	// Masking happens BEFORE anything observes the bytes: ring, decoder,
	// screen, delta and health all see the same safe stream. A chunk wholly
	// withheld as a possible credential prefix produces no samples, so it
	// cannot skew seqRate / changedPerChunk.
	s.ingestLocked(s.mask.Feed(chunk))
}

// ingestLocked holds bufMu and consumes already-masked bytes.
func (s *sessionCore) ingestLocked(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	now := time.Now()
	_, _ = s.ring.Write(chunk)
	before := s.screen.Lines()
	evs := s.decoder.Feed(chunk)
	s.screen.FeedEvents(evs)
	after := s.screen.Lines()
	changed := rawChangedLines(before, after)
	s.health.Feed(len(chunk), changed, after, now)
	s.delta.OnBytes(s.ring.Seq(), s.screen.TakeWrites(), now)
	s.delta.MaybeSettle(after, now)
	s.fireChange()
}

// settle must run on a timer even when no bytes arrive: otherwise the stable
// window never expires and the last frame never reaches settledSeq.
func (s *sessionCore) settle(now time.Time) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	s.delta.MaybeSettle(s.screen.Lines(), now)
	s.health.Heartbeat(now)
}

func rawChangedLines(a, b []string) int {
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	n := 0
	for i := 0; i < max; i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			n++
		}
	}
	return n
}

func (s *sessionCore) Poll(sinceSeq uint64, mode string, lines int, wait time.Duration) (SessionView, error) {
	s.Touch()
	if wait > s.cfg.PollWaitMax {
		wait = s.cfg.PollWaitMax
	}
	now := time.Now()
	s.settle(now)
	settled := s.delta.SettledSeq()
	if sinceSeq > settled {
		// Guard against an agent accidentally using seq (raw bytes) as the
		// cursor: clamp instead of silently skipping unsettled content.
		sinceSeq = settled
	}
	if wait > 0 {
		if d, _ := s.delta.Delta(sinceSeq); d == "" {
			deadline := time.Now().Add(wait)
			for time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
				s.settle(time.Now())
				if d, _ = s.delta.Delta(sinceSeq); d != "" {
					break
				}
			}
		}
	}
	return s.viewInternal(sinceSeq, mode, lines, true), nil
}

func (s *sessionCore) view(sinceSeq uint64, mode string, lines int) SessionView {
	return s.viewInternal(sinceSeq, mode, lines, false)
}

// viewInternal builds an agent-facing snapshot. allowTail is true ONLY for
// Poll: View() is called by watch / auto-upgrade on a 50ms tick, so it must
// never pay for a raw-to-text conversion.
func (s *sessionCore) viewInternal(sinceSeq uint64, mode string, lines int, allowTail bool) SessionView {
	s.mu.RLock()
	state, reason, lastSeq, exitCode, orphaned := s.state, s.reason, s.lastSeq, s.exitCode, s.orphaned
	s.mu.RUnlock()
	s.bufMu.Lock()
	defer s.bufMu.Unlock()

	delta, settled := s.delta.Delta(sinceSeq)
	screenLines := s.screen.Lines()
	if lines <= 0 {
		lines = s.rows
	}
	if len(screenLines) > lines {
		screenLines = screenLines[len(screenLines)-lines:]
	}
	procAlive := state == SessionRunning
	if s.alive != nil {
		procAlive = s.alive()
	}
	v := SessionView{
		SessionID:  s.id,
		PID:        s.pid,
		PGID:       s.pgid,
		State:      state,
		Backend:    s.backend,
		Seq:        s.ring.Seq(),
		SettledSeq: settled,
		Delta:      delta,
		Screen:     strings.Join(screenLines, "\n"),
		URLs:       extractURLs(s.ring.Snapshot(), 20),
		ExitCode:   exitCode,
		Truncated:  s.ring.Truncated() || len(strings.Split(delta, "\n")) > s.cfg.DeltaMaxLines,
		Merged:     true,
		StderrTail: s.stderr.String(),
		BootDiag:   s.diag.String(),
		Reason:     reason,
		LastSeq:    lastSeq,
		Orphaned:   orphaned,
		Health:     s.health.Verdict(state, procAlive, screenLines, time.Now()),
	}
	// Credential / connection prompts need an explicit next step. The literal
	// value must never appear in a tool argument: the model writes the
	// {{secret:name}} placeholder registered by secret_input, and the
	// orchestrator resolves it only at tool execution time.
	switch v.Health.Verdict {
	case HealthAwaitingSecret:
		v.Hint = "credential requested: put the secret_input placeholder in send.text; never paste the literal value. Use secret_input action=list/request if no placeholder is ready"
	case HealthConnectFailed:
		v.Hint = "authentication or connection failed: report it, do not retry the same credential blindly"
	}
	if mode == "raw" {
		raw, _ := s.ring.Since(sinceSeq)
		v.Delta = string(raw)
		v.Truncated = v.Truncated || s.ring.Truncated()
	} else if allowTail && (mode == "tail" || (delta == "" && settled > sinceSeq)) {
		// Screen-diff deltas can be empty even while bytes keep arriving: a
		// scrolling CLI, a clear-on-redraw frame, or CR-only progress output
		// leaves no changed rows, and MaybeSettle still advances settledSeq.
		// Without this the agent polls "blind" (delta="" + seq climbing),
		// repeats the identical poll and gets cut off by the step guard.
		// mode=tail is an explicit escape hatch for the same situation.
		tail, truncated := s.plainTailSinceLocked(sinceSeq, settled)
		if tail != "" {
			v.Delta = tail
			v.Truncated = v.Truncated || truncated
		}
	}
	if state == SessionRunning && v.Seq == 0 && time.Since(s.startedAt) > 3*time.Second {
		if v.BootDiag == "" {
			v.BootDiag = "no output yet; no diagnostics available (diagUnavailable)"
		}
		v.Hint = "no output; possible startup failure, check diagnostics"
	}
	if state == SessionLost {
		if v.LastSeq == 0 {
			v.LastSeq = v.SettledSeq
		}
		v.Hint = "session owner is gone; reopen a new session, output up to lastSeq is retained"
	}
	// Egress redaction catches values registered after these bytes arrived.
	// It runs last so mode=raw and the generated bootDiag are covered too.
	redactSessionView(&v)
	return v
}

// sessionTailMaxBytes caps the plain-text fallback so a chatty program cannot
// flood the LLM context or the tool-result log. The orchestrator truncates
// again downstream; this is the first line of defence.
const sessionTailMaxBytes = 16 << 10

// plainTailSinceLocked returns a bounded, control-byte-stripped tail of the raw
// stream between sinceSeq and settledSeq. Caller MUST hold s.bufMu.
//
// Cursor discipline: only bytes up to settledSeq are exposed. The agent's next
// sinceSeq is settledSeq, so the same bytes can never be replayed twice. The
// ring is already ingress-masked; redactSessionView runs afterwards in view()
// to catch secrets registered later.
//
// This is deliberately a LAST RESORT, not a replacement for semantic deltas:
// when the VT screen does change, the diff is far cheaper and more precise.
func (s *sessionCore) plainTailSinceLocked(sinceSeq, settled uint64) (string, bool) {
	if settled <= sinceSeq {
		return "", false
	}
	raw, resync := s.ring.Since(sinceSeq)
	truncated := resync
	if resync {
		// Since() returned the whole retained window [Seq-len, Seq], NOT bytes
		// after sinceSeq. Compute the equivalent cut from that window's base.
		base := s.ring.Seq() - uint64(len(raw))
		if settled > base {
			if want := int(settled - base); want < len(raw) {
				raw = raw[:want]
			}
		}
	} else if want := int(settled - sinceSeq); want < len(raw) {
		raw = raw[:want]
	}
	if len(raw) == 0 {
		return "", false
	}
	// stripTerminalBytes has its own 2 MiB head cap; keep only a recent window
	// so the "tail" really is the tail of this slice, not of the first 2 MiB.
	if len(raw) > 1<<20 {
		raw = raw[len(raw)-1<<20:]
		truncated = true
	}
	text := stripTerminalBytes(raw)
	if len(text) > sessionTailMaxBytes {
		text = text[len(text)-sessionTailMaxBytes:]
		// Never start mid-rune: drop leading UTF-8 continuation bytes.
		for len(text) > 0 && text[0]&0xC0 == 0x80 {
			text = text[1:]
		}
		truncated = true
	}
	lines := strings.Split(text, "\n")
	maxLines := s.cfg.DeltaMaxLines
	if maxLines <= 0 {
		maxLines = 80
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	body := strings.Join(lines, "\n")
	if strings.TrimSpace(body) == "" {
		return "", false
	}
	return "[output with no screen change; plain-text tail]\n" + body, truncated || s.ring.Truncated()
}

// flushMask emits any credential-prefix tail still withheld by the ingress
// masker. It MUST run before a terminal state snapshots lastSeq, otherwise the
// web-console replay would end mid-stream and Seq / lastSeq would disagree.
func (s *sessionCore) flushMask() {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if tail := s.mask.Flush(); len(tail) > 0 {
		s.ingestLocked(tail) // already a marker, never re-mask it
	}
}

// markFinished is the terminal transition for a normal exit / explicit close.
// reason is only applied when nothing more specific was recorded earlier.
func (s *sessionCore) markFinished(code int, reason string) {
	s.flushMask()
	s.mu.Lock()
	if s.state == SessionLost {
		// lost is terminal and must not be silently rewritten as a clean exit.
		s.mu.Unlock()
		return
	}
	s.state = SessionExited
	if s.reason == "" {
		s.reason = reason
	}
	s.exitCode = &code
	s.lastSeq = s.ring.Seq()
	s.exitAt = time.Now()
	r := s.reason
	s.mu.Unlock()
	s.settle(time.Now())
	s.fireChange()
	s.logEvent("session_exit", map[string]any{"exitCode": code, "reason": r})
}

// markLost is the channel-backend "container/daemon went away" transition
// (docs §13 #9): the host never tries to reattach, it reports state=lost with
// lastSeq so the agent can reopen without guessing.
func (s *sessionCore) markLost(reason string) {
	s.flushMask()
	s.mu.Lock()
	if s.state != SessionRunning {
		s.mu.Unlock()
		return
	}
	s.state = SessionLost
	s.reason = reason
	s.lastSeq = s.ring.Seq()
	s.exitAt = time.Now()
	s.mu.Unlock()
	s.settle(time.Now())
	s.fireChange()
	s.logEvent("session_lost", map[string]any{"reason": reason, "lastSeq": s.ring.Seq()})
}

func (s *sessionCore) View() SessionView {
	return s.view(0, "screen", s.rows)
}

// resizeScreen keeps the daemon-side VT grid in sync with the real PTY winsize.
// pty.Setsize (or a channel resize frame) only tells the CHILD to redraw; if the
// grid stayed at spawn-time dimensions, the agent-facing Screen / delta would
// silently disagree with what a human sees in the web console (docs §10 P2).
// RenderScreen is a pure function of the raw stream, so this is idempotent.
// The live decoder is deliberately NOT reset: it may hold a pending escape or
// partial UTF-8 sequence that the next chunk must complete.
func (s *sessionCore) resizeScreen(cols, rows uint16) {
	if cols == 0 || rows == 0 {
		return
	}
	// A resize frame can arrive from a browser over the web console. Cap the
	// grid so an untrusted cols/rows cannot make RenderScreen allocate a huge
	// [][]vtCell. SessionRegistry.Spawn / Resize apply the SAME ClampWinsize
	// caps to the real PTY before calling into a backend, so this is a second
	// line of defence, not the only one (previously the comment claimed callers
	// bounded the PTY, but none did — see the resize feedback-loop fix).
	cc, rr := ClampWinsize(cols, rows)
	c, r := int(cc), int(rr)
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if s.cols == c && s.rows == r {
		return
	}
	s.cols, s.rows = c, r
	s.screen = RenderScreen(s.ring.Snapshot(), c, r)
}

// RawSince is the raw-byte counterpart of Poll. It uses the Seq watermark and
// never touches the semantic per-agent cursor: Poll(mode="raw") would clamp
// sinceSeq to SettledSeq and the registry would advance the cursor by
// SettledSeq, so a WebSocket loop would replay the same bytes on every frame.
func (s *sessionCore) RawSince(seq uint64) RawChunk {
	s.Touch() // an attached human console counts as activity for idle TTL
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	cur := s.ring.Seq()
	if seq > cur {
		seq = cur
	}
	data, gap := s.ring.Since(seq)
	return redactRawChunk(RawChunk{
		Data: data, Seq: cur, State: state,
		Cols: s.cols, Rows: s.rows, Resync: gap,
	})
}

func (s *sessionCore) Touch() {
	s.mu.Lock()
	s.lastTouch = time.Now()
	s.mu.Unlock()
}

func (s *sessionCore) LastTouch() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastTouch
}

func (s *sessionCore) BotID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.botID
}

func (s *sessionCore) SetOwner(botID string) {
	s.mu.Lock()
	s.botID = botID
	s.mu.Unlock()
}

func (s *sessionCore) ID() string { return s.id }

func (s *sessionCore) noteDiag(msg string) {
	_, _ = s.diag.Write([]byte(msg + "\n"))
}

// AttachDiag is the side-channel ingress. Backends feed CLI structured log
// tails here; nothing is written into the PTY.
func (s *sessionCore) AttachDiag(r io.Reader) {
	if r == nil {
		return
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_, _ = s.stderr.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
}

func (s *sessionCore) logEvent(event string, fields map[string]any) {
	if s.logger == nil {
		return
	}
	args := []any{"event", event, "sessionId", s.id, "pid", s.pid,
		"pgid", s.pgid, "botID", s.BotID(), "backend", s.backend,
		"seq", s.ring.Seq(), "settledSeq", s.delta.SettledSeq(),
		"elapsedMs", time.Since(s.startedAt).Milliseconds()}
	for k, v := range fields {
		args = append(args, k, v)
	}
	s.logger.Infow("interactive_session", args...)
}

// redactCommand keeps only a short, non-secret prefix for logs.
func redactCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if len(cmd) > 120 {
		cmd = cmd[:120] + "..."
	}
	return cmd
}
