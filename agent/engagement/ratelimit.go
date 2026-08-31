package engagement

import (
	"sync"
	"sync/atomic"
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

// Refund 退还一个令牌（不超过容量）。
func (b *TokenBucket) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(b.capacity, b.tokens+1)
}

// Go 1.21+ 内置 min 支持所有可比较有序类型，无需手动定义。

// ============================================================================
// SlidingWindow — 滑动窗口限流器
// ============================================================================

// SlidingWindow 实现滑动窗口限流。
// 在 window 时间窗口内最多允许 limit 次操作。
type SlidingWindow struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	events []time.Time
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
	// took 记录未退还的预扣令牌数。
	// 使用 atomic.Int32 计数器解决并发 Allow/Refund 时的竞态问题：
	// 多个 goroutine 可以并发 Allow（各自 +1），各自 Refund 时 -1，互不干扰。
	took atomic.Int32
}

// NewRateLimitRule 创建频率限制规则。
func NewRateLimitRule(bucket *TokenBucket) *RateLimitRule {
	return &RateLimitRule{bucket: bucket}
}

// Allow 实现 Rule。预扣一个令牌，避免 TOCTOU 竞态。
// 如果后续决定不参与，调用 Refund 退还令牌。
func (r *RateLimitRule) Allow(_ *core.Message) (bool, string) {
	if r.bucket == nil {
		return true, ""
	}
	if !r.bucket.TryTake() {
		return false, "rate limit exceeded"
	}
	r.took.Add(1)
	return true, ""
}

// Consume 确认消耗。令牌已在 Allow 中预扣，此方法现在是 no-op。
func (r *RateLimitRule) Consume() {
	// 令牌已在 Allow 中预扣，无需额外操作
}

// Refund 退还预扣的令牌（当消息最终未参与时调用）。
// 使用 CAS 循环确保只退还 Allow() 实际预扣的令牌，避免超额退还。
func (r *RateLimitRule) Refund() {
	if r.bucket == nil {
		return
	}
	for {
		old := r.took.Load()
		if old <= 0 {
			return
		}
		if r.took.CompareAndSwap(old, old-1) {
			r.bucket.Refund()
			return
		}
	}
}
