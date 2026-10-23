package telegram

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Private-chat placeholder + editMessageText streaming.
//
// Groups/supergroups keep typing keepalive only (see typing.go) and wait for
// the full reply — NO placeholder, NO edits. Private chats send 「正在思考…」,
// then periodically editMessageText with partial content (~1/sec), and finish
// with one final edit. Parse/HTML errors fall back to plain text via the
// existing editMessageText path in api.go.
//
// WeCom StreamReply is a different protocol (aibot_respond_msg stream frames);
// Telegram must use sendMessage + editMessageText.
// ============================================================================

const (
	// streamPlaceholder is the initial private-chat bubble.
	streamPlaceholder = "正在思考…"

	// streamDraftSuffix marks in-progress text so the user can tell write-in-
	// progress from a finished short reply.
	streamDraftSuffix = "\n\n（还在写…）"

	// streamToolSuffix is used while tools are running (no new text yet).
	streamToolSuffix = "\n\n（工具执行中…）"

	// streamErrorNote replaces a stuck placeholder when the turn ends with no
	// usable content (cancel / silent / error / max age).
	streamErrorNote = "（生成中断）"

	// streamInterruptNote finishes a superseded private stream.
	streamInterruptNote = "（这条已被新消息打断）"

	// defaultPlaceholderDelay: fast replies (< delay) skip the placeholder
	// entirely — finishStream returns false and Send posts a normal message.
	defaultPlaceholderDelay = 1500 * time.Millisecond

	// defaultEditInterval throttles partial edits (~1/sec per task brief).
	defaultEditInterval = 1 * time.Second

	// defaultStreamMaxAge hard-stops a stream that never got a final Send
	// (silent / suppressed / error). Must not leave 「正在思考…」 forever.
	defaultStreamMaxAge = 10 * time.Minute

	// streamEditTimeout bounds a single send/edit call.
	streamEditTimeout = 10 * time.Second

	// maxToolLines caps tool-activity lines in the bubble.
	maxToolLines = 20
)

// Overridable in tests (same pattern as typingRefresh / wecom deltaLivenessInterval).
var (
	streamPlaceholderDelay = defaultPlaceholderDelay
	streamEditInterval     = defaultEditInterval
	streamMaxAgeD          = defaultStreamMaxAge
	streamLivenessInterval = 1 * time.Second
	streamStartGrace       = 5 * time.Second
)

// DeltaKind classifies a streaming increment (mirrors wecom.DeltaKind).
type DeltaKind int

const (
	// DeltaText is a text fragment.
	DeltaText DeltaKind = iota
	// DeltaToolStart begins a tool call (Text = tool name).
	DeltaToolStart
	// DeltaToolDone ends a tool call (Text = tool name, Failed = error).
	DeltaToolDone
)

// Delta is one cleaned increment for the Telegram bubble.
//
// Tool deltas carry only the name — never args/results (external channel).
type Delta struct {
	Kind   DeltaKind
	ID     string
	Text   string
	Failed bool
}

// DeltaSourceFunc subscribes to cleaned text/tool deltas for a trace.
// Injected by api (same shape as wecom); nil channel = unavailable.
type DeltaSourceFunc func(traceID string) (<-chan Delta, func())

// MessageLiveness reports whether a trace is still running on the orchestrator.
// Injected by api (BotService.IsMessageActive). Used as the feedStream watchdog
// so a silent turn does not leave a stuck placeholder.
type MessageLiveness interface {
	IsMessageActive(botID, traceID string) bool
}

// SetDeltaSource injects the delta source (api layer, before Start).
func (c *TelegramChannel) SetDeltaSource(fn DeltaSourceFunc) { c.deltaSrc = fn }

// SetMessageLiveness injects orchestrator liveness (api layer, before Start).
func (c *TelegramChannel) SetMessageLiveness(l MessageLiveness) { c.liveness = l }

// streamEnabled reports whether private edit streaming is on.
func (c *TelegramChannel) streamEnabled() bool { return c.cfg.StreamReply }

