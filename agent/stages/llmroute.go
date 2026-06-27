package stages

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// StreamPublisher — LLM 流式输出发布器
// ============================================================================

// StreamPublisher 发布 LLM 流式增量（文本 + 工具调用）。
// 当 LLMConfig.StreamPublisher 非 nil 时，LLMStage 使用 OrchestrateStream，
// 并将每个增量通过此接口发布，供 SSE handler 实时消费。
type StreamPublisher interface {
	PublishTextDelta(ctx context.Context, traceID, botID, text string)
	PublishToolCall(ctx context.Context, traceID, botID, toolName string, input any)
	PublishToolResult(ctx context.Context, traceID, botID, toolName string, output any, errMsg string)
}

// ============================================================================
// ToolResolver — 动态工具解析接口
// ============================================================================

// ToolResolver 根据请求上下文动态解析可用工具列表。
// 如果 LLMConfig.ToolResolver 非 nil，Stage 在构建 GenerateParams 时自动调用，
// 解析出的工具会注入到 GenerateParams.Tools（provider 支持则自动 function calling）。
//
// ToolManager.ResolveForEnvelope 自然满足此接口，无需额外适配。
type ToolResolver interface {
	ResolveForEnvelope(ctx context.Context, env *core.Envelope) ([]llm.Tool, error)
}

// resolveTools 解析工具列表：优先用 ToolResolver 动态解析，回退到静态 Tools。
func resolveTools(ctx context.Context, cfg LLMConfig, env *core.Envelope) []llm.Tool {
	if cfg.ToolResolver != nil {
		tools, err := cfg.ToolResolver.ResolveForEnvelope(ctx, env)
		if err == nil && len(tools) > 0 {
			return tools
		}
	}
	return cfg.Tools
}

// ============================================================================
// LLMStage — 调用 LLM Provider 生成回复
// ============================================================================

// LLMConfig 配置 LLM Stage。
type LLMConfig struct {
	// SystemPrompt 系统提示词。
	SystemPrompt string
	// MaxSteps Orchestrate 最大执行步数（0=单次, >0=多步, -1=无限）。
	MaxSteps int
	// Tools 静态工具列表。
	// 如果 ToolResolver 为 nil，直接使用此列表。
	Tools []llm.Tool
	// ToolResolver 动态工具解析器。
	// 非 nil 时，每次请求自动按上下文解析工具（覆盖 Tools）。
	// 通常传入 *tools.ToolManager 实例。
	ToolResolver ToolResolver
	// Model 指定使用的模型。
	Model *llm.Model
	// Temperature 采样温度。
	Temperature *float64
	// MaxTokens 最大 token 数。
	MaxTokens *int
	// ReasoningEffort 深度思考程度（""=禁用, "minimal", "low", "medium", "high"）。
	ReasoningEffort string
	// MessageBuilder 自定义消息构造函数。
	// 如果为 nil，默认将 Message.Text 作为 user message。
	MessageBuilder func(msg core.Message) []llm.Message
	// UsageRecorder 可选的使用统计记录器。
	// 非 nil 时，每次 LLM 调用后自动记录 bot/model/feature 维度的用量。
	UsageRecorder llm.UsageRecorder

	// StreamPublisher 可选的流式输出发布器。
	// 非 nil 时，LLMStage 使用 OrchestrateStream（流式生成），
	// 并将文本增量通过此发布器推送，供 SSE handler 实时消费。
	StreamPublisher StreamPublisher

	// ReductionConfig 可选的上下文压缩配置。
	// 非 nil 时，在 orchestration 循环中启用两阶段压缩：
	//   Phase 1: 工具执行后截断超大输出
	//   Phase 2: 模型调用前压缩旧消息历史
	// 为 nil 时禁用压缩（仅依赖 PatchToolCalls 安全网）。
	ReductionConfig *llm.ReductionConfig
}

// ============================================================================
type LLMStage struct {
	name     string
	provider llm.Provider
	config   LLMConfig
	tracer   trace.Tracer
	logger   *zap.SugaredLogger
}

