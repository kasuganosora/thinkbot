package llm

import "context"

// ============================================================================
// Context helpers — pass stats metadata through context
// ============================================================================

// statsSkipKey marks a context where StatsRecordingProvider should skip
// recording. Pipeline stages (reply / llmroute) set this flag before calling
// Orchestrate, then record the combined result themselves via recordUsage().
// This prevents double-counting: once per-step inside StatsRecordingProvider
// and once at the stage level.
type statsSkipKey struct{}

// WithStatsSkip marks the context so that StatsRecordingProvider does NOT
// record. The caller assumes responsibility for recording.
func WithStatsSkip(ctx context.Context) context.Context {
	return context.WithValue(ctx, statsSkipKey{}, true)
}

func shouldSkipStats(ctx context.Context) bool {
	v, _ := ctx.Value(statsSkipKey{}).(bool)
	return v
}

// statsFeatureKey carries the feature label (e.g. "vision", "subagent",
// "memory_formation") for stats recording.
type statsFeatureKey struct{}

// WithStatsFeature sets the feature label for stats recording in context.
// Non-stage callers use this to tag their LLM calls with a meaningful label.
func WithStatsFeature(ctx context.Context, feature string) context.Context {
	return context.WithValue(ctx, statsFeatureKey{}, feature)
}

func statsFeatureFromContext(ctx context.Context) string {
	v, _ := ctx.Value(statsFeatureKey{}).(string)
	return v
}

// ============================================================================
// StatsRecordingProvider
// ============================================================================

// StatsRecordingProvider wraps an llm.Provider and records token usage to
// the stats system after each DoGenerate / DoStream call.
//
// When WithStatsSkip is present in the context, recording is skipped — the
// caller handles recording itself (e.g. pipeline stages record the combined
// orchestration result via recordUsage).
//
// When WithStatsFeature is present, the feature label is used; otherwise
// "unknown" is used as a fallback.
type StatsRecordingProvider struct {
	inner    Provider
	recorder UsageRecorder
	botID    string
}

// NewStatsRecordingProvider creates a provider that auto-records token usage.
// botID is fixed at construction time (the bot always stays the same).
// Feature is read from context via WithStatsFeature.
func NewStatsRecordingProvider(inner Provider, recorder UsageRecorder, botID string) *StatsRecordingProvider {
	return &StatsRecordingProvider{inner: inner, recorder: recorder, botID: botID}
}

// Name delegates to the inner provider.
func (p *StatsRecordingProvider) Name() string { return p.inner.Name() }

// DoGenerate calls the inner provider, then records usage if stats are not
// skipped and a recorder is configured.
func (p *StatsRecordingProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	result, err := p.inner.DoGenerate(ctx, params)
	if err == nil && result != nil {
		p.record(ctx, params, result)
	}
	return result, err
}

// DoStream calls the inner provider and returns a wrapped stream that records
// usage when the stream completes (FinishPart arrives).
func (p *StatsRecordingProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	result, err := p.inner.DoStream(ctx, params)
	if err == nil && result != nil {
		result = p.wrapStream(ctx, params, result)
	}
	return result, err
}

// record builds a UsageMetric and forwards it to the recorder.
func (p *StatsRecordingProvider) record(ctx context.Context, params GenerateParams, result *GenerateResult) {
	if p.recorder == nil || shouldSkipStats(ctx) || result.Usage.TotalTokens <= 0 {
		return
	}
	feature := statsFeatureFromContext(ctx)
	if feature == "" {
		feature = "unknown"
	}
	modelID := ""
	if params.Model != nil {
		modelID = params.Model.ID
	}
	if modelID == "" {
		modelID = p.inner.Name()
	}
	toolCalls := 0
	for _, step := range result.Steps {
		toolCalls += len(step.ToolCalls)
	}
	p.recorder.RecordUsage(ctx, UsageMetric{
		BotID:     p.botID,
		Model:     modelID,
		Feature:   feature,
		Usage:     result.Usage,
		ToolCalls: toolCalls,
		Steps:     len(result.Steps),
	})
}

// wrapStream wraps the stream channel to detect the FinishPart and record
// usage on completion.
func (p *StatsRecordingProvider) wrapStream(ctx context.Context, params GenerateParams, sr *StreamResult) *StreamResult {
	orig := sr.Stream
	wrapped := make(chan StreamPart, 16)

	go func() {
		defer close(wrapped)
		var totalUsage Usage
		for part := range orig {
			if fp, ok := part.(*FinishPart); ok {
				totalUsage = fp.TotalUsage
			}
			wrapped <- part
		}
		if totalUsage.TotalTokens > 0 {
			result := &GenerateResult{Usage: totalUsage}
			p.record(ctx, params, result)
		}
	}()

	sr.Stream = wrapped
	return sr
}
