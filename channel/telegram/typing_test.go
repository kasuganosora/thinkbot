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

	ch.startTypingKeepalive(42)
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

	ch.startTypingKeepalive(7)
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

	ch.startTypingKeepalive(99)
	rec.waitAtLeast(t, 1, time.Second)
	ch.startTypingKeepalive(99) // supersede
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

	ch.startTypingKeepalive(1)
	ch.startTypingKeepalive(2)
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
	ch.startTypingKeepalive(3)
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
	ch.startTypingKeepalive(0)
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

	ch.startTypingKeepalive(55)
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
