package llm

import (
	"errors"
	"testing"
	"time"
)

func TestLLMError_Error(t *testing.T) {
	e := NewLLMError(ErrorReasonRateLimit, "anthropic", "rate limited")
	if e.Error() != "anthropic: rate_limit: rate limited" {
		t.Errorf("unexpected error string: %s", e.Error())
	}

	e2 := NewLLMError(ErrorReasonRateLimit, "", "rate limited")
	if e2.Error() != "rate_limit: rate limited" {
		t.Errorf("unexpected error string without provider: %s", e2.Error())
	}
}

func TestErrorReason_IsRetryable(t *testing.T) {
	retryable := []ErrorReason{
		ErrorReasonRateLimit,
		ErrorReasonProviderInternal,
		ErrorReasonTransport,
	}
	for _, r := range retryable {
		if !r.IsRetryable() {
			t.Errorf("expected %s to be retryable", r)
		}
	}

	notRetryable := []ErrorReason{
		ErrorReasonInvalidRequest,
		ErrorReasonAuthentication,
		ErrorReasonQuotaExceeded,
		ErrorReasonContentPolicy,
		ErrorReasonInvalidProviderOutput,
		ErrorReasonUnknownProvider,
		ErrorReasonNoRoute,
	}
	for _, r := range notRetryable {
		if r.IsRetryable() {
			t.Errorf("expected %s to NOT be retryable", r)
		}
	}
}

func TestLLMError_RetryableFlag(t *testing.T) {
	e := NewLLMError(ErrorReasonRateLimit, "openai", "too many requests")
	if !e.Retryable {
		t.Error("rate_limit should be retryable by default")
	}

	e2 := NewLLMError(ErrorReasonAuthentication, "openai", "bad key")
	if e2.Retryable {
		t.Error("authentication should not be retryable")
	}

	// Override retryable
	e3 := NewLLMError(ErrorReasonAuthentication, "openai", "bad key", WithRetryable(true))
	if !e3.Retryable {
		t.Error("WithRetryable(true) should override")
	}
}

func TestLLMError_RetryAfter(t *testing.T) {
	e := NewLLMError(ErrorReasonRateLimit, "anthropic", "slow down", WithRetryAfter(5*time.Second))
	if e.RetryAfterMs != 5000 {
		t.Errorf("expected 5000ms, got %d", e.RetryAfterMs)
	}
	if e.RetryAfter() != 5*time.Second {
		t.Errorf("expected 5s, got %v", e.RetryAfter())
	}

	e2 := NewLLMError(ErrorReasonInvalidRequest, "openai", "bad")
	if e2.RetryAfter() != 0 {
		t.Errorf("expected 0 retry delay, got %v", e2.RetryAfter())
	}
}

func TestLLMError_Unwrap(t *testing.T) {
	inner := errors.New("network reset")
	e := NewLLMError(ErrorReasonTransport, "anthropic", "conn reset", WithCause(inner))
	if !errors.Is(e, inner) {
		t.Error("errors.Is should find the cause")
	}
}

func TestAsLLMError(t *testing.T) {
	e := NewLLMError(ErrorReasonRateLimit, "openai", "429")
	wrapped := errors.New("wrapper")

	// Direct
	if le, ok := AsLLMError(e); !ok || le.Reason != ErrorReasonRateLimit {
		t.Error("should extract LLMError directly")
	}

	// Non-LLM error
	if _, ok := AsLLMError(wrapped); ok {
		t.Error("should return false for non-LLM error")
	}

	// Nil
	if _, ok := AsLLMError(nil); ok {
		t.Error("should return false for nil")
	}
}

func TestIsRetryableLLMError(t *testing.T) {
	retryable := NewLLMError(ErrorReasonRateLimit, "x", "429")
	if !IsRetryableLLMError(retryable) {
		t.Error("rate_limit should be retryable")
	}

	notRetryable := NewLLMError(ErrorReasonAuthentication, "x", "401")
	if IsRetryableLLMError(notRetryable) {
		t.Error("authentication should not be retryable")
	}

	plain := errors.New("plain error")
	if IsRetryableLLMError(plain) {
		t.Error("plain error should not be retryable")
	}
}
