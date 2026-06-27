package llm

import (
	"context"
)

// QuotaUsageRecorder is called after each successful LLM call to record token
// consumption against a quota dimension. Returns the new total for the dimension.
// Signature matches pipeline.TokenQuotaState.AddUsage for direct assignment.
type QuotaUsageRecorder func(dimension string, tokens int64) int64

// QuotaRecordingProvider wraps an llm.Provider and records token usage after
// each DoGenerate / DoStream call. The dimension is read from context (set by
// the pipeline TokenQuotaMiddleware via pipeline.WithQuotaDimension).
//
// If no dimension is present in the context, the call is silently not recorded
// (e.g., cron-triggered tasks that run outside a user-facing pipeline).
type QuotaRecordingProvider struct {
	inner    Provider
	recorder QuotaUsageRecorder
}

// NewQuotaRecordingProvider creates a provider that auto-records token usage.
func NewQuotaRecordingProvider(inner Provider, recorder QuotaUsageRecorder) *QuotaRecordingProvider {
	return &QuotaRecordingProvider{inner: inner, recorder: recorder}
}

// Name delegates to the inner provider.
func (p *QuotaRecordingProvider) Name() string {
	return p.inner.Name()
}

// DoGenerate calls the inner provider, then records usage if a dimension is present.
func (p *QuotaRecordingProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	result, err := p.inner.DoGenerate(ctx, params)
	if err == nil && result != nil {
		p.recordUsage(ctx, result.Usage.TotalTokens)
	}
	return result, err
}

// DoStream calls the inner provider and returns a wrapped stream that records
// usage when the stream completes.
func (p *QuotaRecordingProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	result, err := p.inner.DoStream(ctx, params)
	if err == nil && result != nil {
		// Wrap the stream channel to record usage on completion.
		// Usage is recorded when the stream is fully consumed and FinishPart arrives.
		result = p.wrapStream(ctx, result)
	}
	return result, err
}

// recordUsage reports token usage to the recorder if a dimension is present.
func (p *QuotaRecordingProvider) recordUsage(ctx context.Context, totalTokens int) {
	if totalTokens <= 0 {
		return
	}
	dim := QuotaDimensionFromContext(ctx)
	if dim == "" {
		return
	}
	p.recorder(dim, int64(totalTokens))
}

// wrapStream wraps the stream channel to detect the FinishPart and record usage.
func (p *QuotaRecordingProvider) wrapStream(ctx context.Context, sr *StreamResult) *StreamResult {
	orig := sr.Stream
	wrapped := make(chan StreamPart, 16)

	go func() {
		defer close(wrapped)
		for part := range orig {
			if fp, ok := part.(*FinishPart); ok {
				// Stream completed — record the accumulated total usage
				p.recordUsage(ctx, fp.TotalUsage.TotalTokens)
			}
			wrapped <- part
		}
	}()

	sr.Stream = wrapped
	return sr
}

// ============================================================================
// Context helpers — pass quota dimension through context
// ============================================================================

type quotaDimCtxKey struct{}

// WithQuotaDimension injects the resolved quota dimension into the context.
// All QuotaRecordingProvider instances read this value and record token usage
// to the correct dimension counter.
func WithQuotaDimension(ctx context.Context, dim string) context.Context {
	return context.WithValue(ctx, quotaDimCtxKey{}, dim)
}

// QuotaDimensionFromContext extracts the quota dimension from context.
// Returns empty string if no dimension is set.
func QuotaDimensionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(quotaDimCtxKey{}).(string)
	return v
}
