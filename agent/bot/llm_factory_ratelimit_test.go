package bot

import (
	"net/http"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// 非流式路径的 429 有两种载体：retry 包自带的 *HTTPStatusError，以及
// 只携带状态码的 errs.Error。两者都必须登记冷却，否则 workflow 的
// SubAgent 仍会用 2s 起步的退避反复撞限流。
func TestBuildRetryConfig_AppliesAdaptiveCooldownOn429(t *testing.T) {
	cfg := config.DefaultLLMClientConfig()
	cfg.RateLimitCooldownMS = 40
	cfg.RateLimitMaxCooldownMS = 80
	cfg.MinRequestIntervalMS = 0
	t.Cleanup(func() {
		retry.ConfigureGlobalRateLimitGate(
			retry.DefaultRateLimitCooldown,
			retry.DefaultRateLimitMaxCooldown,
			retry.DefaultMinRequestInterval,
		)
	})

	rc := buildRetryConfig(cfg)
	if rc.GetRetryDelay == nil {
		t.Fatal("GetRetryDelay must be configured")
	}

	if got := rc.GetRetryDelay(&retry.HTTPStatusError{StatusCode: 429, Headers: http.Header{}}); got < 40*time.Millisecond {
		t.Fatalf("HTTPStatusError 429 must open adaptive cooldown, got %v", got)
	}
	if got := rc.GetRetryDelay(errs.New("upstream rejected").WithCode(http.StatusTooManyRequests)); got < 40*time.Millisecond {
		t.Fatalf("errs.Error 429 must open adaptive cooldown, got %v", got)
	}
	if got := rc.GetRetryDelay(&retry.HTTPStatusError{StatusCode: 500, Headers: http.Header{}}); got != 0 {
		t.Fatalf("non-429 must keep the original delay, got %v", got)
	}
}

func TestBuildRetryConfig_DefaultRateLimitValues(t *testing.T) {
	d := config.DefaultLLMClientConfig()
	if d.RateLimitCooldownMS != 8000 || d.RateLimitMaxCooldownMS != 60000 || d.MinRequestIntervalMS != 150 {
		t.Fatalf("unexpected rate limit defaults: %+v", d)
	}
}
