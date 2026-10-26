package telegram

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingTypingAction records every typing send for assertions.
type countingTypingAction struct {
	mu    sync.Mutex
	calls []int64 // chatIDs in order
	n     atomic.Int32
}

func (c *countingTypingAction) fn(_ context.Context, chatID int64) error {
	c.n.Add(1)
	c.mu.Lock()
	c.calls = append(c.calls, chatID)
	c.mu.Unlock()
	return nil
}

func (c *countingTypingAction) count() int {
	return int(c.n.Load())
}

func (c *countingTypingAction) waitAtLeast(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.count() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("typing calls = %d, want >= %d within %s", c.count(), want, timeout)
}

func newTypingTestChannel(action *countingTypingAction) *TelegramChannel {
	ch := &TelegramChannel{name: "tg-test"}
	ch.typingAction = action.fn
	return ch
}

func withFastTyping(t *testing.T, refresh, maxAge time.Duration) {
	t.Helper()
	oldR, oldM := typingRefresh, typingMaxAgeD
	typingRefresh, typingMaxAgeD = refresh, maxAge
	t.Cleanup(func() {
		typingRefresh, typingMaxAgeD = oldR, oldM
	})
}

// TestTypingKeepalive_RefreshesUntilStop core lifecycle: immediate send +
// periodic refresh, then clean stop with no further calls.
func TestTypingKeepalive_RefreshesUntilStop(t *testing.T) {
	withFastTyping(t, 20*time.Millisecond, time.Minute)
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)

	ch.startTypingKeepalive(42, "")
	rec.waitAtLeast(t, 1, time.Second) // immediate

	rec.waitAtLeast(t, 3, time.Second) // at least two refreshes
	if !ch.typingActive(42) {
		t.Fatal("keepalive should still be active before stop")
	}

	ch.stopTypingKeepalive(42)
	n := rec.count()
	time.Sleep(80 * time.Millisecond)
	if got := rec.count(); got != n {
		t.Fatalf("typing continued after stop: before=%d after=%d", n, got)
	}
	if ch.typingActive(42) {
		t.Fatal("keepalive map entry leaked after stop")
	}
}

// TestTypingKeepalive_ShortReplySingleAction: stop before first refresh tick
// → exactly one sendChatAction (no API spam on fast replies).
func TestTypingKeepalive_ShortReplySingleAction(t *testing.T) {
	withFastTyping(t, 200*time.Millisecond, time.Minute)
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)

	ch.startTypingKeepalive(7, "")
	rec.waitAtLeast(t, 1, time.Second)
	ch.stopTypingKeepalive(7)

	time.Sleep(50 * time.Millisecond)
	if got := rec.count(); got != 1 {
		t.Fatalf("short reply should send typing once, got %d", got)
	}
}

// TestTypingKeepalive_RestartCancelsPrevious: new inbound for same chat must
// cancel the old loop (no leaked goroutines / double refresh).
func TestTypingKeepalive_RestartCancelsPrevious(t *testing.T) {
	withFastTyping(t, 30*time.Millisecond, time.Minute)
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)

	ch.startTypingKeepalive(99, "")
	rec.waitAtLeast(t, 1, time.Second)
	ch.startTypingKeepalive(99, "") // supersede
	rec.waitAtLeast(t, 2, time.Second)

	ch.stopTypingKeepalive(99)
	// Drain: wait for both loops to exit via wg... channel has no Start, so
	// just give the cancelled goroutines a moment and assert map is clear.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !ch.typingActive(99) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ch.typingActive(99) {
		t.Fatal("typing map still active after stop following restart")
	}

	n := rec.count()
	time.Sleep(100 * time.Millisecond)
	if got := rec.count(); got != n {
		t.Fatalf("refresh continued after final stop: before=%d after=%d", n, got)
	}
}

// TestTypingKeepalive_StopAllAndMaxAge: stopAllTyping cancels every chat;
// max age also ends a session that never got a reply.
func TestTypingKeepalive_StopAllAndMaxAge(t *testing.T) {
	withFastTyping(t, 15*time.Millisecond, 60*time.Millisecond)
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)

	ch.startTypingKeepalive(1, "")
	ch.startTypingKeepalive(2, "")
	rec.waitAtLeast(t, 2, time.Second)

	ch.stopAllTyping()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !ch.typingActive(1) && !ch.typingActive(2) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ch.typingActive(1) || ch.typingActive(2) {
		t.Fatal("stopAllTyping left sessions active")
	}

	// Max age: start one and let it expire without stop.
	ch.startTypingKeepalive(3, "")
	rec.waitAtLeast(t, rec.count(), time.Second) // ensure running
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !ch.typingActive(3) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("typing keepalive did not exit on max age")
}

// TestTypingKeepalive_ZeroChatIDIgnored: defensive no-op.
func TestTypingKeepalive_ZeroChatIDIgnored(t *testing.T) {
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)
	ch.startTypingKeepalive(0, "")
	time.Sleep(30 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatalf("zero chatID should not send typing, got %d", rec.count())
	}
	if ch.typingActive(0) {
		t.Fatal("zero chatID should not register a session")
	}
}

