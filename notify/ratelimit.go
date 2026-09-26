package notify

import (
	"math"
	"sync"
	"time"
)

// Limiter 是按 key 的令牌桶（进程内）。容量 N，每 Window 补满 N 个。
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	rate   Rate
}

// NewLimiter 创建限流器。
func NewLimiter() *Limiter {
	return &Limiter{buckets: make(map[string]*bucket)}
}

// Allow 尝试消耗一个令牌；失败时返回建议的 Retry-After。
func (l *Limiter) Allow(key string, r Rate, now time.Time) (bool, time.Duration) {
	if r.N <= 0 || r.Window <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || b.rate != r {
		b = &bucket{tokens: float64(r.N), last: now, rate: r}
		l.buckets[key] = b
	}
	perSec := float64(r.N) / r.Window.Seconds()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(float64(r.N), b.tokens+el*perSec)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		l.gcLocked(now)
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / perSec * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// gcLocked 清掉早已补满的桶，防止 key 无限增长。
func (l *Limiter) gcLocked(now time.Time) {
	if len(l.buckets) < 1024 {
		return
	}
	for k, b := range l.buckets {
		if now.Sub(b.last) > b.rate.Window {
			delete(l.buckets, k)
		}
	}
}
