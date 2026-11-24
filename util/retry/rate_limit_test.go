package retry

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock 让冷却/衰减测试不依赖真实墙钟。任意固定基准即可，无业务含义。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRateLimitGate_OnThrottled_GrowsExponentiallyAndClamps(t *testing.T) {
	clk := newFakeClock()
	g := NewRateLimitGate(8*time.Second, 30*time.Second, 0)
	g.now = clk.now

	want := []time.Duration{8, 16, 30, 30}
	for i, w := range want {
		if got := g.OnThrottled(); got != w*time.Second {
			t.Fatalf("throttle #%d = %v, want %v", i+1, got, w*time.Second)
		}
		clk.advance(time.Second) // 仍在衰减窗口（2*max=60s）内
	}
}

func TestRateLimitGate_OnThrottled_DecaysAfterIdle(t *testing.T) {
	clk := newFakeClock()
	g := NewRateLimitGate(8*time.Second, 30*time.Second, 0)
	g.now = clk.now

	if got := g.OnThrottled(); got != 8*time.Second {
		t.Fatalf("first throttle = %v, want 8s", got)
	}
	clk.advance(61 * time.Second) // 超过 2*max 衰减窗口
	if got := g.OnThrottled(); got != 8*time.Second {
		t.Fatalf("throttle after idle = %v, want 8s (decayed)", got)
	}
}

func TestRateLimitGate_CooldownRemainingTracksClock(t *testing.T) {
	clk := newFakeClock()
	g := NewRateLimitGate(8*time.Second, 30*time.Second, 0)
	g.now = clk.now

	g.OnThrottled()
	if got := g.CooldownRemaining(); got != 8*time.Second {
		t.Fatalf("cooldown = %v, want 8s", got)
	}
	clk.advance(3 * time.Second)
	if got := g.CooldownRemaining(); got != 5*time.Second {
		t.Fatalf("cooldown after 3s = %v, want 5s", got)
	}
	clk.advance(10 * time.Second)
	if got := g.CooldownRemaining(); got != 0 {
		t.Fatalf("expired cooldown = %v, want 0", got)
	}
}

func TestRateLimitGate_ConfigureKeepsActiveCooldown(t *testing.T) {
	clk := newFakeClock()
	g := NewRateLimitGate(8*time.Second, 30*time.Second, 0)
	g.now = clk.now

	g.OnThrottled()
	g.Configure(2*time.Second, 10*time.Second, 0)
	if got := g.CooldownRemaining(); got != 8*time.Second {
		t.Fatalf("reconfiguring providers must not clear the active cooldown, got %v", got)
	}
}

func TestRateLimitGate_BeforeRequest_WaitsForGlobalCooldown(t *testing.T) {
	g := NewRateLimitGate(20*time.Millisecond, 80*time.Millisecond, 0)
	g.OnThrottled()

	start := time.Now()
	if err := g.BeforeRequest(context.Background()); err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 12*time.Millisecond {
		t.Fatalf("request started too early: elapsed=%v, want >= ~20ms cooldown", elapsed)
	}
}

func TestRateLimitGate_BeforeRequest_MinIntervalStaggersBursts(t *testing.T) {
	g := NewRateLimitGate(20*time.Millisecond, 80*time.Millisecond, 30*time.Millisecond)
	if err := g.BeforeRequest(context.Background()); err != nil {
		t.Fatalf("first BeforeRequest: %v", err)
	}
	start := time.Now()
	if err := g.BeforeRequest(context.Background()); err != nil {
		t.Fatalf("second BeforeRequest: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("burst not staggered: elapsed=%v, want >= ~30ms", elapsed)
	}
}

func TestRateLimitGate_BeforeRequest_HonorsCanceledContext(t *testing.T) {
	g := NewRateLimitGate(time.Second, 2*time.Second, 0)
	g.OnThrottled()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.BeforeRequest(ctx); err != context.Canceled {
		t.Fatalf("canceled ctx must return immediately, got %v", err)
	}
}

func TestAdaptiveRetryDelay_NonRateLimitedKeepsServerHint(t *testing.T) {
	clk := newFakeClock()
	g := NewRateLimitGate(8*time.Second, 30*time.Second, 0)
	g.now = clk.now

	if got := adaptiveRetryDelay(g, 3*time.Second, false); got != 3*time.Second {
		t.Fatalf("non-throttle delay = %v, want 3s (unchanged)", got)
	}
	if got := g.CooldownRemaining(); got != 0 {
		t.Fatalf("non-throttle error must not open a cooldown, got %v", got)
	}
}

func TestAdaptiveRetryDelay_TakesMaxOfCooldownAndServerHint(t *testing.T) {
	clk := newFakeClock()
	g := NewRateLimitGate(8*time.Second, 30*time.Second, 0)
	g.now = clk.now

	if got := adaptiveRetryDelay(g, 0, true); got != 8*time.Second {
		t.Fatalf("first throttle with no Retry-After = %v, want 8s", got)
	}
	// 服务端明确给了更长的 Retry-After 时必须尊重它，而不是被冷却值覆盖。
	if got := adaptiveRetryDelay(g, 45*time.Second, true); got != 45*time.Second {
		t.Fatalf("server hint longer than cooldown must win, got %v", got)
	}
}

func TestAdaptiveRetryDelay_GlobalSingleton(t *testing.T) {
	ConfigureGlobalRateLimitGate(10*time.Millisecond, 20*time.Millisecond, 0)
	t.Cleanup(func() {
		ConfigureGlobalRateLimitGate(
			DefaultRateLimitCooldown, DefaultRateLimitMaxCooldown, DefaultMinRequestInterval,
		)
	})
	if got := AdaptiveRetryDelay(0, true); got < 10*time.Millisecond {
		t.Fatalf("global gate = %v, want >= 10ms", got)
	}
	if got := AdaptiveRetryDelay(7*time.Millisecond, false); got != 7*time.Millisecond {
		t.Fatalf("global non-throttle = %v, want 7ms", got)
	}
}
