package stages

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

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
	// MessageBuilder 自定义消息构造函数。
	// 如果为 nil，默认将 Message.Text 作为 user message。
	MessageBuilder func(msg core.Message) []llm.Message
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
	systemPrompt := s.config.SystemPrompt
	if v, ok := env.Get("system.prompt"); ok {
		if sp, ok := v.(string); ok && sp != "" {
			systemPrompt = sp
		}
	}

	// 解析工具列表
	tools := resolveTools(ctx, s.config, env)

	// 构建参数
	params := llm.GenerateParams{
		Model:       s.config.Model,
		System:      systemPrompt,
		Messages:    messages,
		Tools:       tools,
		Temperature: s.config.Temperature,
		MaxTokens:   s.config.MaxTokens,
	}

	cfg := &llm.OrchestrateConfig{
		Params:   params,
		MaxSteps: s.config.MaxSteps,
	}

	logger.Debugw("llm stage: starting orchestrate",
		"message_id", env.Message.ID,
		"provider", s.provider.Name(),
		"max_steps", s.config.MaxSteps)

	result, err := llm.OrchestrateGenerate(ctx, s.provider, cfg)
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
			"source_channel": env.Message.Source, // ChannelReplyHandler 路由必需
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
