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
// 工作流维度（workflow / node）
//
// 与 statsFeatureKey 同构，但**不参与 UsageDaily 的聚合维度**——
// 那条聚合链按 (bot, model, feature, channel, date) 归并，加入工作流维度
// 会把日聚合表撑成明细表（每条工作流每节点每天一行）。
//
// 这里的取值只用于**旁路写入逐条明细表**（stats.WorkflowUsage），
// 让「一条工作流花在哪、哪个节点最贵」可回答，同时不污染日聚合语义。
// ============================================================================

type statsWorkflowIDKey struct{}
type statsNodeIDKey struct{}

// WithStatsWorkflow 标记本次 LLM 调用属于哪条工作流的哪个节点。
//
// 与 WithStatsFeature 不同，本函数**不清除** WithStatsSkip 标志：
// 它的职责只是给已决定要记录的调用补充归因维度，
// 是否记录仍由 skip / feature 那一套决定。
func WithStatsWorkflow(ctx context.Context, workflowID, nodeID string) context.Context {
	ctx = context.WithValue(ctx, statsWorkflowIDKey{}, workflowID)
	return context.WithValue(ctx, statsNodeIDKey{}, nodeID)
}

func statsWorkflowFromContext(ctx context.Context) (workflowID, nodeID string) {
	wf, _ := ctx.Value(statsWorkflowIDKey{}).(string)
	n, _ := ctx.Value(statsNodeIDKey{}).(string)
	return wf, n
}

// ============================================================================
// 工作区写操作记录
//
// 用途：并发写冲突检测。工作流默认并行 3 个节点、共享同一个 bot 工作区、
// 无任何文件锁。若不记录谁写了什么，两个节点覆盖同一文件时毫无痕迹。
//
// 接口定义在 llm 包（两个使用方都依赖它）：
//   - sandbox 的写类工具：执行时上报路径
//   - workflow 引擎：注入 recorder，事后读取并检测冲突
//
// 刻意**不回传执行结果**——工具只上报「我写了哪里」，是否冲突由引擎判定，
// 工具不关心、也不该关心调度语义。
// ============================================================================

// PathRecorder 记录工作区写操作的发生。
type PathRecorder interface {
	// RecordWrite 上报一次写操作。op 为操作类型（write/replace/delete/move）。
	// 实现必须线程安全：并行节点会并发调用。
	RecordWrite(path string, op string)
}

type pathRecorderKey struct{}

// WithPathRecorder 把写操作记录器放进 ctx。
func WithPathRecorder(ctx context.Context, rec PathRecorder) context.Context {
	return context.WithValue(ctx, pathRecorderKey{}, rec)
}

// PathRecorderFromContext 取出写操作记录器，不存在时返回 nil。
// 调用方必须判空——非工作流路径不会注入。
func PathRecorderFromContext(ctx context.Context) PathRecorder {
	rec, _ := ctx.Value(pathRecorderKey{}).(PathRecorder)
	return rec
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
	// 工作流维度：非 workflow 路径两者均为空，不影响既有统计。
	// 注意它们**不进 UsageDaily 的聚合维度**，仅供旁路明细写入。
	workflowID, nodeID := statsWorkflowFromContext(ctx)
	p.recorder.RecordUsage(ctx, UsageMetric{
		BotID:      p.botID,
		At:         time.Now(),
		Model:      modelID,
		Feature:    feature,
		Usage:      result.Usage,
		ToolCalls:  toolCalls,
		Steps:      len(result.Steps),
		WorkflowID: workflowID,
		NodeID:     nodeID,
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
