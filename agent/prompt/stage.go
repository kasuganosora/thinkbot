package prompt

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// PromptStage — Pipeline 系统提示词组装 Stage
// ============================================================================

// PromptStage 是一个 Pipeline Stage，负责在 LLM 调用之前组装完整的 system prompt。
//
// 执行流程：
//  1. 从 Envelope KV 中收集上游 Stage 注入的数据（memory.context、bot.config 等）
//  2. 构建 AssemblyContext
//  3. 调用 Assembler 按 Section Order 组装最终 prompt
//  4. 将结果写入 env.Set("system.prompt", finalPrompt)
//
// PromptStage 通常放在 Order=200 的位置：
//   - 在 MemoryStage (Order=100) 之后 — 确保记忆上下文已注入
//   - 在 LLMStage/ReplyStage (Order=500) 之前 — 确保 LLM 能读到组装后的 prompt
//
// Envelope KV 依赖（上游注入）：
//   - "memory.context": string — 记忆上下文文本（MemoryStage 注入）
//   - "bot.config": BotConfig — Bot 配置（Bot.OnBeforeProcess 注入）
//   - "bot.id": string — Bot 标识
//
// Envelope KV 产出：
//   - "system.prompt": string — 组装后的完整 system prompt
//   - "system.prompt.sections_used": []string — 参与组装的段落名称
//   - "system.prompt.length": int — prompt 字符长度
//
// 旁路事件：
//   - prompt.assembled: 组装完成（含段落/变量统计）
type PromptStage struct {
	name      string
	assembler *Assembler
	config    PromptStageConfig
	tracer    trace.Tracer
	logger    *zap.SugaredLogger
}

// PromptStageConfig 配置 PromptStage。
type PromptStageConfig struct {
	// BaseSectionName 基础 prompt 段落名称。
	// 如果 Registry 中不存在此名称的段落，PromptStage 会从 BotConfig.SystemPrompt
	// 自动创建一个 Order=0 的基础段落注入（向后兼容）。
	// 默认值: "identity"
	BaseSectionName string

	// InjectMemoryContext 是否自动将 "memory.context" 注入为一个临时段落。
	// 默认 true。如果为 false，需要手动在 Registry 中注册一个使用
	// {{.MemoryContext}} 变量的 Section。
	InjectMemoryContext bool

	// MemorySectionOrder 记忆段落的 Order（默认 200）。
	MemorySectionOrder int

	// FallbackToConfig 当 Registry 为空时，是否回退到 BotConfig.SystemPrompt。
	// 默认 true。
	FallbackToConfig bool
}

// DefaultPromptStageConfig 返回默认配置。
func DefaultPromptStageConfig() PromptStageConfig {
	return PromptStageConfig{
		BaseSectionName:    "identity",
		InjectMemoryContext: true,
		MemorySectionOrder: 200,
		FallbackToConfig:   true,
	}
}

