package retry

import (
	"math"
	"math/rand"
	"time"
)

// Strategy 退避策略类型。
type Strategy string

const (
	StrategyFixed      Strategy = "fixed"      // 固定间隔
	StrategyLinear     Strategy = "linear"     // 线性增长：initial * attempt
	StrategyExponential Strategy = "exponential" // 指数增长：initial * factor^attempt
)

// Backoff 退避配置。
type Backoff struct {
	// Strategy 退避策略，默认 fixed。
	Strategy Strategy
	// Initial 初始间隔，默认 200ms。
	Initial time.Duration
	// Max 最大间隔上限（指数/线性退避时生效），默认 30s。
	Max time.Duration
	// Factor 指数退避因子，默认 2.0。
	Factor float64
	// Jitter 是否添加随机抖动（避免惊群），默认 false。
	Jitter bool
}

// DefaultBackoff 返回默认退避配置（固定 200ms）。
func DefaultBackoff() Backoff {
	return Backoff{
		Strategy: StrategyFixed,
		Initial:  200 * time.Millisecond,
		Max:      30 * time.Second,
		Factor:   2.0,
	}
}

// Calc 根据退避配置和当前尝试次数（从 1 开始）计算等待时间。
func (b Backoff) Calc(attempt int) time.Duration {
	initial := b.Initial
	if initial <= 0 {
		initial = 200 * time.Millisecond
	}

	maxWait := b.Max
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}

	factor := b.Factor
	if factor <= 0 {
		factor = 2.0
	}

	strategy := b.Strategy
	if strategy == "" {
		strategy = StrategyFixed
	}

	var wait time.Duration

	switch strategy {
	case StrategyLinear:
		wait = time.Duration(int64(initial) * int64(attempt))

	case StrategyExponential:
		// initial * factor^(attempt-1)
		mult := math.Pow(factor, float64(attempt-1))
		wait = time.Duration(float64(initial) * mult)

	default: // StrategyFixed
		wait = initial
	}

	// 上限
	if wait > maxWait {
		wait = maxWait
	}

	// 抖动：[0.5*wait, 1.5*wait)
	if b.Jitter && wait > 0 {
		jitter := float64(wait) * (0.5 + rand.Float64())
		wait = time.Duration(jitter)
	}

	return wait
}
