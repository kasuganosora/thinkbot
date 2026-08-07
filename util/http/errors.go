package http

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/util/watchdog"
)

// ============================================================================
// 看门狗超时错误
// ============================================================================

// WatchdogTimeoutError 表示流式连接因看门狗超时而被中断。
//
// 与普通的 context.Canceled 不同，此错误明确表示：
//   - TCP 连接仍然存活
//   - 但在 WatchdogTimeout 时间内没有收到任何数据
//   - 这是"数据流卡住"而非"用户主动取消"
//
// 可通过 IsWatchdogTimeout(err) 判断，也可通过 errors.Is(err, watchdog.ErrWatchdogTimeout) 判断。
type WatchdogTimeoutError struct {
	// URL 请求的 URL。
	URL string
	// ItemsReceived 本次连接中收到的事件数（SSE）或数据块数（Stream）。
	ItemsReceived int
	// BytesReceived 本次连接中收到的总字节数。
	BytesReceived int
	// Elapsed 从连接建立到超时的耗时。
	Elapsed time.Duration
	// WatchdogName 看门狗名称。
	WatchdogName string
}

func (e *WatchdogTimeoutError) Error() string {
	return fmt.Sprintf("watchdog timeout after %v on %s: received %d items, %d bytes",
		e.Elapsed, e.URL, e.ItemsReceived, e.BytesReceived)
}

// Unwrap 支持 errors.Is，返回 watchdog.ErrWatchdogTimeout。
func (e *WatchdogTimeoutError) Unwrap() error {
	return watchdog.ErrWatchdogTimeout
}

// IsWatchdogTimeout 判断错误是否为看门狗超时。
//
// 用于区分两种 context 取消场景：
//   - true  → 数据流卡住，可能需要重试
//   - false → 用户主动取消，不应重试
func IsWatchdogTimeout(err error) bool {
	if err == nil {
		return false
	}
	// 快速路径：直接类型断言
	var e *WatchdogTimeoutError
	if errors.As(err, &e) {
		return true
	}
	// 兼容路径：sentinel
	return errors.Is(err, watchdog.ErrWatchdogTimeout)
}

// ============================================================================
// 流式重试策略
// ============================================================================

// DefaultStreamShouldRetry 是流式连接的默认 ShouldRetry 策略，用于 retry.Config。
//
// 策略：
//   - 看门狗超时且本次连接未收到任何数据 → 重试（连接刚建立就卡住了）
//   - 看门狗超时但已收到部分数据 → 不重试（避免数据重复）
//   - 非看门狗超时错误（如用户取消、连接失败） → 不重试
func DefaultStreamShouldRetry(attempt int, err error) bool {
	var wdErr *WatchdogTimeoutError
	if !errors.As(err, &wdErr) {
		return false
	}
	return wdErr.ItemsReceived == 0
}

// StreamShouldRetry 是流式连接的增强 ShouldRetry 策略，在 DefaultStreamShouldRetry
// 基础上额外处理连接建立阶段的 HTTP 错误（如 429 限流、5xx 服务端错误）。
//
// 流式请求的 429/5xx 都发生在连接建立阶段（首字节之前），此时重试整条流是安全的
// （尚未收到任何数据，不会造成重复）。这类错误由 streamConnect 返回 *StreamHTTPError。
//
// 策略：
//   - context 取消 / deadline → 不重试（用户主动取消）
//   - 看门狗超时且未收到数据 → 重试
//   - **配额耗尽（429/403 且 body 表明额度用尽）→ 不重试**（见 IsQuotaExhausted）
//   - StreamHTTPError 且状态码 ∈ {429,500,502,503,504,529,408} → 重试
//   - StreamHTTPError 其它状态码（如 400/401/403）→ 不重试（客户端错误，重试无意义）
//   - 其它错误（如网络中断、连接失败）→ 重试
func StreamShouldRetry(attempt int, err error) bool {
	if err == nil {
		return false
	}
	// 用户主动取消不重试
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 看门狗超时：仅在未收到任何数据时重试
	var wdErr *WatchdogTimeoutError
	if errors.As(err, &wdErr) {
		return wdErr.ItemsReceived == 0
	}
	// 连接建立阶段的 HTTP 错误：按状态码判断
	var httpErr *StreamHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case 429, 500, 502, 503, 504, 529, 408:
			// 429 有两种语义，只看状态码会把「额度用尽」当成「瞬时限流」空转重试。
			// 配额耗尽在窗口重置前 100% 失败，重试只会加剧限流。
			if IsQuotaExhausted(err) {
				return false
			}
			return true
		default:
			return false
		}
	}
	// 其它错误（网络层）默认重试
	return true
}

