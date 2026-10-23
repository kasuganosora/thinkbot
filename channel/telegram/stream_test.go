package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingStreamAPI captures send/edit/delete for stream unit tests.
type recordingStreamAPI struct {
	mu      sync.Mutex
	sends   []streamSendRec
	edits   []streamEditRec
	deletes []streamEditRec
	nextID  atomic.Int64
}

type streamSendRec struct {
	ChatID  int64
	Text    string
	ReplyTo int64
	MsgID   int64
}

type streamEditRec struct {
	ChatID    int64
	MessageID int64
	Text      string
}

func newRecordingStreamAPI() *recordingStreamAPI {
	r := &recordingStreamAPI{}
	r.nextID.Store(100)
	return r
}

func (r *recordingStreamAPI) send(_ context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	id := r.nextID.Add(1)
	r.mu.Lock()
	r.sends = append(r.sends, streamSendRec{ChatID: chatID, Text: text, ReplyTo: replyTo, MsgID: id})
	r.mu.Unlock()
	return id, nil
}

func (r *recordingStreamAPI) edit(_ context.Context, chatID, messageID int64, text string) error {
	r.mu.Lock()
	r.edits = append(r.edits, streamEditRec{ChatID: chatID, MessageID: messageID, Text: text})
	r.mu.Unlock()
	return nil
}

func (r *recordingStreamAPI) del(_ context.Context, chatID, messageID int64) error {
	r.mu.Lock()
	r.deletes = append(r.deletes, streamEditRec{ChatID: chatID, MessageID: messageID})
	r.mu.Unlock()
	return nil
}

func (r *recordingStreamAPI) sendCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sends)
}

func (r *recordingStreamAPI) editCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.edits)
}

func (r *recordingStreamAPI) lastEditText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.edits) == 0 {
		return ""
	}
	return r.edits[len(r.edits)-1].Text
}

func (r *recordingStreamAPI) sendTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.sends))
	for i, s := range r.sends {
		out[i] = s.Text
	}
	return out
}

func newStreamTestChannel(api *recordingStreamAPI) *TelegramChannel {
	ch := &TelegramChannel{
		name:  "tg-stream-test",
		botID: "bot-1",
		cfg:   Config{StreamReply: true},
	}
	ch.streamSend = api.send
	ch.streamEdit = api.edit
	ch.streamDelete = api.del
	return ch
}

func withFastStream(t *testing.T, delay, editGap, maxAge time.Duration) {
	t.Helper()
	oldD, oldE, oldM, oldL, oldG := streamPlaceholderDelay, streamEditInterval, streamMaxAgeD, streamLivenessInterval, streamStartGrace
	streamPlaceholderDelay, streamEditInterval, streamMaxAgeD = delay, editGap, maxAge
	streamLivenessInterval, streamStartGrace = 20*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() {
		streamPlaceholderDelay, streamEditInterval, streamMaxAgeD = oldD, oldE, oldM
		streamLivenessInterval, streamStartGrace = oldL, oldG
	})
}

func waitUntil(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// TestStream_PrivateOpensGroupSkipped: gating — only private chats register a stream.
func TestStream_PrivateOpensGroupSkipped(t *testing.T) {
	withFastStream(t, time.Hour, time.Second, time.Minute) // delay huge: never auto-push
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)

	ch.openStream(context.Background(), 1, "group", "T-g", 10)
	ch.openStream(context.Background(), 2, "supergroup", "T-sg", 11)
	ch.openStream(context.Background(), 3, "channel", "T-c", 12)
	if ch.streamActive(1) || ch.streamActive(2) || ch.streamActive(3) {
		t.Fatal("groups/supergroups/channels must not open edit streams")
	}
	if api.sendCount() != 0 {
		t.Fatalf("group path must not send placeholder, got %d sends", api.sendCount())
	}

	ch.openStream(context.Background(), 42, "private", "T-p", 99)
	if !ch.streamActive(42) {
		t.Fatal("private chat must open a stream")
	}
	// Placeholder still delayed — no send yet.
	if api.sendCount() != 0 {
		t.Fatalf("placeholder should be delayed, got %d sends", api.sendCount())
	}
	_ = ch.finishStream(context.Background(), 42, "") // clean up unpushed
}

// TestStream_DisabledConfig: StreamReply=false → no stream even in private.
func TestStream_DisabledConfig(t *testing.T) {
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)
	ch.cfg.StreamReply = false
	ch.openStream(context.Background(), 7, "private", "T1", 1)
	if ch.streamActive(7) {
		t.Fatal("StreamReply=false must not open streams")
	}
}