// NewPromptStage 创建系统提示词组装 Stage。
func NewPromptStage(
	name string,
	assembler *Assembler,
	config PromptStageConfig,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *PromptStage {
	if name == "" {
		name = "prompt"
	}
	return &PromptStage{
		name:      name,
		assembler: assembler,
		config:    config,
		tracer:    tp.Tracer("github.com/kasuganosora/thinkbot/agent/prompt"),
		logger:    logger.With("component", "prompt_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *PromptStage) Name() string { return s.name }

// Process 组装 system prompt 并注入 Envelope。
func (s *PromptStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.prompt.assemble",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.channel", env.Message.Channel),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, s.logger)

	// 构建 AssemblyContext
	asmCtx := s.buildAssemblyContext(env)

	// 准备临时段落
	extraSections := s.buildExtraSections(env, asmCtx)

	// 执行组装
	result, err := s.assembler.Assemble(asmCtx, extraSections...)
	if err != nil {
		span.RecordError(err)
		logger.Errorw("prompt assembly failed",
			"message_id", env.Message.ID,
			"err", err)
		// 尝试 fallback
		if s.config.FallbackToConfig {
			fallback := s.fallbackPrompt(env)
			if fallback != "" {
				env.Set("system.prompt", fallback)
				span.SetAttributes(attribute.Bool("prompt.fallback", true))
			logger.Warnw("using fallback prompt from BotConfig",
				"message_id", env.Message.ID)
				return env, nil
			}
		}
		return env, &core.PipelineError{
			Stage:   s.name,
			Message: "system prompt assembly failed",
			Cause:   err,
		}
	}

	// 注入 Envelope KV
	env.Set("system.prompt", result.Prompt)
	env.Set("system.prompt.sections_used", result.SectionsUsed)
	env.Set("system.prompt.length", result.PromptLength)

	span.SetAttributes(
		attribute.Int("prompt.length", result.PromptLength),
		attribute.Int("prompt.sections_used", len(result.SectionsUsed)),
		attribute.Int("prompt.sections_skipped", len(result.SectionsSkipped)),
		attribute.Int("prompt.vars_resolved", result.VariablesResolved),
		attribute.Int("prompt.vars_failed", result.VariablesFailed),
		attribute.Bool("prompt.truncated", result.Truncated),
	)

	logger.Debugw("system prompt assembled",
		"message_id", env.Message.ID,
		"length", result.PromptLength,
		"sections_used", result.SectionsUsed,
		"sections_skipped", result.SectionsSkipped,
		"vars_resolved", result.VariablesResolved,
		"truncated", result.Truncated)

	// 旁路事件
	emitter := outbound.EmitterFromContext(ctx)
	emitter.Emit(ctx, "prompt.assembled", env.Message.TraceID, map[string]any{
		"length":           result.PromptLength,
		"sections_used":    result.SectionsUsed,
		"sections_skipped": result.SectionsSkipped,
		"vars_resolved":    result.VariablesResolved,
		"vars_failed":      result.VariablesFailed,
		"truncated":        result.Truncated,
	})

	return env, nil
}

// buildAssemblyContext 从 Envelope 构建组装上下文。
func (s *PromptStage) buildAssemblyContext(env *core.Envelope) *AssemblyContext {
	// 收集 Envelope KV 快照：
	// 1. 收集已知的上游 KV keys（memory/bot 相关）
	// 2. 收集 Registry 中所有 Section.Variables 声明的 EnvelopeKey
	values := make(map[string]any)

	// 已知的上游 KV keys
	knownKeys := []string{
		"memory.context",
		"memory.entries_used",
		"memory.compressed",
		"bot.config",
		"bot.id",
	}
	for _, key := range knownKeys {
		if v, ok := env.Get(key); ok {
			values[key] = v
		}
	}

	// 收集 Section 中 Variable 声明的所有 EnvelopeKey
	sections := s.assembler.registry.List()
	for _, sec := range sections {
		for _, v := range sec.Variables {
			if v.Source == SourceEnvelopeKV && v.EnvelopeKey != "" {
				if val, ok := env.Get(v.EnvelopeKey); ok {
					values[v.EnvelopeKey] = val
				}
			}
		}
	}

	// 获取 BotID
	botID := ""
	if v, ok := env.Get("bot.id"); ok {
		if s, ok := v.(string); ok {
			botID = s
		}
	}
	if botID == "" {
		botID = env.Message.BotID
	}

	return &AssemblyContext{
		Values:    values,
		BotID:     botID,
		Channel:   env.Message.Channel,
		ChatType:  env.Message.ChatType,
		UserID:    env.Message.UserID,
		Timestamp: time.Now(),
	}
}

// buildExtraSections 构建临时段落（不注册到 Registry）。
func (s *PromptStage) buildExtraSections(env *core.Envelope, ctx *AssemblyContext) []Section {
	var extra []Section

	// 自动注入 BotConfig.SystemPrompt 作为基础段落（如果 Registry 中没有）
	if _, ok := s.assembler.registry.Get(s.config.BaseSectionName); !ok {
		basePrompt := s.fallbackPrompt(env)
		if basePrompt != "" {
			extra = append(extra, Section{
				Name:    s.config.BaseSectionName,
				Order:   0,
				Content: basePrompt,
				Enabled: true,
			})
		}
	}

	// 自动注入记忆上下文段落
	if s.config.InjectMemoryContext {
		memCtx := ctx.GetString("memory.context")
		if memCtx != "" {
			extra = append(extra, Section{
				Name:    "memory_context",
				Order:   s.config.MemorySectionOrder,
				Content: memCtx,
				Enabled: true,
			})
		}
	}

	return extra
}

// fallbackPrompt 从 BotConfig 获取基础 prompt。
func (s *PromptStage) fallbackPrompt(env *core.Envelope) string {
	v, ok := env.Get("bot.config")
	if !ok {
		return ""
	}
	// BotConfig 可能以值或指针方式存储
	switch cfg := v.(type) {
	case interface{ GetSystemPrompt() string }:
		return cfg.GetSystemPrompt()
	default:
		// 尝试通过反射或类型断言访问 SystemPrompt 字段
		// 为简单起见，直接尝试常见结构
		if m, ok := v.(map[string]any); ok {
			if sp, ok := m["systemPrompt"]; ok {
				if s, ok := sp.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// Assembler 返回 Stage 使用的组装器（便于外部访问 Registry 或 Metrics）。
func (s *PromptStage) Assembler() *Assembler {
	return s.assembler
}
