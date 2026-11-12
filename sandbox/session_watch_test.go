package sandbox

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// watchTestCore builds the same coreTestSpawner registry used by the P2 tests,
// so wake-ups come from real consume()/markFinished() calls rather than a
// mocked notify channel.
func watchTestRegistry(t *testing.T, cfg SessionConfig) (*SessionRegistry, *sessionCore) {
	t.Helper()
	c := newTestCore(80, 24)
	h := &coreTestHandle{c}
	reg := NewSessionRegistry(cfg, zap.NewNop().Sugar(), &coreTestSpawner{h})
	if _, err := reg.Spawn(context.Background(), "bot", sessionAgent, SpawnRequest{Command: "cmd"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return reg, c
}

func TestWatchWait_ImmediateAttentionDoesNotBlock(t *testing.T) {
	cfg := DefaultSessionConfig()
	reg, c := watchTestRegistry(t, cfg)
	// Force a P0 verdict via health state instead of waiting for a real window.
	c.consume([]byte("Error: permission required. Approve? [y/N]\n"))
	c.mu.Lock()
	c.exitCode = nil
	c.mu.Unlock()
	c.markFinished(1, SessionReasonExit)

	start := time.Now()
	res := reg.WaitForAttention(context.Background(), "bot", sessionAgent, 8, false, 5*time.Second, nil)
	if res.WakeReason != "attention" {
		t.Errorf("wakeReason = %q, want attention", res.WakeReason)
	}
	if time.Since(start) > time.Second {
		t.Error("existing attention must return immediately, not wait")
	}
}

func TestWatchWait_WakesOnOutputEventBeforeTimeout(t *testing.T) {
	cfg := DefaultSessionConfig()
	reg, c := watchTestRegistry(t, cfg)
	// At baseline the session is quiet (P2), so the long poll must block.
	go func() {
		time.Sleep(220 * time.Millisecond)
		c.markFinished(0, SessionReasonExit) // real state change -> fireChange
	}()
	start := time.Now()
	res := reg.WaitForAttention(context.Background(), "bot", sessionAgent, 8, false, 6*time.Second, nil)
	elapsed := time.Since(start)
	if res.WakeReason == "timeout" {
		t.Fatal("event wake-up did not fire")
	}
	if elapsed > 3*time.Second {
		t.Errorf("woke too late (%v); long poll ignored the event", elapsed)
	}
	if res.WakeReason != "attention" && res.WakeReason != "change" {
		t.Errorf("unexpected wakeReason %q", res.WakeReason)
	}
}

func TestWatchWait_TimeoutReturnsReadOnlySnapshot(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.WatchWaitMax = 400 * time.Millisecond
	reg, c := watchTestRegistry(t, cfg)
	c.consume([]byte("working...\n"))

	before := reg.Watch("bot", sessionAgent, 8, true)
	start := time.Now()
	res := reg.WaitForAttention(context.Background(), "bot", sessionAgent, 8, true, 5*time.Second, nil)
	if res.WakeReason != "timeout" {
		t.Errorf("wakeReason = %q, want timeout", res.WakeReason)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("wait was not capped by WatchWaitMax")
	}
	if res.Total != before.Total {
		t.Errorf("total changed: %d -> %d", before.Total, res.Total)
	}
	// Zero side effects: no cursor advance, no Touch, no delta consumption.
	after := reg.Watch("bot", sessionAgent, 8, true)
	if attentionSignature(before) != attentionSignature(after) {
		t.Error("long poll mutated the attention snapshot")
	}
}

func TestWatchWait_ContextCancelReturnsPromptly(t *testing.T) {
	cfg := DefaultSessionConfig()
	reg, _ := watchTestRegistry(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res := reg.WaitForAttention(ctx, "bot", sessionAgent, 8, false, 10*time.Second, nil)
	if res.WakeReason != "cancelled" {
		t.Errorf("wakeReason = %q, want cancelled", res.WakeReason)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("cancellation did not interrupt the long poll")
	}
}

func TestWatchWait_DisabledIsInstant(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.WatchWaitMax = -1 // explicitly disabled
	reg, _ := watchTestRegistry(t, cfg)
	start := time.Now()
	res := reg.WaitForAttention(context.Background(), "bot", sessionAgent, 8, false, 5*time.Second, nil)
	if res.WakeReason != "nowait" || time.Since(start) > time.Second {
		t.Errorf("disabled wait = %q after %v", res.WakeReason, time.Since(start))
	}
}

func TestWatchWait_ZeroConfigDoesNotClampToZero(t *testing.T) {
	// NewSessionRegistry called with a zero-valued SessionConfig (common in
	// tests / direct construction) must still honour a real wait.
	c := newTestCore(80, 24)
	// Keep the other tunables valid (MaxTotal=0 would reject Spawn), but leave
	// WatchWaitMax at its zero value — that is exactly the fallback under test.
	cfg := DefaultSessionConfig()
	cfg.WatchWaitMax = 0
	reg := NewSessionRegistry(cfg, zap.NewNop().Sugar(),
		&coreTestSpawner{&coreTestHandle{c}})
	_, err := reg.Spawn(context.Background(), "bot", sessionAgent, SpawnRequest{Command: "cmd"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	start := time.Now()
	res := reg.WaitForAttention(context.Background(), "bot", sessionAgent, 8, false, 300*time.Millisecond, nil)
	if time.Since(start) < 200*time.Millisecond {
		t.Errorf("zero config returned immediately (%v): %+v", time.Since(start), res)
	}
}

func TestWatchWait_RepaintOnlyFakeLiveIsEscalated(t *testing.T) {
	// A repaint-only session (CSI clears, no visible text) is the "fake live"
	// sample from the health confusion matrix (§3.5): health verdict turns into
	// a P0/P1 item and MUST wake the agent. This asserts the event path works
	// for a real-ish stream, not just a synthetic markFinished call.
	cfg := DefaultSessionConfig()
	cfg.WatchWaitMax = 3 * time.Second
	cfg.HealthWindow = 150 * time.Millisecond
	reg, c := watchTestRegistry(t, cfg)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				c.consume([]byte("\x1b[2J\x1b[H"))
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	defer close(stop)
	res := reg.WaitForAttention(context.Background(), "bot", sessionAgent, 8, false, 2*time.Second, nil)
	if res.WakeReason == "timeout" {
		t.Error("fake-live stream was not escalated to attention")
	}
}

func TestWatchWait_QuietProgressDoesNotSpuriouslyWake(t *testing.T) {
	// Steady, genuinely progressing output must NOT wake the agent on every
	// byte: changeHook fires per chunk, but only decision-level changes count.
	cfg := DefaultSessionConfig()
	cfg.WatchWaitMax = 450 * time.Millisecond
	reg, c := watchTestRegistry(t, cfg)
	// Pre-fill a normal screen so the baseline verdict is already settled.
	c.consume([]byte("build: compiling module A\n"))
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				c.consume([]byte("progress line\n"))
				time.Sleep(3 * time.Millisecond)
			}
		}
	}()
	defer close(stop)
	res := reg.WaitForAttention(context.Background(), "bot", sessionAgent, 8, false, 350*time.Millisecond, nil)
	// Either it blocks to timeout, or (if health escalates) it reports a
	// decision-level reason. What must NEVER happen is a "change" wake caused
	// purely by raw seq advancing.
	if res.WakeReason == "change" {
		t.Errorf("raw output churn produced a spurious change wake: %+v", res.Attention)
	}
}

func TestAttentionSignature_IgnoresRawSeq(t *testing.T) {
	a := WatchResult{Total: 1, Quiet: 1, Attention: []WatchItem{{SessionID: "s", Priority: 2, Verdict: HealthProgressing, Seq: 10}}}
	b := WatchResult{Total: 1, Quiet: 1, Attention: []WatchItem{{SessionID: "s", Priority: 2, Verdict: HealthProgressing, Seq: 9999}}}
	if attentionSignature(a) != attentionSignature(b) {
		t.Error("raw seq must not be part of the wake signature")
	}
	b.Attention[0].Priority = 0
	if attentionSignature(a) == attentionSignature(b) {
		t.Error("priority change must alter the signature")
	}
}