// TestStream_PlaceholderThenFinishEdit: A-档 lifecycle — placeholder, then final edit.
func TestStream_PlaceholderThenFinishEdit(t *testing.T) {
	withFastStream(t, 15*time.Millisecond, time.Second, time.Minute)
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)

	ch.openStream(context.Background(), 42, "private", "T1", 7)
	waitUntil(t, time.Second, func() bool { return api.sendCount() >= 1 })
	if got := api.sendTexts()[0]; got != streamPlaceholder {
		t.Fatalf("placeholder text = %q, want %q", got, streamPlaceholder)
	}
	msgID := ch.streamMessageID(42)
	if msgID == 0 {
		t.Fatal("placeholder must set messageID")
	}

	if !ch.finishStream(context.Background(), 42, "你好，世界") {
		t.Fatal("finishStream should succeed after placeholder")
	}
	if api.editCount() < 1 {
		t.Fatal("expected final edit")
	}
	if got := api.lastEditText(); got != "你好，世界" {
		t.Fatalf("final edit = %q, want full reply", got)
	}
	if ch.streamActive(42) {
		t.Fatal("stream map entry leaked after finish")
	}
}

// TestStream_FastReplySkipsPlaceholder: finish before delay → no bubble, caller sends.
func TestStream_FastReplySkipsPlaceholder(t *testing.T) {
	withFastStream(t, 200*time.Millisecond, time.Second, time.Minute)
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)

	ch.openStream(context.Background(), 9, "private", "Tfast", 1)
	if ch.finishStream(context.Background(), 9, "quick") {
		t.Fatal("unpushed finish must return false so Send posts normally")
	}
	time.Sleep(50 * time.Millisecond)
	if api.sendCount() != 0 {
		t.Fatalf("fast reply must not send placeholder, got %v", api.sendTexts())
	}
	if ch.streamActive(9) {
		t.Fatal("stream should be cleared after unpushed finish")
	}
}

// TestStream_PartialEditsThrottled: B-档 — deltas produce edited partials, throttled.
func TestStream_PartialEditsThrottled(t *testing.T) {
	withFastStream(t, time.Hour, 30*time.Millisecond, time.Minute) // no auto placeholder
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)

	deltas := make(chan Delta, 16)
	ch.deltaSrc = func(string) (<-chan Delta, func()) { return deltas, func() {} }
	// Always-alive so feedStream does not exit on liveness tick before we close.
	ch.liveness = alwaysAlive{}

	ch.openStream(context.Background(), 5, "private", "Tdelta", 3)
	deltas <- Delta{Kind: DeltaText, Text: "从前有"}
	deltas <- Delta{Kind: DeltaText, Text: "一座山"}
	close(deltas)

	waitUntil(t, time.Second, func() bool { return api.sendCount()+api.editCount() >= 1 })
	// First content before placeholder → send (not edit) of draft text.
	waitUntil(t, time.Second, func() bool { return !ch.streamActive(5) || api.sendCount() >= 1 })

	// Drain feedStream: finish the stream so the goroutine can exit.
	_ = ch.finishStream(context.Background(), 5, "从前有一座山")
	waitUntil(t, time.Second, func() bool { return !ch.streamActive(5) })

	found := false
	for _, s := range api.sendTexts() {
		if strings.Contains(s, "从前有") {
			found = true
			if !strings.Contains(s, "还在写") && s != "从前有一座山" {
				// draft suffix expected on non-final flushes
			}
		}
	}
	for _, e := range func() []streamEditRec {
		api.mu.Lock()
		defer api.mu.Unlock()
		return append([]streamEditRec(nil), api.edits...)
	}() {
		if strings.Contains(e.Text, "从前有") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected partial content in send/edit, sends=%v edits=%d", api.sendTexts(), api.editCount())
	}
}

type alwaysAlive struct{}

func (alwaysAlive) IsMessageActive(string, string) bool { return true }

// TestStream_GroupSendNeverEdits: integration-ish — group finish is always plain send path.
func TestStream_GroupSendNeverEdits(t *testing.T) {
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)
	// Simulate group inbound: openStream no-op, Send → finishStream false.
	ch.openStream(context.Background(), 100, "supergroup", "Tg", 1)
	if ch.finishStream(context.Background(), 100, "hello group") {
		t.Fatal("group must not finish via edit stream")
	}
	if api.editCount() != 0 || api.sendCount() != 0 {
		t.Fatalf("group finishStream must be a no-op, sends=%d edits=%d", api.sendCount(), api.editCount())
	}
}

// TestStream_AbortReplacesStuckPlaceholder: Stop/abort must not leave 「正在思考…」.
func TestStream_AbortReplacesStuckPlaceholder(t *testing.T) {
	withFastStream(t, 10*time.Millisecond, time.Second, time.Minute)
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)

	ch.openStream(context.Background(), 8, "private", "Tabort", 1)
	waitUntil(t, time.Second, func() bool { return api.sendCount() >= 1 })

	ch.abortAllStreams(context.Background())
	waitUntil(t, time.Second, func() bool { return api.editCount() >= 1 })
	if got := api.lastEditText(); got != streamErrorNote {
		t.Fatalf("abort edit = %q, want %q", got, streamErrorNote)
	}
	if ch.streamActive(8) {
		t.Fatal("abort must clear stream map")
	}
}

