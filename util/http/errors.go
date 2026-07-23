package http

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"
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