func TestIsPrivateChat(t *testing.T) {
	cases := map[string]bool{
		"private":    true,
		"group":      false,
		"supergroup": false,
		"channel":    false,
		"":           false,
	}
	for ct, want := range cases {
		if got := isPrivateChat(ct); got != want {
			t.Errorf("isPrivateChat(%q) = %v, want %v", ct, got, want)
		}
	}
	idCases := map[int64]bool{
		424242:  true,  // private user id
		-1001:   false, // basic group
		-100123: false, // supergroup / channel
		0:       false,
	}
	for id, want := range idCases {
		if got := isPrivateChatID(id); got != want {
			t.Errorf("isPrivateChatID(%d) = %v, want %v", id, got, want)
		}
	}
}

// TestTypingKeepalive_RunCtxCancel: cancelling channel runCtx ends keepalives
// (mirrors Stop cancelling runCtx before stopAllTyping).
func TestTypingKeepalive_RunCtxCancel(t *testing.T) {
	withFastTyping(t, 20*time.Millisecond, time.Minute)
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)

	ctx, cancel := context.WithCancel(context.Background())
	ch.mu.Lock()
	ch.runCtx = ctx
	ch.mu.Unlock()

	ch.startTypingKeepalive(55, "")
	rec.waitAtLeast(t, 1, time.Second)
	cancel()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !ch.typingActive(55) {
			n := rec.count()
			time.Sleep(60 * time.Millisecond)
			if got := rec.count(); got != n {
				t.Fatalf("typing continued after runCtx cancel: before=%d after=%d", n, got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("typing keepalive did not exit after runCtx cancel")
}


// TestTypingMaxAgeIsThreeMinutes pins the ops-requested hard cap (~2–3 min).
func TestTypingMaxAgeIsThreeMinutes(t *testing.T) {
	if typingMaxAge != 3*time.Minute {
		t.Fatalf("typingMaxAge = %s, want 3m (ops hang release)", typingMaxAge)
	}
	if defaultStreamMaxAge != 3*time.Minute {
		t.Fatalf("defaultStreamMaxAge = %s, want 3m (aligned with typing)", defaultStreamMaxAge)
	}
}

// fakeTypingLiveness toggles orchestrator turn activity for keepalive tests.
type fakeTypingLiveness struct {
	active atomic.Bool
}

func (f *fakeTypingLiveness) IsMessageActive(_, _ string) bool {
	return f.active.Load()
}

// TestTypingKeepalive_TurnInactiveStops: cancel / silent / drop / hard-timeout
// clear IsMessageActive without Send — typing must stop (not wait for max age).
func TestTypingKeepalive_TurnInactiveStops(t *testing.T) {
	withFastTyping(t, 20*time.Millisecond, time.Minute) // max age far away
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)
	live := &fakeTypingLiveness{}
	live.active.Store(true)
	ch.botID = "bot-1"
	ch.SetMessageLiveness(live)

	// Shrink grace so the watchdog can observe inactive quickly.
	oldGrace := streamStartGrace
	streamStartGrace = 5 * time.Millisecond
	t.Cleanup(func() { streamStartGrace = oldGrace })
	oldLive := streamLivenessInterval
	streamLivenessInterval = 10 * time.Millisecond
	t.Cleanup(func() { streamLivenessInterval = oldLive })

	ch.startTypingKeepalive(77, "trace-cancel")
	rec.waitAtLeast(t, 1, time.Second)
	if !ch.typingActive(77) {
		t.Fatal("keepalive should be active while turn is alive")
	}

	live.active.Store(false) // simulates OnMessageDone after AbortMessage / silent
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !ch.typingActive(77) {
			n := rec.count()
			time.Sleep(60 * time.Millisecond)
			if got := rec.count(); got != n {
				t.Fatalf("typing continued after turn inactive: before=%d after=%d", n, got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("typing keepalive did not stop after turn became inactive")
}

// TestTypingKeepalive_InterruptRestarts: new inbound same chat cancels old loop
// (pipeline interrupt path) — covered by RestartCancelsPrevious; assert stop
// still works when a traceID is bound.
func TestTypingKeepalive_InterruptWithTraceStops(t *testing.T) {
	withFastTyping(t, 20*time.Millisecond, time.Minute)
	rec := &countingTypingAction{}
	ch := newTypingTestChannel(rec)
	live := &fakeTypingLiveness{}
	live.active.Store(true)
	ch.botID = "bot-1"
	ch.SetMessageLiveness(live)

	ch.startTypingKeepalive(88, "trace-old")
	rec.waitAtLeast(t, 1, time.Second)
	ch.startTypingKeepalive(88, "trace-new") // interrupt / supersede
	rec.waitAtLeast(t, 2, time.Second)
	ch.stopTypingKeepalive(88)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !ch.typingActive(88) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("typing still active after interrupt + stop")
}