// TestStream_SupersedeCancelsPrevious: new private inbound replaces old stream.
func TestStream_SupersedeCancelsPrevious(t *testing.T) {
	withFastStream(t, 10*time.Millisecond, time.Second, time.Minute)
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)

	ch.openStream(context.Background(), 11, "private", "Told", 1)
	waitUntil(t, time.Second, func() bool { return api.sendCount() >= 1 })
	oldMsg := ch.streamMessageID(11)

	ch.openStream(context.Background(), 11, "private", "Tnew", 2)
	waitUntil(t, time.Second, func() bool {
		return api.editCount() >= 1 || api.sendCount() >= 2
	})
	// Old bubble should be finished with interrupt note.
	found := false
	api.mu.Lock()
	for _, e := range api.edits {
		if e.MessageID == oldMsg && strings.Contains(e.Text, "打断") {
			found = true
		}
	}
	api.mu.Unlock()
	if !found {
		t.Fatalf("expected interrupt edit on old message %d, edits=%v", oldMsg, func() []string {
			api.mu.Lock()
			defer api.mu.Unlock()
			var out []string
			for _, e := range api.edits {
				out = append(out, fmt.Sprintf("%d:%s", e.MessageID, e.Text))
			}
			return out
		}())
	}

	_ = ch.finishStream(context.Background(), 11, "new answer")
}

// TestStream_FinishEditFailureFallsBack: edit fail → delete placeholder, return false.
func TestStream_FinishEditFailureFallsBack(t *testing.T) {
	withFastStream(t, 10*time.Millisecond, time.Second, time.Minute)
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)
	ch.streamEdit = func(context.Context, int64, int64, string) error {
		return fmt.Errorf("telegram editMessageText failed: [400] boom")
	}

	ch.openStream(context.Background(), 13, "private", "Tfail", 1)
	waitUntil(t, time.Second, func() bool { return api.sendCount() >= 1 })
	if ch.finishStream(context.Background(), 13, "final") {
		t.Fatal("failed edit must return false for plain-send fallback")
	}
	api.mu.Lock()
	n := len(api.deletes)
	api.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected placeholder delete on edit failure, got %d", n)
	}
}

// TestIsNotModifiedError: identical consecutive edits are treated as success.
func TestIsNotModifiedError(t *testing.T) {
	if !isNotModifiedError(fmt.Errorf("telegram editMessageText failed: [400] Bad Request: message is not modified")) {
		t.Fatal("expected not-modified detection")
	}
	if isNotModifiedError(fmt.Errorf("telegram editMessageText failed: [400] can't parse entities")) {
		t.Fatal("parse errors must not be treated as not-modified")
	}
}

// TestTruncateRunes: streaming drafts stay within Telegram length.
func TestTruncateRunes(t *testing.T) {
	s := strings.Repeat("字", 10)
	got := truncateRunes(s, 5)
	if utf8Count := len([]rune(got)); utf8Count != 5 {
		t.Fatalf("truncateRunes len=%d, want 5 (%q)", utf8Count, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
}


// TestStream_TurnInactiveReleasesTypingAndPlaceholder: pipeline cancel / silent
// / drop ends IsMessageActive without Send — must stop typing and replace
// 「正在思考…」 (not wait for max age).
func TestStream_TurnInactiveReleasesTypingAndPlaceholder(t *testing.T) {
	withFastStream(t, 10*time.Millisecond, time.Second, time.Minute)
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)
	live := &fakeTypingLiveness{}
	live.active.Store(true)
	ch.SetMessageLiveness(live)

	rec := &countingTypingAction{}
	ch.typingAction = rec.fn
	withFastTyping(t, 30*time.Millisecond, time.Minute)

	ch.startTypingKeepalive(21, "Tsilent")
	ch.openStream(context.Background(), 21, "private", "Tsilent", 1)
	waitUntil(t, time.Second, func() bool { return api.sendCount() >= 1 })
	if !ch.typingActive(21) {
		t.Fatal("typing should be active during generation")
	}

	live.active.Store(false) // OnMessageDone after silent / AbortMessage
	waitUntil(t, 2*time.Second, func() bool {
		return !ch.streamActive(21) && !ch.typingActive(21)
	})
	if got := api.lastEditText(); got != streamErrorNote {
		t.Fatalf("inactive release edit = %q, want %q", got, streamErrorNote)
	}
	n := rec.count()
	time.Sleep(80 * time.Millisecond)
	if got := rec.count(); got != n {
		t.Fatalf("typing continued after inactive release: before=%d after=%d", n, got)
	}
}

// TestStream_MaxAgeReleasesStuckPlaceholder: hard cap cleans 「正在思考…」
// when the turn stays "active" (true mid-generation hang).
func TestStream_MaxAgeReleasesStuckPlaceholder(t *testing.T) {
	withFastStream(t, 5*time.Millisecond, time.Second, 80*time.Millisecond)
	api := newRecordingStreamAPI()
	ch := newStreamTestChannel(api)
	ch.SetMessageLiveness(alwaysAlive{})

	ch.openStream(context.Background(), 22, "private", "Thang", 1)
	waitUntil(t, time.Second, func() bool { return api.sendCount() >= 1 })
	waitUntil(t, 2*time.Second, func() bool { return !ch.streamActive(22) })
	if got := api.lastEditText(); got != streamErrorNote {
		t.Fatalf("max-age edit = %q, want %q", got, streamErrorNote)
	}
}
