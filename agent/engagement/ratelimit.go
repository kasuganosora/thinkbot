package engagement

import (
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// TokenBucket — 令牌桶限流器
// ============================================================================

// TokenBucket 实现令牌桶限流，用于控制主动参与的频率。
//
// 参数：
//   - capacity: 桶容量（最大突发量）
//   - refillInterval: 每多久补充一个令牌
//
// 用法：
//
//	bucket := NewTokenBucket(3, 1*time.Hour)  // 每小时最多 3 次主动参与
//	if bucket.TryTake() {
//	    // 可以参与
//	}
type TokenBucket struct {
	mu             sync.Mutex
	tokens         float64
	capacity       float64
	refillInterval time.Duration
	lastRefill     time.Time
}

// NewTokenBucket 创建令牌桶。
func NewTokenBucket(capacity int, refillInterval time.Duration) *TokenBucket {
	return &TokenBucket{
		tokens:         float64(capacity),
		capacity:       float64(capacity),
		refillInterval: refillInterval,
		lastRefill:     time.Now(),
	}
}

// TryTake 尝试取一个令牌。成功返回 true，桶空返回 false。
func (b *TokenBucket) TryTake() bool {
	return b.TryTakeN(1)
}

// TryTakeN 尝试取 n 个令牌。成功返回 true，不足返回 false。
func (b *TokenBucket) TryTakeN(n float64) bool {
	if n <= 0 {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill()

	if b.tokens >= n {
		b.tokens -= n
		return true
	}
	return false
}

// Available 返回当前可用令牌数。
func (b *TokenBucket) Available() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	return b.tokens
}

// refill 补充令牌（调用者需持有锁）。
func (b *TokenBucket) refill() {
	if b.refillInterval <= 0 {
		return
	}
	now := time.Now()
	elapsed := now.Sub(b.lastRefill)
	refill := elapsed.Seconds() / b.refillInterval.Seconds()
	if refill > 0 {
		b.tokens = min(b.capacity, b.tokens+refill)
		b.lastRefill = now
	}
}

// Go 1.21+ 内置 min 支持所有可比较有序类型，无需手动定义。

// ============================================================================
// SlidingWindow — 滑动窗口限流器
// ============================================================================

// SlidingWindow 实现滑动窗口限流。
// 在 window 时间窗口内最多允许 limit 次操作。
type SlidingWindow struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	events  []time.Time
}

// NewSlidingWindow 创建滑动窗口限流器。
func NewSlidingWindow(limit int, window time.Duration) *SlidingWindow {
	return &SlidingWindow{
		limit:  limit,
		window: window,
	}
}

// Allow 检查是否允许操作。允许时记录时间戳并返回 true。
func (w *SlidingWindow) Allow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-w.window)

	// 移除过期的时间戳
	idx := 0
	for ; idx < len(w.events); idx++ {
		if w.events[idx].After(cutoff) {
			break
		}
	}
	w.events = w.events[idx:]

	if len(w.events) >= w.limit {
		return false
	}

	w.events = append(w.events, now)
	return true
}

// Count 返回当前窗口内的事件数。
func (w *SlidingWindow) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-w.window)

	count := 0
	for _, t := range w.events {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

// ============================================================================
// RateLimitRule — 将限流器适配为 Rule
// ============================================================================

// RateLimitRule 将限流器包装为 Tier 1 规则。
// 基于 msg.Source 或全局进行限流。
type RateLimitRule struct {
	bucket *TokenBucket
}

// NewRateLimitRule 创建频率限制规则。
func NewRateLimitRule(bucket *TokenBucket) *RateLimitRule {
	return &RateLimitRule{bucket: bucket}
}

// Allow 实现 Rule。
func (r *RateLimitRule) Allow(_ *core.Message) (bool, string) {
	// 注意：这里只是"检查"是否允许，不真正消耗令牌。
	// 令牌在 EngagementStage 确认参与后才消耗（参见 Stage 的 consumeToken）。
	// 这样被后续 Tier 拒绝的消息不会浪费令牌配额。
	if r.bucket == nil {
		return true, ""
	}
	if r.bucket.Available() < 1 {
		return false, "rate limit exceeded"
	}
	return true, ""
}

// Consume 消耗一个令牌。应在确定参与后调用。
func (r *RateLimitRule) Consume() {
	if r.bucket != nil {
		r.bucket.TryTake()
	}
}