// tgStream is one in-flight private-chat stream message.
type tgStream struct {
	chatID    int64
	messageID int64 // 0 until placeholder / first content is posted
	traceID   string
	replyTo   int64
	started   time.Time
	gen       uint64
	cancel    context.CancelFunc

	timer *time.Timer // placeholder delay

	mu      sync.Mutex
	content string // last stored body (no draft suffix)
	fed     bool   // true once real content was shown
	pushed  bool   // true once any Telegram message exists
	closed  bool
}

// openStream registers a potential private-chat stream for chatID.
//
// Does NOT send yet: the placeholder is delayed so fast replies never flash
// 「正在思考…」. Groups/supergroups are a no-op (typing keepalive only).
func (c *TelegramChannel) openStream(ctx context.Context, chatID int64, chatType, traceID string, replyTo int64) {
	if !c.streamEnabled() || !isPrivateChat(chatType) || chatID == 0 || traceID == "" {
		return
	}

	parent := c.typingParent() // runCtx; must not hold streamMu (Stop: mu then streamMu)

	c.streamMu.Lock()
	if c.streams == nil {
		c.streams = make(map[int64]*tgStream)
	}
	if old, ok := c.streams[chatID]; ok && old != nil {
		delete(c.streams, chatID)
		c.streamMu.Unlock()
		c.closeStream(ctx, old, supersedeTGText(old))
		c.streamMu.Lock()
	}

	stCtx, cancel := context.WithCancel(parent)
	c.streamGen++
	gen := c.streamGen
	st := &tgStream{
		chatID:  chatID,
		traceID: traceID,
		replyTo: replyTo,
		started: time.Now(),
		gen:     gen,
		cancel:  cancel,
	}
	c.streams[chatID] = st
	c.streamMu.Unlock()

	st.timer = time.AfterFunc(streamPlaceholderDelay, func() { c.pushPlaceholder(chatID, st) })

	if c.deltaSrc != nil {
		c.wg.Add(1)
		go c.feedStream(stCtx, chatID, st, traceID)
	}
}

// supersedeTGText chooses finish text when a newer private inbound replaces an
// older stream. Preserve fed content; otherwise leave an interrupt note.
func supersedeTGText(old *tgStream) string {
	if old == nil {
		return streamInterruptNote
	}
	old.mu.Lock()
	defer old.mu.Unlock()
	if old.fed && old.content != "" {
		return old.content + "\n\n" + streamInterruptNote
	}
	return streamInterruptNote
}

// pushPlaceholder posts 「正在思考…」 once the delay elapses (private only).
func (c *TelegramChannel) pushPlaceholder(chatID int64, st *tgStream) {
	c.streamMu.Lock()
	cur, ok := c.streams[chatID]
	if !ok || cur != st {
		c.streamMu.Unlock()
		return
	}
	st.mu.Lock()
	if st.closed || st.fed || st.pushed {
		st.mu.Unlock()
		c.streamMu.Unlock()
		return
	}
	st.mu.Unlock()
	c.streamMu.Unlock()

	ctx, cancel := context.WithTimeout(c.typingParent(), streamEditTimeout)
	defer cancel()
	msgID, err := c.doStreamSend(ctx, chatID, streamPlaceholder, st.replyTo)
	if err != nil {
		traceid.L(ctx).Warnw("telegram: stream placeholder failed, falling back to plain send",
			"channel", c.name, "chat_id", chatID, "err", err)
		c.dropStream(chatID, st)
		return
	}
	st.mu.Lock()
	st.messageID = msgID
	st.pushed = true
	st.content = streamPlaceholder
	st.mu.Unlock()
	traceid.L(ctx).Debugw("telegram: stream placeholder sent",
		"channel", c.name, "chat_id", chatID, "message_id", msgID)
}

