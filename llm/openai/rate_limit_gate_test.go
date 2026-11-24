package openai

import (
	"net/http"
	"testing"
	"time"

	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// 进程级冷却只应由生产链路（WithRateLimitGate）打开；手构造 Client 的
// 毫秒级重试用例必须保持原有节奏，否则回归测试会被 8s 冷却拖挂。
func TestStreamRetryConfig_RateLimitGateIsOptIn(t *testing.T) {
	retry.ConfigureGlobalRateLimitGate(50*time.Millisecond, 100*time.Millisecond, 0)
	t.Cleanup(func() {
		retry.ConfigureGlobalRateLimitGate(
			retry.DefaultRateLimitCooldown,
			retry.DefaultRateLimitMaxCooldown,
			retry.DefaultMinRequestInterval,
		)
	})

	cfg := retry.LLMRetryConfig(3)
	throttled := &httputil.StreamHTTPError{StatusCode: 429, Headers: http.Header{}}

	plain := newTestClient("http://example.invalid", WithRetry(cfg))
	if got := plain.streamRetryConfig().GetRetryDelay(throttled); got != 0 {
		t.Fatalf("opt-out client must keep the original delay (0 without Retry-After), got %v", got)
	}

	gated := newTestClient("http://example.invalid", WithRetry(cfg), WithRateLimitGate())
	if got := gated.streamRetryConfig().GetRetryDelay(throttled); got < 50*time.Millisecond {
		t.Fatalf("opt-in client must apply adaptive cooldown on 429, got %v", got)
	}
}

func TestStreamRetryConfig_ServerRetryAfterStillWins(t *testing.T) {
	retry.ConfigureGlobalRateLimitGate(20*time.Millisecond, 40*time.Millisecond, 0)
	t.Cleanup(func() {
		retry.ConfigureGlobalRateLimitGate(
			retry.DefaultRateLimitCooldown,
			retry.DefaultRateLimitMaxCooldown,
			retry.DefaultMinRequestInterval,
		)
	})

	cfg := retry.LLMRetryConfig(3)
	gated := newTestClient("http://example.invalid", WithRetry(cfg), WithRateLimitGate())
	withHint := &httputil.StreamHTTPError{
		StatusCode: 429,
		Headers:    http.Header{"Retry-After": []string{"2"}},
	}
	if got := gated.streamRetryConfig().GetRetryDelay(withHint); got != 2*time.Second {
		t.Fatalf("server Retry-After must win over cooldown, got %v", got)
	}

	// 非限流错误（5xx）不得打开冷却窗口，保持原有退避节奏。
	serverErr := &httputil.StreamHTTPError{StatusCode: 500, Headers: http.Header{}}
	if got := gated.streamRetryConfig().GetRetryDelay(serverErr); got != 0 {
		t.Fatalf("non-429 must keep the original delay, got %v", got)
	}
}