// NewLLMStage 创建 LLM Stage。
func NewLLMStage(name string, provider llm.Provider, config LLMConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *LLMStage {
	if name == "" {
		name = "llm"
	}
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &LLMStage{
		name:     name,
		provider: provider,
		config:   config,
		tracer:   tp.Tracer("github.com/kasuganosora/thinkbot/agent/stages"),
		logger:   logger,
	}
}

// Name 返回 Stage 名称。
func (s *LLMStage) Name() string { return s.name }

// reasoningEffortPtr 将非空字符串转为 *string，空字符串返回 nil。
func reasoningEffortPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Process 调用 LLM 生成回复。
func (s *LLMStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.llm.orchestrate",
		trace.WithAttributes(
			attribute.String("llm.provider", s.provider.Name()),
			attribute.String("message.id", env.Message.ID),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, s.logger)

	// 构建消息
	var messages []llm.Message
	if s.config.MessageBuilder != nil {
		messages = s.config.MessageBuilder(env.Message)
	} else {
		messages = []llm.Message{llm.UserMessage(env.Message.Text)}
	}

	// 解析 system prompt：优先从 Envelope KV 读取动态组装的 prompt（PromptStage 注入），
	// 回退到 LLMConfig.SystemPrompt 静态配置（向后兼容）。
	// 并将延迟注入的 pipeline 警告（token 预算、循环检测等）合并到 system prompt 末尾。
	systemPrompt := s.config.SystemPrompt
	if v, ok := env.Get("system.prompt"); ok {
		if sp, ok := v.(string); ok && sp != "" {
			systemPrompt = sp
		}
	}
	systemPrompt = core.MergeWarnings(env, systemPrompt)

	// 解析工具列表
	tools := resolveTools(ctx, s.config, env)

	// 构建参数
	params := llm.GenerateParams{
		Model:           s.config.Model,
		System:          systemPrompt,
		Messages:        messages,
		Tools:           tools,
		Temperature:     s.config.Temperature,
		MaxTokens:       s.config.MaxTokens,
		ReasoningEffort: reasoningEffortPtr(s.config.ReasoningEffort),
	}

	cfg := &llm.OrchestrateConfig{
		Params:   params,
		MaxSteps: s.config.MaxSteps,
	}

	// Enable reduction if configured.
	if s.config.ReductionConfig != nil {
		rc := *s.config.ReductionConfig
		cfg.OnToolResults = llm.NewOnToolResultsCallback(rc)
		cfg.PrepareStep = llm.NewReducePrepareStepCallback(rc)
	}

	logger.Debugw("llm stage: starting orchestrate",
		"message_id", env.Message.ID,
		"provider", s.provider.Name(),
		"max_steps", s.config.MaxSteps,
		"streaming", s.config.StreamPublisher != nil)

	var result *llm.GenerateResult
	// WithStatsSkip: StatsRecordingProvider 会跳过 Orchestrate 内部的每次调用，
	// 由下方 recordUsage() 统一记录合并后的总用量到 journal + stats
	statsCtx := llm.WithStatsSkip(ctx)
	if s.config.StreamPublisher != nil {
		var err error
		result, err = s.processStream(statsCtx, env, cfg, logger)
		if err != nil {
			span.RecordError(err)
			logger.Errorw("llm stage: stream orchestrate failed",
				"message_id", env.Message.ID,
				"err", err)
			return env, &core.PipelineError{
				Stage:   s.name,
				Message: "LLM stream orchestrate failed",
				Cause:   err,
			}
		}
	} else {
		var err error
		result, err = llm.OrchestrateGenerate(statsCtx, s.provider, cfg)
		if err != nil {
			span.RecordError(err)
			logger.Errorw("llm stage: orchestrate failed",
				"message_id", env.Message.ID,
				"err", err)
			return env, &core.PipelineError{
				Stage:   s.name,
				Message: "LLM orchestrate failed",
				Cause:   err,
			}
		}
	}

	// 记录 OTel 属性
	span.SetAttributes(
		attribute.Int("llm.steps", len(result.Steps)),
		attribute.Int("llm.total_tokens", result.Usage.TotalTokens),
		attribute.Int("llm.input_tokens", result.Usage.InputTokens),
		attribute.Int("llm.output_tokens", result.Usage.OutputTokens),
		attribute.String("llm.finish_reason", string(result.FinishReason)),
	)

	logger.Infow("llm stage: generation complete",
		"message_id", env.Message.ID,
		"steps", len(result.Steps),
		"tokens", result.Usage.TotalTokens,
		"finish_reason", result.FinishReason)

	// 记录使用统计
	recordUsage(ctx, s.config.UsageRecorder, env, s.config.Model, s.name, result)

	// 将回复添加为 Action
	// 使用 reply_target 作为 outbound 回复目标（由 Channel 在 Inbound 时设置）
	replyTarget := env.Message.Channel // 默认使用 Channel（向后兼容）
	if env.Message.Metadata != nil {
		if rt, ok := env.Message.Metadata["reply_target"]; ok {
			if s, ok := rt.(string); ok && s != "" {
				replyTarget = s
			}
		}
	}

	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: replyTarget,
		UserID:  env.Message.UserID,
		Payload: result.Text,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,  // ChannelReplyHandler 路由必需
			"trace_id":       env.Message.TraceID, // WebChannel 路由必需
			"finish_reason":  string(result.FinishReason),
			"usage":          result.Usage,
			"tool_calls":     result.ToolCalls,
			"steps":          len(result.Steps),
		},
	})

	// 在 Envelope KV 中存储完整结果
	env.Set("llm.result", result)

	return env, nil
}

