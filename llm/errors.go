package llm

import (
	"errors"
	"fmt"
	"time"
)

// ============================================================================
// Unified LLM Error Classification (P0)
//
// All provider adapters should wrap API errors into *LLMError so the
// orchestration layer can make intelligent retry/routing decisions based on
// the structured Reason rather than parsing raw error strings.
// ============================================================================

// ErrorReason classifies the root cause of an LLM operation failure.
type ErrorReason string

const (
	ErrorReasonInvalidRequest        ErrorReason = "invalid_request"
	ErrorReasonContextOverflow       ErrorReason = "context_overflow"
	ErrorReasonAuthentication        ErrorReason = "authentication"
	ErrorReasonRateLimit             ErrorReason = "rate_limit"
	ErrorReasonQuotaExceeded         ErrorReason = "quota_exceeded"
	ErrorReasonContentPolicy         ErrorReason = "content_policy"
	ErrorReasonProviderInternal      ErrorReason = "provider_internal"
	ErrorReasonTransport             ErrorReason = "transport"
	ErrorReasonInvalidProviderOutput ErrorReason = "invalid_provider_output"
	ErrorReasonUnknownProvider       ErrorReason = "unknown_provider"
	ErrorReasonNoRoute               ErrorReason = "no_route"
)

// IsRetryable returns true for error reasons that may succeed on retry.
func (r ErrorReason) IsRetryable() bool {
	switch r {
	case ErrorReasonRateLimit, ErrorReasonProviderInternal, ErrorReasonTransport:
		return true
	default:
		return false
	}
}

// HTTPContext captures HTTP request/response details for debugging.
type HTTPContext struct {
	StatusCode      int               `json:"statusCode,omitempty"`
	URL             string            `json:"url,omitempty"`
	Method          string            `json:"method,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
	ResponseBody    string            `json:"responseBody,omitempty"`
}

// LLMError is the unified error type returned by all LLM provider operations.
type LLMError struct {
	Reason       ErrorReason  `json:"reason"`
	Message      string       `json:"message"`
	ProviderName string       `json:"providerName,omitempty"`
	Retryable    bool         `json:"retryable"`
	RetryAfterMs int          `json:"retryAfterMs,omitempty"`
	HTTPContext  *HTTPContext `json:"httpContext,omitempty"`

	// cause is the underlying error (for errors.Is/errors.As chain).
	cause error
}

func (e *LLMError) Error() string {
	prefix := e.ProviderName
	if prefix == "" {
		prefix = string(e.Reason)
		return fmt.Sprintf("%s: %s", prefix, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", prefix, e.Reason, e.Message)
}

// Unwrap supports errors.Is / errors.As.
func (e *LLMError) Unwrap() error { return e.cause }

// RetryAfter returns the server-suggested retry delay.
func (e *LLMError) RetryAfter() time.Duration {
	if e.RetryAfterMs <= 0 {
		return 0
	}
	return time.Duration(e.RetryAfterMs) * time.Millisecond
}

// NewLLMError creates a structured LLMError.
func NewLLMError(reason ErrorReason, provider, message string, opts ...LLMErrorOpt) *LLMError {
	e := &LLMError{
		Reason:       reason,
		Message:      message,
		ProviderName: provider,
		Retryable:    reason.IsRetryable(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// LLMErrorOpt configures an LLMError.
type LLMErrorOpt func(*LLMError)

// WithCause sets the underlying error.
func WithCause(err error) LLMErrorOpt {
	return func(e *LLMError) { e.cause = err }
}

// WithRetryAfter sets the retry delay (from Retry-After header, etc.).
func WithRetryAfter(d time.Duration) LLMErrorOpt {
	return func(e *LLMError) {
		if d > 0 {
			e.RetryAfterMs = int(d / time.Millisecond)
		}
	}
}

// WithHTTPContext attaches HTTP context for debugging.
func WithHTTPContext(ctx *HTTPContext) LLMErrorOpt {
	return func(e *LLMError) { e.HTTPContext = ctx }
}

// WithRetryable overrides the default retryability derived from Reason.
func WithRetryable(retryable bool) LLMErrorOpt {
	return func(e *LLMError) { e.Retryable = retryable }
}

// AsLLMError extracts an *LLMError from an error chain.
func AsLLMError(err error) (*LLMError, bool) {
	if err == nil {
		return nil, false
	}
	var llmErr *LLMError
	if errors.As(err, &llmErr) {
		return llmErr, true
	}
	return nil, false
}

// IsRetryableLLMError checks whether err is a retryable LLMError.
func IsRetryableLLMError(err error) bool {
	if llmErr, ok := AsLLMError(err); ok {
		return llmErr.Retryable
	}
	return false
}
