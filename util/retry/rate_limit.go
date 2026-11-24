package retry

import (
	"context"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
)

// ============================================================================
// LLM 上游限流协调器（进程级）
//
// 背景：部分上游在高负载时对 429 只返回很短的 HTTP 响应、且不携带
// Retry-After / Retry-After-MS 头。此时 GetRetryDelay 返回 0，重试完全退化成
// 2s 起步的指数退避：多个会话/多个编排步骤会在几乎同一时刻再次打上去，
// 反复撞同一个限流窗口，最终把时间耗在等待重试上。
//
// 两个机制：
//  1. 冷却窗口：任一请求遇到 429 后登记一个进程级「恢复时刻」，后续请求
//     发起前先等到该时刻；短时间内连续 429 按指数拉长，上限可配；闲置一段时间后
//     自动衰减回初始值（无需显式「成功」回调，因为 retry.Do 成功时不会调用
//     GetRetryDelay）。
//  2. 起跑间隔：两次请求发起之间的最小间隔，削掉同一瞬间的并发尖峰。
//     它只约束「起跑」，不持有流式连接的整个生命周期，因此长任务不会饿死其它请求。
//
// 与 Backoff 的关系：Backoff 决定单个请求内部的重试节奏；RateLimitGate 决定
// 跨请求、跨会话的节奏。两者取较大值生效（见 AdaptiveRetryDelay）。
// ============================================================================

const (
	DefaultRateLimitCooldown    = 8 * time.Second
	DefaultRateLimitMaxCooldown = 60 * time.Second
	DefaultMinRequestInterval   = 150 * time.Millisecond
)

// RateLimitGate 是进程级的限流协调器。零值不可用，请用 NewRateLimitGate。
type RateLimitGate struct {
	mu sync.Mutex

	initial     time.Duration
	max         time.Duration
	minInterval time.Duration

	consecutive   int
	lastThrottle  time.Time
	cooldownUntil time.Time
	nextAllowed   time.Time

	// now 仅供测试注入时钟；生产恒为 time.Now。
	now func() time.Time
}

// NewRateLimitGate 构造一个独立的协调器。参数 <= 0 时回落到默认值。
func NewRateLimitGate(initial, max, minInterval time.Duration) *RateLimitGate {
	g := &RateLimitGate{now: time.Now}
	g.configure(initial, max, minInterval)
	return g
}

// Configure 热更新参数（幂等）。不重置已经登记的冷却窗口，避免构建多个
// Provider 时把正在生效的冷却清零。
func (g *RateLimitGate) Configure(initial, max, minInterval time.Duration) {
	g.configure(initial, max, minInterval)
}

func (g *RateLimitGate) configure(initial, max, minInterval time.Duration) {
	if initial <= 0 {
		initial = DefaultRateLimitCooldown
	}
	if max <= 0 {
		max = DefaultRateLimitMaxCooldown
	}
	if max < initial {
		max = initial
	}
	if minInterval < 0 {
		minInterval = 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initial = initial
	g.max = max
	g.minInterval = minInterval
}

// OnThrottled 登记一次限流事件，返回本次建议等待时长。
// 连续限流按 2 倍递增；距上次限流超过衰减窗口则重新从初始值开始。
func (g *RateLimitGate) OnThrottled() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	decay := 2 * g.max
	if decay < time.Minute {
		decay = time.Minute
	}
	if g.consecutive == 0 || now.Sub(g.lastThrottle) > decay {
		g.consecutive = 0
	}
	g.consecutive++
	g.lastThrottle = now

	d := g.initial
	for i := 1; i < g.consecutive && d < g.max; i++ {
		d *= 2
	}
	if d > g.max {
		d = g.max
	}
	if until := now.Add(d); until.After(g.cooldownUntil) {
		g.cooldownUntil = until
	}
	log.Logger.Warnw("rate limit cooldown opened",
		"consecutive", g.consecutive,
		"cooldown_ms", d.Milliseconds(),
		"max_cooldown_ms", g.max.Milliseconds())
	return d
}

// CooldownRemaining 返回当前剩余的全局冷却时长（无冷却时为 0）。
func (g *RateLimitGate) CooldownRemaining() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.cooldownUntil.Sub(g.now())
	if d < 0 {
		return 0
	}
	return d
}

// BeforeRequest 在每次 LLM 请求发起前调用：等待全局冷却，并占用一个起跑间隔。
// ctx 取消时立即返回 ctx.Err()，不额外等待。
func (g *RateLimitGate) BeforeRequest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	now := g.now()
	waitUntil := g.nextAllowed
	if g.cooldownUntil.After(waitUntil) {
		waitUntil = g.cooldownUntil
	}
	if waitUntil.Before(now) {
		waitUntil = now
	}
	cooldownMs := g.cooldownUntil.Sub(now).Milliseconds()
	if cooldownMs < 0 {
		cooldownMs = 0
	}
	// 无论本次等待是否被 ctx 打断，起跑槽位都按序推进，避免惊群。
	g.nextAllowed = waitUntil.Add(g.minInterval)
	g.mu.Unlock()

	wait := waitUntil.Sub(g.now())
	if wait <= 0 {
		return ctx.Err()
	}
	// 正常的起跑错峰（百毫秒级）不打日志，只在冷却真正拖住请求时留痕。
	if wait >= time.Second {
		log.Logger.Infow("llm request deferred by rate limit gate",
			"wait_ms", wait.Milliseconds(),
			"cooldown_remaining_ms", cooldownMs)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// adaptiveRetryDelay 把「服务端建议延迟」与「限流冷却」取较大值。
// rateLimited 由调用方按各自的错误类型判定，避免 retry 包反向依赖 util/http。
func adaptiveRetryDelay(g *RateLimitGate, base time.Duration, rateLimited bool) time.Duration {
	if !rateLimited {
		return base
	}
	if d := g.OnThrottled(); d > base {
		return d
	}
	return base
}

// globalRateLimitGate 是生产使用的进程级单例。所有 Provider 共用，
// 否则每个模型实例各退各的，跨会话的尖峰仍然削不掉。
var globalRateLimitGate = NewRateLimitGate(
	DefaultRateLimitCooldown, DefaultRateLimitMaxCooldown, DefaultMinRequestInterval,
)

// GlobalRateLimitGate 返回进程级单例（供 LLM 请求入口调用 BeforeRequest）。
func GlobalRateLimitGate() *RateLimitGate { return globalRateLimitGate }

// ConfigureGlobalRateLimitGate 用集中配置覆盖单例参数（启动时调用，幂等）。
func ConfigureGlobalRateLimitGate(initial, max, minInterval time.Duration) {
	globalRateLimitGate.Configure(initial, max, minInterval)
}

// AdaptiveRetryDelay 供 GetRetryDelay 包装使用：仅当本次错误确为限流时
// 才登记冷却并拉长等待；其它错误（5xx / 网络错误）保持原有的服务端建议值。
func AdaptiveRetryDelay(base time.Duration, rateLimited bool) time.Duration {
	return adaptiveRetryDelay(globalRateLimitGate, base, rateLimited)
}