// processStream 使用 OrchestrateStream 执行流式生成，
// 将文本增量通过 StreamPublisher 实时发布，最终返回完整的 GenerateResult。
//
// 注意：stream channel 只能消费一次，因此这里手动组装 GenerateResult，
// 而不是调用 StreamResult.ToResult()（后者会再次 range 已关闭的 channel）。
func (s *LLMStage) processStream(ctx context.Context, env *core.Envelope, cfg *llm.OrchestrateConfig, logger *zap.SugaredLogger) (*llm.GenerateResult, error) {
	streamResult, err := llm.OrchestrateStream(ctx, s.provider, cfg)
	if err != nil {
		return nil, err
	}

	traceID := env.Message.TraceID
	botID := env.Message.BotID
	publisher := s.config.StreamPublisher

	result := &llm.GenerateResult{}

	// 单次消费 stream channel，同时转发 text delta 到 EventBus
	for part := range streamResult.Stream {
		switch p := part.(type) {
		case *llm.TextDeltaPart:
			result.Text += p.Text
			if p.Text != "" {
				publisher.PublishTextDelta(ctx, traceID, botID, p.Text)
			}
		case *llm.ReasoningDeltaPart:
			result.Reasoning += p.Text
		case *llm.StreamToolCallPart:
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				ToolCallID: p.ToolCallID,
				ToolName:   p.ToolName,
				Input:      p.Input,
			})
			publisher.PublishToolCall(ctx, traceID, botID, p.ToolName, p.Input)
		case *llm.StreamToolResultPart:
			result.ToolResults = append(result.ToolResults, llm.ToolResult{
				ToolCallID: p.ToolCallID,
				ToolName:   p.ToolName,
				Output:     p.Output,
			})
			publisher.PublishToolResult(ctx, traceID, botID, p.ToolName, p.Output, "")
		case *llm.FinishStepPart:
			result.Response = p.Response
			if result.Usage.TotalTokens == 0 {
				result.Usage = p.Usage
				result.FinishReason = p.FinishReason
				result.RawFinishReason = p.RawFinishReason
			}
		case *llm.FinishPart:
			result.FinishReason = p.FinishReason
			result.RawFinishReason = p.RawFinishReason
			result.Usage = p.TotalUsage
		case *llm.ErrorPart:
			return nil, p.Error
		}
	}

	result.Steps = streamResult.Steps
	result.Messages = streamResult.Messages

	logger.Debugw("llm stage: stream completed",
		"message_id", env.Message.ID,
		"steps", len(result.Steps),
		"text_len", len(result.Text))

	return result, nil
}

// recordUsage 从 Envelope 提取 bot_id，构建 UsageMetric 并异步记录。
// recorder 为 nil 时跳过。
func recordUsage(ctx context.Context, recorder llm.UsageRecorder, env *core.Envelope, model *llm.Model, feature string, result *llm.GenerateResult) {
	if recorder == nil {
		return
	}
	botID := ""
	if v, ok := env.Get("bot.id"); ok {
		if s, ok := v.(string); ok {
			botID = s
		}
	}
	modelID := ""
	if model != nil {
		modelID = model.ID
	}
	toolCalls := 0
	steps := len(result.Steps)
	for _, step := range result.Steps {
		toolCalls += len(step.ToolCalls)
	}
	recorder.RecordUsage(ctx, llm.UsageMetric{
		BotID:     botID,
		Model:     modelID,
		Feature:   feature,
		Channel:   env.Message.Channel,
		Usage:     result.Usage,
		ToolCalls: toolCalls,
		Steps:     steps,
	})
}