// finishStream replaces the placeholder/partial with final text.
//
// Returns true when the reply was delivered via edit (caller must not send a
// second message). false → caller falls back to a normal sendMessage.
func (c *TelegramChannel) finishStream(ctx context.Context, chatID int64, text string) bool {
	c.streamMu.Lock()
	st, ok := c.streams[chatID]
	if ok {
		delete(c.streams, chatID)
	}
	c.streamMu.Unlock()
	if !ok || st == nil {
		return false
	}
	return c.closeStream(ctx, st, text)
}

// closeStream edits (or sends) the final bubble and marks the stream closed.
func (c *TelegramChannel) closeStream(ctx context.Context, st *tgStream, text string) bool {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return false
	}
	st.closed = true
	pushed := st.pushed
	msgID := st.messageID
	chatID := st.chatID
	replyTo := st.replyTo
	if text == "" {
		text = st.content
	}
	st.mu.Unlock()

	if st.cancel != nil {
		st.cancel()
	}
	if st.timer != nil {
		st.timer.Stop()
	}

	if text == "" {
		text = streamErrorNote
	}

	// Never posted → caller should sendMessage normally (fast-reply path).
	if !pushed || msgID == 0 {
		return false
	}

	chunks := splitMessage(text, telegramMaxMessageLength)
	first := chunks[0]
	editCtx, cancel := context.WithTimeout(ctx, streamEditTimeout)
	defer cancel()
	if err := c.doStreamEdit(editCtx, chatID, msgID, first); err != nil {
		traceid.L(ctx).Warnw("telegram: stream finish edit failed, deleting placeholder and falling back",
			"channel", c.name, "chat_id", chatID, "message_id", msgID, "err", err)
		_ = c.doStreamDelete(editCtx, chatID, msgID)
		return false
	}
	// Overflow: edit carried the first chunk; send the rest as new messages.
	for i := 1; i < len(chunks); i++ {
		if _, err := c.doStreamSend(editCtx, chatID, chunks[i], 0); err != nil {
			traceid.L(ctx).Warnw("telegram: stream finish overflow send failed",
				"channel", c.name, "chat_id", chatID, "chunk", i, "err", err)
			// First chunk already delivered — treat as success so we don't
			// double-post the whole reply via ReplyWithMode.
			break
		}
	}
	_ = replyTo // reserved: placeholder already replied when sent
	traceid.L(ctx).Infow("telegram: stream finished",
		"channel", c.name, "chat_id", chatID, "message_id", msgID, "bytes", len(text))
	return true
}

// dropStream removes a stream after placeholder send failure (no bubble left).
func (c *TelegramChannel) dropStream(chatID int64, st *tgStream) {
	c.streamMu.Lock()
	if cur, ok := c.streams[chatID]; ok && cur == st {
		delete(c.streams, chatID)
	}
	c.streamMu.Unlock()
	st.mu.Lock()
	st.closed = true
	st.mu.Unlock()
	if st.cancel != nil {
		st.cancel()
	}
	if st.timer != nil {
		st.timer.Stop()
	}
}

// abortAllStreams finishes every open stream (Stop / shutdown).
func (c *TelegramChannel) abortAllStreams(ctx context.Context) {
	c.streamMu.Lock()
	all := make([]*tgStream, 0, len(c.streams))
	for id, st := range c.streams {
		if st != nil {
			all = append(all, st)
		}
		delete(c.streams, id)
	}
	c.streamMu.Unlock()
	for _, st := range all {
		st.mu.Lock()
		text := st.content
		st.mu.Unlock()
		if text == "" || text == streamPlaceholder {
			text = streamErrorNote
		}
		_ = c.closeStream(ctx, st, text)
	}
}

