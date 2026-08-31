package llm

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

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
//
// IMPORTANT: This also clears the WithStatsSkip flag. Pipeline stages set
// WithStatsSkip to prevent double-counting of orchestrated calls, but
// explicit WithStatsFeature means the caller wants this specific call
// recorded (e.g. subagent tool calls within a pipeline stage). Without
// clearing the skip flag, subagent/workflow tool LLM calls inside a
// pipeline would be silently dropped from stats.
func WithStatsFeature(ctx context.Context, feature string) context.Context {
	// Clear skip flag — explicit feature implies desired recording
	ctx = context.WithValue(ctx, statsSkipKey{}, false)
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
	// 可观测性：一次 LLM 请求边界（含模型 / 消息数 / 工具数）。
	p.emitLLMEvent(ctx, params, core.EventLLMRequest, false, nil, nil)
	result, err := p.inner.DoGenerate(ctx, params)
	if err == nil && result != nil {
		p.record(ctx, params, result)
	}
	// 可观测性：一次 LLM 响应边界（含 finish_reason / token 用量 / 工具调用数）。
	p.emitLLMEvent(ctx, params, core.EventLLMResponse, false, result, err)
	return result, err
}

// DoStream calls the inner provider and returns a wrapped stream that records
// usage when the stream completes (FinishPart arrives).
func (p *StatsRecordingProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	// 可观测性：流式请求边界（响应事件在流结束时于 wrapStream 内补发）。
	p.emitLLMEvent(ctx, params, core.EventLLMRequest, true, nil, nil)
	result, err := p.inner.DoStream(ctx, params)
	if err == nil && result != nil {
		result = p.wrapStream(ctx, params, result)
	}
	return result, err
}

// emitLLMEvent 向 EventSink 追加一条 LLM 请求/响应边界事件（append-only 轨迹）。
// 事件独立于 stats 记录：即使调用方用 WithStatsSkip 跳过了用量统计，
// 边界事件仍照常发出，供可观测性 / 回放消费。无 sink 时（NoopSink）自动丢弃。
func (p *StatsRecordingProvider) emitLLMEvent(ctx context.Context, params GenerateParams, kind core.EventKind, stream bool, result *GenerateResult, callErr error) {
	sink := core.EventSinkFromContext(ctx)
	feature := statsFeatureFromContext(ctx)
	if feature == "" {
		feature = "unknown"
	}
	payload := map[string]any{
		"model":   p.modelID(params),
		"feature": feature,
		"stream":  stream,
	}
	if kind == core.EventLLMRequest {
		payload["messages"] = len(params.Messages)
		payload["tools"] = len(params.Tools)
		payload["has_system"] = params.System != ""
	} else {
		toolCalls := 0
		if result != nil {
			for _, step := range result.Steps {
				toolCalls += len(step.ToolCalls)
			}
			toolCalls += len(result.ToolCalls)
			payload["finish_reason"] = string(result.FinishReason)
			payload["raw_finish_reason"] = result.RawFinishReason
			payload["steps"] = len(result.Steps)
			payload["usage"] = map[string]any{
				"input":     result.Usage.InputTokens,
				"output":    result.Usage.OutputTokens,
				"total":     result.Usage.TotalTokens,
				"cached":    result.Usage.CachedInputTokens,
				"reasoning": result.Usage.ReasoningTokens,
			}
		}
		payload["tool_calls"] = toolCalls
		if callErr != nil {
			payload["error"] = callErr.Error()
		}
	}
	sink.Emit(ctx, core.Event{
		Kind:    kind,
		Source:  "llm:" + p.inner.Name(),
		Surface: false,
		Payload: payload,
	})
}

// modelID 返回本次请求使用的模型标识（优先取 params.Model，否则退化为 provider 名）。
func (p *StatsRecordingProvider) modelID(params GenerateParams) string {
	if params.Model != nil && params.Model.ID != "" {
		return params.Model.ID
	}
	return p.inner.Name()
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
		At:        time.Now(),
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
		p.record(ctx, params, &GenerateResult{Usage: totalUsage})
	}
	// 可观测性：流式响应边界（流结束时补发；即使用量未上报也照常记录边界）。
	p.emitLLMEvent(ctx, params, core.EventLLMResponse, true, &GenerateResult{Usage: totalUsage}, nil)
	}()

	sr.Stream = wrapped
	return sr
}
