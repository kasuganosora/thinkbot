package retry

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPStatusError(t *testing.T) {
	err := &HTTPStatusError{
		StatusCode: 429,
		Headers:    http.Header{},
		Body:       "rate limited",
	}
	if err.Error() == "" {
		t.Error("expected non-empty error message")
	}
	if !IsRateLimitError(err) {
		t.Error("expected rate limit error")
	}
}

func TestIsRateLimitError(t *testing.T) {
	err := &HTTPStatusError{StatusCode: 429}
	if !IsRateLimitError(err) {
		t.Error("429 should be rate limit")
	}

	err2 := &HTTPStatusError{StatusCode: 500}
	if IsRateLimitError(err2) {
		t.Error("500 should not be rate limit")
	}
}

func TestIsOverloadedError(t *testing.T) {
	err := &HTTPStatusError{StatusCode: 503}
	if !IsOverloadedError(err) {
		t.Error("503 should be overloaded")
	}

	err2 := &HTTPStatusError{StatusCode: 529}
	if !IsOverloadedError(err2) {
		t.Error("529 should be overloaded")
	}
}

func TestIsServerError(t *testing.T) {
	err := &HTTPStatusError{StatusCode: 500}
	if !IsServerError(err) {
		t.Error("500 should be server error")
	}

	err2 := &HTTPStatusError{StatusCode: 404}
	if IsServerError(err2) {
		t.Error("404 should not be server error")
	}
}

func TestHTTPShouldRetry(t *testing.T) {
	tests := []struct {
		statusCode int
		expected   bool
	}{
		{429, true},
		{503, true},
		{529, true},
		{500, true},
		{502, true},
		{504, true},
		{408, true},
		{404, false},
		{400, false},
		{401, false},
	}

	for _, tc := range tests {
		err := &HTTPStatusError{StatusCode: tc.statusCode}
		result := HTTPShouldRetry(1, err)
		if result != tc.expected {
			t.Errorf("status %d: expected %v, got %v", tc.statusCode, tc.expected, result)
		}
	}
}

func TestHTTPGetRetryDelay_Seconds(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "5")

	err := &HTTPStatusError{
		StatusCode: 429,
		Headers:    headers,
	}

	delay := HTTPGetRetryDelay(err)
	if delay != 5*time.Second {
		t.Errorf("expected 5s delay, got %v", delay)
	}
}

func TestHTTPGetRetryDelay_Milliseconds(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After-MS", "500")

	err := &HTTPStatusError{
		StatusCode: 429,
		Headers:    headers,
	}

	delay := HTTPGetRetryDelay(err)
	expected := 500 * time.Millisecond
	if delay != expected {
		t.Errorf("expected %v delay, got %v", expected, delay)
	}
}

func TestHTTPGetRetryDelay_None(t *testing.T) {
	err := &HTTPStatusError{
		StatusCode: 500,
		Headers:    http.Header{},
	}

	delay := HTTPGetRetryDelay(err)
	if delay != 0 {
		t.Errorf("expected 0 delay, got %v", delay)
	}
}

func TestHTTPGetRetryDelay_NonHTTPError(t *testing.T) {
	// 非 HTTP 错误应返回 0
	delay := HTTPGetRetryDelay(nil)
	if delay != 0 {
		t.Errorf("expected 0 delay for nil error, got %v", delay)
	}
}

func TestLLMRetryConfig(t *testing.T) {
	cfg := LLMRetryConfig(3)
	if cfg.MaxRetries != 3 {
		t.Errorf("expected 3 retries, got %d", cfg.MaxRetries)
	}
	if cfg.Backoff == nil {
		t.Error("expected non-nil backoff")
	}
	if cfg.Backoff.Strategy != StrategyExponential {
		t.Error("expected exponential strategy")
	}
	if cfg.ShouldRetry == nil {
		t.Error("expected non-nil ShouldRetry")
	}
	if cfg.GetRetryDelay == nil {
		t.Error("expected non-nil GetRetryDelay")
	}
}

func TestDefaultLLMRetryConfig(t *testing.T) {
	cfg := DefaultLLMRetryConfig()
	if cfg.MaxRetries != 3 {
		t.Errorf("expected 3 retries, got %d", cfg.MaxRetries)
	}
}

func TestStreamingRetryConfig(t *testing.T) {
	cfg := StreamingRetryConfig(5)
	if cfg.MaxRetries != 5 {
		t.Errorf("expected 5 retries, got %d", cfg.MaxRetries)
	}
}

func TestAggressiveRetryConfig(t *testing.T) {
	cfg := AggressiveRetryConfig()
	if cfg.MaxRetries != 5 {
		t.Errorf("expected 5 retries, got %d", cfg.MaxRetries)
	}
	if cfg.Backoff.Max < 60*time.Second {
		t.Error("expected max >= 60s for aggressive config")
	}
}