// feedStream subscribes to deltas and edits the private bubble (~1/sec).
//
// Must be started before Ingress.Receive so early deltas are not missed.
// Exits cleanly on cancel / channel close / orchestrator idle / max age.
func (c *TelegramChannel) feedStream(ctx context.Context, chatID int64, mine *tgStream, traceID string) {
	defer c.wg.Done()
	if c.deltaSrc == nil || mine == nil || traceID == "" {
		return
	}
	ch, stop := c.deltaSrc(traceID)
	if ch == nil {
		return
	}
	defer stop()

	start := time.Now()
	var buf strings.Builder
	var activity []string
	toolLine := map[string]int{}
	running := 0
	lastEdit := time.Time{}
	deltas, edits := 0, 0
	detached := false

	defer func() {
		traceid.L(ctx).Infow("telegram: stream delta feed ended",
			"channel", c.name, "chat_id", chatID, "trace_id", traceID,
			"deltas", deltas, "edits", edits, "bytes", buf.Len(),
			"tools", len(activity), "elapsed", time.Since(start).String())
	}()

	compose := func(text string) string {
		if len(activity) == 0 {
			return text
		}
		var b strings.Builder
		for i, line := range activity {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("> ")
			b.WriteString(line)
		}
		if text != "" {
			b.WriteString("\n\n")
			b.WriteString(text)
		}
		return b.String()
	}

	flush := func(final bool) {
		text := buf.String()
		if text == "" && len(activity) == 0 {
			return
		}
		store := compose(text)
		display := store
		if !final {
			if running > 0 {
				display += streamToolSuffix
			} else {
				display += streamDraftSuffix
			}
		}
		if !c.pushStreamContent(ctx, chatID, mine, store, display) {
			detached = true
			return
		}
		lastEdit = time.Now()
		edits++
	}

	watch := time.NewTicker(streamLivenessInterval)
	defer watch.Stop()
	deadline := start.Add(streamMaxAgeD)
	sawActive := false
	gap := streamEditInterval

	for {
		select {
		case <-ctx.Done():
			return
		case <-watch.C:
			if time.Now().After(deadline) {
				flush(true)
				// Max age with no Send: finish the bubble so it is not stuck.
				c.streamMu.Lock()
				stillMine := c.streams[chatID] == mine
				if stillMine {
					delete(c.streams, chatID)
				}
				c.streamMu.Unlock()
				if stillMine {
					final := buf.String()
					if final == "" {
						final = streamErrorNote
					} else {
						final = compose(final)
					}
					_ = c.closeStream(ctx, mine, final)
				}
				return
			}
			if c.turnAlive(traceID) {
				sawActive = true
				continue
			}
			if !sawActive && time.Since(start) < streamStartGrace {
				continue
			}
			flush(true)
			return
		case d, ok := <-ch:
			if !ok {
				flush(true)
				return
			}
			deltas++
			if !sawActive {
				sawActive = c.turnAlive(traceID)
			}
			switch d.Kind {
			case DeltaText:
				buf.WriteString(d.Text)
				if !lastEdit.IsZero() && time.Since(lastEdit) < gap {
					continue
				}
			case DeltaToolStart:
				if d.Text == "" {
					continue
				}
				running++
				if len(activity) >= maxToolLines {
					continue
				}
				toolLine[d.ID] = len(activity)
				activity = append(activity, tgToolLine(d.Text, false, false))
			case DeltaToolDone:
				if d.Text == "" {
					continue
				}
				if running > 0 {
					running--
				}
				if idx, ok := toolLine[d.ID]; ok {
					activity[idx] = tgToolLine(d.Text, true, d.Failed)
					delete(toolLine, d.ID)
				} else if len(activity) < maxToolLines {
					activity = append(activity, tgToolLine(d.Text, true, d.Failed))
				}
			default:
				continue
			}
			flush(false)
			if detached {
				return
			}
		}
	}
}

func tgToolLine(name string, done, failed bool) string {
	switch {
	case !done:
		return "🔧 正在调用 " + name
	case failed:
		return "❌ " + name + " 失败"
	default:
		return "✅ " + name + " 完成"
	}
}

// turnAlive asks the orchestrator whether this trace is still running.
// Unknown (no liveness injected) → true so we rely on max age / channel close.
func (c *TelegramChannel) turnAlive(traceID string) bool {
	if c.liveness == nil || c.botID == "" || traceID == "" {
		return true
	}
	return c.liveness.IsMessageActive(c.botID, traceID)
}

