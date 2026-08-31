package retry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// HTTP 智能重试策略
//
// 智能重试策略：
//   - 支持解析 Retry-After / Retry-After-MS 响应头
//   - 区分 rate limit / overloaded / 5xx 不同类型
//   - 指数退避 + 服务端建议延迟取较大值
//
// 使用方式：
//
//	cfg := retry.Config{
//	    MaxRetries: 3,
//	    Backoff: &retry.Backoff{
//	        Strategy: retry.StrategyExponential,
//	        Initial:  2 * time.Second,
//	        Factor:   2.0,
//	        Max:      30 * time.Second,
//	    },
//	    ShouldRetry:   retry.HTTPShouldRetry,
//	    GetRetryDelay: retry.HTTPGetRetryDelay,
//	}
// ============================================================================

// HTTPStatusError 携带 HTTP 状态码的错误。
type HTTPStatusError struct {
	StatusCode int
	Headers    http.Header
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return "http error " + strconv.Itoa(e.StatusCode) + ": " + e.Body
}

// IsRateLimitError 判断是否为限流错误（429）。
func IsRateLimitError(err error) bool {
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 429
	}
	return false
}

// IsOverloadedError 判断是否为服务器过载错误（529 / 503）。
func IsOverloadedError(err error) bool {
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 529 || httpErr.StatusCode == 503
	}
	return false
}

// IsServerError 判断是否为服务器错误（5xx）。
func IsServerError(err error) bool {
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= 500
	}
	return false
}

// HTTPShouldRetry 判断 HTTP 错误是否应该重试。
// 对 429（限流）、503（服务不可用）、529（过载）、500/502/504（服务器错误）重试。
func HTTPShouldRetry(attempt int, err error) bool {
	if err == nil {
		return false
	}

	// context 取消不重试
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) {
		// 非 HTTP 错误（如网络超时）也重试
		return true
	}

	switch httpErr.StatusCode {
	case 429, 503, 529, 500, 502, 504:
		return true
	case 408: // Request Timeout
		return true
	default:
		return false
	}
}

// HTTPGetRetryDelay 从 HTTP 错误响应中提取 Retry-After 延迟。
// 支持：
//   - Retry-After-MS（毫秒，优先）
//   - Retry-After（秒数或 HTTP-date 格式）
func HTTPGetRetryDelay(err error) time.Duration {
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) {
		return 0
	}

	headers := httpErr.Headers
	if headers == nil {
		return 0
	}

	// Retry-After-MS（毫秒）
	if msStr := headers.Get("Retry-After-MS"); msStr != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(msStr), 64); err == nil {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}

	// Retry-After（秒数或 HTTP-date）
	if raStr := headers.Get("Retry-After"); raStr != "" {
		raStr = strings.TrimSpace(raStr)

		// 尝试解析为秒数
		if secs, err := strconv.Atoi(raStr); err == nil {
			return time.Duration(secs) * time.Second
		}

		// 尝试解析为 HTTP-date 格式
		if t, err := http.ParseTime(raStr); err == nil {
			delay := time.Until(t)
			if delay > 0 {
				return delay
			}
		}
	}

	return 0
}

// LLMRetryConfig 返回适合 LLM API 调用的重试配置。
// 使用指数退避 + Retry-After 头解析。
func LLMRetryConfig(maxRetries int) Config {
	return Config{
		MaxRetries: maxRetries,
		Backoff: &Backoff{
			Strategy: StrategyExponential,
			Initial:  2 * time.Second,
			Factor:   2.0,
			Max:      30 * time.Second,
			Jitter:   true,
		},
		ShouldRetry:   HTTPShouldRetry,
		GetRetryDelay: HTTPGetRetryDelay,
	}
}

// DefaultLLMRetryConfig 返回默认 LLM 重试配置（3 次重试）。
func DefaultLLMRetryConfig() Config {
	return LLMRetryConfig(3)
}

// AggressiveRetryConfig 返回更激进的重试配置（5 次重试）。
func AggressiveRetryConfig() Config {
	cfg := LLMRetryConfig(5)
	cfg.Backoff.Max = 60 * time.Second
	return cfg
}

// StreamingRetryConfig 返回适合流式连接的重试配置。
// 流式连接的重试只在未收到任何数据时有意义。
func StreamingRetryConfig(maxRetries int) Config {
	return Config{
		MaxRetries: maxRetries,
		Backoff: &Backoff{
			Strategy: StrategyExponential,
			Initial:  1 * time.Second,
			Factor:   2.0,
			Max:      20 * time.Second,
			Jitter:   true,
		},
		ShouldRetry:   HTTPShouldRetry,
		GetRetryDelay: HTTPGetRetryDelay,
	}
}

// ============================================================================
// 便捷方法：带重试的 HTTP 请求
// ============================================================================

// DoHTTPRequest 执行 HTTP 请求并自动重试。
//
// 使用 HTTPShouldRetry + HTTPGetRetryDelay 策略。
func DoHTTPRequest(ctx context.Context, client *http.Client, req *http.Request, cfg Config) (*http.Response, error) {
	var lastResp *http.Response

	result := Do(ctx, "http_request", cfg, func(ctx context.Context) error {
		// 克隆请求（重试时需要新请求）
		retryReq := req.Clone(ctx)

		resp, err := client.Do(retryReq)
		if err != nil {
			return err
		}

		// 检查是否需要重试
		if resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 408 {
			// 读取 body 用于错误信息
			lastResp = resp
			return &HTTPStatusError{
				StatusCode: resp.StatusCode,
				Headers:    resp.Header,
			}
		}

		lastResp = resp
		return nil
	})

	if result.Err != nil && lastResp != nil {
		return lastResp, result.Err
	}
	if lastResp == nil {
		return nil, result.Err
	}
	return lastResp, result.Err
}