// StreamGetRetryDelay 从流式连接的 *StreamHTTPError 中解析服务端建议的重试延迟
// （Retry-After / Retry-After-MS 响应头）。用作 retry.Config.GetRetryDelay。
// 无法解析时返回 0，由退避策略决定间隔。
func StreamGetRetryDelay(err error) time.Duration {
	var httpErr *StreamHTTPError
	if !errors.As(err, &httpErr) || httpErr.Headers == nil {
		return 0
	}
	headers := httpErr.Headers
	// Retry-After-MS（毫秒，优先）
	if msStr := strings.TrimSpace(headers.Get("Retry-After-MS")); msStr != "" {
		if ms, e := strconv.ParseFloat(msStr, 64); e == nil {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	// Retry-After（秒数或 HTTP-date）
	if raStr := strings.TrimSpace(headers.Get("Retry-After")); raStr != "" {
		if secs, e := strconv.Atoi(raStr); e == nil {
			return time.Duration(secs) * time.Second
		}
		if t, e := nethttp.ParseTime(raStr); e == nil {
			if delay := time.Until(t); delay > 0 {
				return delay
			}
		}
	}
	return 0
}

// ============================================================================
// 配额耗尽识别 —— 429 的两种语义
// ============================================================================
//
// 背景（真实事故 wf-71f5988de878028d6a3dcb6a，2026-08-07）：
//
//	GLM 上游返回 HTTP 429 + body {"error":{"code":"1308","message":
//	"您在当前时间段的请求已达到 5 小时的使用上限。您的限额将在 2026-08-07 20:06:23 重置。"}}
//	旧 StreamShouldRetry 只看状态码，把它当成瞬时限流重试 5 次；外层节点又重试 3 次
//	→ 单节点撞墙 15 次，8 个失败节点合计 120 次注定失败的请求，反过来加剧限流。
//
// 判定原则：
//
//	瞬时限流（QPS/并发超了）→ 退避重试有效。
//	配额耗尽（窗口/账户额度用尽）→ 重置前重试必然失败，应当立即放弃并上抛，
//	由上层决定熔断、切换供应商或等待重置。
//
// 为避免误伤，只在 **429/403** 且 body 命中「明确表达额度用尽」的特征时才判定为耗尽；
// 单纯的 "rate limit" / "too many requests" 一律按瞬时限流处理（继续重试）。

// quotaExhaustedPatterns 是「额度用尽」的 body 特征（小写匹配）。
//
// 收录标准：该词只可能出现在**持久性**额度问题中。任何可能表达瞬时限流的措辞
// （rate limit / too many requests / concurrency）都**不得**加入，否则会把可恢复的
// 抖动误判为耗尽而放弃重试。
var quotaExhaustedPatterns = []string{
	// 通用 / OpenAI 系
	"insufficient_quota",
	"exceeded your current quota",
	"quota exceeded",
	"quota_exceeded",
	"out of credits",
	"credit balance is too low",
	"billing hard limit",
	"check your plan and billing",
	// 智谱 GLM：1308 = 时间窗口用量上限
	`"code":"1308"`,
	`"code": "1308"`,
	// 中文措辞
	"使用上限",
	"额度已用尽",
	"额度不足",
	"余额不足",
	"配额已用完",
	"超出配额",
}

// IsQuotaExhausted 判断错误是否为「上游额度耗尽」这类**在窗口重置前重试必然失败**的故障。
//
// 仅对 *StreamHTTPError 生效，且要求状态码为 429 或 403。返回 true 时调用方应当
// 立即放弃重试（见 StreamShouldRetry），并考虑熔断整条任务链。
func IsQuotaExhausted(err error) bool {
	var httpErr *StreamHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode != 429 && httpErr.StatusCode != 403 {
		return false
	}
	return matchesQuotaText(string(httpErr.Body))
}

// matchesQuotaText 在文本中匹配「额度用尽」特征（大小写不敏感）。
func matchesQuotaText(s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	for _, p := range quotaExhaustedPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// IsQuotaExhaustedLoose 在 IsQuotaExhausted 基础上增加**纯文本兜底**。
//
// 为什么必须有兜底（2026-08-04 已在 review 基础设施错误分类上吃过同样的亏）：
// 真实错误链是 `节点重试 → SubAgent → LLM 流式重试 → StreamHTTPError`，中间多层
// 用 fmt.Errorf 拼成纯文本（"retry exhausted after 3 attempts: ... HTTP 429 ..."），
// **错误值本身已丢失**，errors.As 必然拿不到 *StreamHTTPError。只靠结构化判定
// 等于这段代码在生产环境永远不生效。
//
// 兜底的误判防线：必须**同时**出现限流状态码（429/403）与额度耗尽措辞才算命中。
// 单有 "配额" 之类的词（可能来自节点任务文本被拼进错误）不足以判定。
func IsQuotaExhaustedLoose(err error) bool {
	if err == nil {
		return false
	}
	if IsQuotaExhausted(err) {
		return true
	}
	s := err.Error()
	if !strings.Contains(s, "429") && !strings.Contains(s, "403") {
		return false
	}
	return matchesQuotaText(s)
}

// quotaResetTimeRe 匹配错误文案里的绝对重置时刻，如
// "您的限额将在 2026-08-07 20:06:23 重置"。
var quotaResetTimeRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})`)

// QuotaResetAt 尽力解析配额恢复时刻。
//
// 优先级：Retry-After 响应头 > body 里的绝对时间戳。两者都拿不到时返回 ok=false，
// 由调用方自行决定兜底等待时长（**不要**在这里编造默认值，否则调用方无法区分
// 「服务端明确告知」和「我猜的」）。
//
// 解析出的时间若已早于当前时刻，同样返回 false —— 过期的重置时间没有调度价值。
func QuotaResetAt(err error) (time.Time, bool) {
	var httpErr *StreamHTTPError
	if !errors.As(err, &httpErr) {
		return time.Time{}, false
	}
	// Retry-After 系响应头（复用流式重试延迟解析）
	if d := StreamGetRetryDelay(err); d > 0 {
		return time.Now().Add(d), true
	}
	// body 里的绝对时间戳（服务端本地时区，按运行时本地时区解析）
	if t, ok := parseQuotaResetText(string(httpErr.Body)); ok {
		return t, true
	}
	return time.Time{}, false
}

// QuotaResetAtLoose 是 QuotaResetAt 的纯文本兜底版本：结构化错误拿不到时，
// 直接从 err.Error() 文本里找绝对重置时刻。理由同 IsQuotaExhaustedLoose。
//
// 同样**不编造默认值**——拿不到就返回 false，由调用方决定兜底等待时长。
func QuotaResetAtLoose(err error) (time.Time, bool) {
	if err == nil {
		return time.Time{}, false
	}
	if t, ok := QuotaResetAt(err); ok {
		return t, true
	}
	return parseQuotaResetText(err.Error())
}

// parseQuotaResetText 从文本中解析未来的绝对重置时刻；已过期的时间视为无效。
func parseQuotaResetText(s string) (time.Time, bool) {
	m := quotaResetTimeRe.FindStringSubmatch(s)
	if len(m) != 3 {
		return time.Time{}, false
	}
	t, e := time.ParseInLocation("2006-01-02 15:04:05", m[1]+" "+m[2], time.Local)
	if e != nil || time.Until(t) <= 0 {
		return time.Time{}, false
	}
	return t, true
}