// pushStreamContent ensures a bubble exists, then edits it to display.
//
// store is the clean body (no draft suffix); display is what the user sees.
func (c *TelegramChannel) pushStreamContent(ctx context.Context, chatID int64, st *tgStream, store, display string) bool {
	c.streamMu.Lock()
	if cur, ok := c.streams[chatID]; !ok || cur != st {
		c.streamMu.Unlock()
		return false
	}
	c.streamMu.Unlock()

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return false
	}
	msgID := st.messageID
	replyTo := st.replyTo
	pushed := st.pushed
	st.mu.Unlock()

	display = truncateRunes(display, telegramMaxMessageLength)

	editCtx, cancel := context.WithTimeout(ctx, streamEditTimeout)
	defer cancel()

	if !pushed || msgID == 0 {
		// Content arrived before placeholder: post it as the first bubble and
		// cancel the delayed 「正在思考…」 so it never flashes over real text.
		if st.timer != nil {
			st.timer.Stop()
		}
		id, err := c.doStreamSend(editCtx, chatID, display, replyTo)
		if err != nil {
			traceid.L(ctx).Warnw("telegram: stream first content send failed",
				"channel", c.name, "chat_id", chatID, "err", err)
			return false
		}
		st.mu.Lock()
		st.messageID = id
		st.pushed = true
		st.fed = true
		st.content = store
		st.mu.Unlock()
		return true
	}

	if err := c.doStreamEdit(editCtx, chatID, msgID, display); err != nil {
		traceid.L(ctx).Warnw("telegram: stream edit failed",
			"channel", c.name, "chat_id", chatID, "message_id", msgID, "err", err)
		return false
	}
	st.mu.Lock()
	st.fed = true
	st.content = store
	st.mu.Unlock()
	return true
}

// doStreamSend posts a new text message (placeholder or first content).
func (c *TelegramChannel) doStreamSend(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	if c.streamSend != nil {
		return c.streamSend(ctx, chatID, text, replyTo)
	}
	if c.api == nil {
		return 0, nil
	}
	return c.api.sendMessageFull(ctx, chatID, text, c.cfg.ParseMode, replyTo)
}

// doStreamEdit edits an existing bubble; "message is not modified" is success.
func (c *TelegramChannel) doStreamEdit(ctx context.Context, chatID, messageID int64, text string) error {
	if c.streamEdit != nil {
		return c.streamEdit(ctx, chatID, messageID, text)
	}
	if c.api == nil {
		return nil
	}
	err := c.api.editMessageText(ctx, chatID, messageID, text, c.cfg.ParseMode)
	if isNotModifiedError(err) {
		return nil
	}
	return err
}

// doStreamDelete removes a failed-finish placeholder so Send can post cleanly.
func (c *TelegramChannel) doStreamDelete(ctx context.Context, chatID, messageID int64) error {
	if c.streamDelete != nil {
		return c.streamDelete(ctx, chatID, messageID)
	}
	if c.api == nil {
		return nil
	}
	return c.api.deleteMessage(ctx, chatID, messageID)
}

// isNotModifiedError treats Telegram's "message is not modified" 400 as OK
// (identical consecutive edits during streaming are common).
func isNotModifiedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "message is not modified")
}

// truncateRunes trims s to at most max runes (Telegram limit is UTF-16-ish but
// rune count is a safe upper bound for streaming drafts).
func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max-1 {
			break
		}
		b.WriteRune(r)
		n++
	}
	b.WriteRune('…')
	return b.String()
}

// streamActive reports whether a stream is registered for chatID (tests).
func (c *TelegramChannel) streamActive(chatID int64) bool {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	_, ok := c.streams[chatID]
	return ok
}

// streamMessageID returns the placeholder message id for tests (0 if none).
func (c *TelegramChannel) streamMessageID(chatID int64) int64 {
	c.streamMu.Lock()
	st := c.streams[chatID]
	c.streamMu.Unlock()
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.messageID
}
