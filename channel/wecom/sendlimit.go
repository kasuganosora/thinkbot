package wecom

// 出站的两条硬约束：限流（30 条/分钟/会话）与 markdown 长度（20480 **字节**）。
//
// 这两条都是实测踩出来的，不是从文档抄的：
//   - 20480 来自 errcode=40058 `body.markdown.content exceed max length 20480`
//   - 30 条/分钟来自官方文档，超了会不会报错没实测（会话级/机器人级粒度也未定）

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// wecomMarkdownLimit 是 markdown 正文的字节上限。
	//
	// ⚠️ 单位是**字节**：一个汉字 3 字节，所以中文正文实际只有约 6800 字。
	// 用字符数截断会在中文场景下**静默超限**（短文本一切正常，长文本才 40058），
	// 人工自测极容易漏 —— 必须按字节算。
	wecomMarkdownLimit = 20480

	// wecomSendRate 每会话的令牌补充速率（个/秒）= 30 条/分钟。
	wecomSendRate = 30.0 / 60.0
	// wecomSendBurst 令牌桶容量。取 5 而不是 30：允许短突发，但不至于一口气
	// 倾泻 30 条把配额打空后长时间阻塞。
	wecomSendBurst = 5

	// bucketIdleTTL 空闲桶的保留时长，超过则在清理时摘除，避免 map 无限增长。
	bucketIdleTTL = 30 * time.Minute
	// bucketMax 桶数量上限（防御：会话 id 若被恶意构造出海量取值）。
	// 超限时整体清空重建 —— 退化成「所有会话重新起桶」，比无界增长安全。
	bucketMax = 4096
)

// markdownEllipsis 是截断后缀。3 个 ASCII 字节，计入字节预算。
const markdownEllipsis = "..."

// truncateForMarkdown 按字节截断正文，并保证不切断 UTF-8 rune。
func truncateForMarkdown(s string) string {
	if len(s) <= wecomMarkdownLimit {
		return s
	}
	limit := wecomMarkdownLimit - len(markdownEllipsis)
	if limit <= 0 {
		return ""
	}
	b := []byte(s[:limit])
	// 回退到最近的合法 rune 边界。
	//
	// ⚠️ 只判断「末字节是不是 lead byte」是不够的：切在 lead byte **之后**
	// （比如正好切下一个 3 字节汉字的第 1 个字节）时末字节仍是合法 lead byte，
	// 但它后面的续字节被切掉了，拼出来的还是非法 UTF-8。
	// 用 DecodeLastRune 判「最后一个 rune 是否完整」才是正确判据。
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRune(b); r == utf8.RuneError && size == 1 {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return string(b) + markdownEllipsis
}

// bucket 是单个会话的令牌桶。
type bucket struct {
	tokens float64
	last   time.Time
}

// sendLimiter 是 per-chat 的发送限流器。
type sendLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

// newSendLimiter 创建一个限流器。
func newSendLimiter() *sendLimiter {
	return &sendLimiter{buckets: make(map[string]*bucket)}
}

// wait 阻塞到取得一个发送配额（或 ctx 结束）。
//
// key 用会话标识（`wecom:dm:<userid>` / `wecom:chat:<chatid>`），
// 与限流的会话粒度对齐 —— 如果实测是机器人级而非会话级，把 key 换成常量即可。
func (l *sendLimiter) wait(ctx context.Context, key string) error {
	for {
		wait, ok := l.tryAcquire(key)
		if ok {
			return nil
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// tryAcquire 尝试取一个令牌，返回 (需等待时长, 是否取得)。
func (l *sendLimiter) tryAcquire(key string) (time.Duration, bool) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) >= bucketMax && l.buckets[key] == nil {
		// 防御性清空：宁可让别人的桶重新起，也不让 map 无界增长。
		l.buckets = make(map[string]*bucket)
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(wecomSendBurst), last: now}
		l.buckets[key] = b
	}

	b.tokens += now.Sub(b.last).Seconds() * wecomSendRate
	if b.tokens > float64(wecomSendBurst) {
		b.tokens = float64(wecomSendBurst)
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens -= 1
		return 0, true
	}
	need := (1 - b.tokens) / wecomSendRate
	return time.Duration(need * float64(time.Second)), false
}

// prune 摘除长时间空闲的桶。
func (l *sendLimiter) prune() {
	cutoff := time.Now().Add(-bucketIdleTTL)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}
