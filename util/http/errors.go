package http

import (
	"errors"
	"fmt"
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
