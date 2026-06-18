package memory

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
)

// ============================================================================
// MemoryStage — Pipeline 记忆集成 Stage
// ============================================================================

// MemoryStage 是一个 Pipeline Stage，在消息处理过程中：
//  1. [读取] 从 Repository 检索与当前消息相关的记忆
//  2. [注入] 将格式化后的记忆上下文注入 Envelope KV（供下游 Stage 使用）
//
// MemoryStage 通常放在 Pipeline 靠前的位置（如 Order=100），在 LLM 调用之前完成。
// 下游的 ReplyStage/LLMStage 可以从 Envelope.Get("memory.context") 获取记忆上下文，
// 拼入 system prompt 或 messages 中。
//
// 旁路事件：
//   - memory.retrieved: 检索完成（含命中数量和耗时）
//
// Envelope KV 注入：
//   - "memory.context": string — 格式化后的记忆上下文文本
//   - "memory.entries": []Entry — 原始记忆条目（供高级 Stage 使用）
//
// 使用示例：
//
//	memStage := memory.NewMemoryStage("memory", repo, memory.MemoryStageConfig{
//	    Context: memory.DefaultContextManagerConfig(),
//	})
//	pipeline.AddStage(core.StageInfo{Stage: memStage, Order: 100, Enabled: true})
type MemoryStage struct {
	name    string
	mgr     *ContextManager
	repo    Repository
	config  MemoryStageConfig
	tracer  trace.Tracer
	logger  *zap.SugaredLogger
}

// MemoryStageConfig 配置记忆 Stage。
type MemoryStageConfig struct {
	// Context 上下文管理器配置。
	Context ContextManagerConfig
	// Builder 上下文格式化配置。
	Builder ContextBuilderConfig
}

// NewMemoryStage 创建记忆 Pipeline Stage。
func NewMemoryStage(
	name string,
	repo Repository,
	config MemoryStageConfig,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *MemoryStage {
	builder := NewContextBuilder(config.Builder)
	mgr := NewContextManager(repo, builder, config.Context)

	return &MemoryStage{
		name:   name,
		mgr:    mgr,
		repo:   repo,
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory"),
		logger: logger.With("component", "memory_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *MemoryStage) Name() string { return s.name }

// Process 从记忆中检索上下文并注入到 Envelope。
func (s *MemoryStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.memory.process",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.channel", env.Message.Channel),
			attribute.String("message.user_id", env.Message.UserID),
		))
	defer span.End()

	start := time.Now()

	// 组装记忆上下文
	contextText, err := s.mgr.AssembleContext(
		ctx,
		env.Message.Channel,
		env.Message.UserID,
		env.Message.Text,
	)
	if err != nil {
		// 记忆检索失败不应阻塞消息处理，降级为无记忆继续
		s.logger.Warnw("memory retrieval failed, proceeding without context",
			"message_id", env.Message.ID,
			"err", err)
		span.RecordError(err)
		return env, nil
	}

	duration := time.Since(start)

	// 注入到 Envelope KV
	if contextText != "" {
		env.Set("memory.context", contextText)
		span.SetAttributes(
			attribute.Int("memory.context_len", len(contextText)),
			attribute.Int64("memory.duration_ms", duration.Milliseconds()),
		)
		s.logger.Debugw("memory context injected",
			"message_id", env.Message.ID,
			"context_len", len(contextText),
			"duration", duration)
	} else {
		span.SetAttributes(attribute.Bool("memory.empty", true))
	}

	// 旁路事件：记忆检索完成
	emitter := outbound.EmitterFromContext(ctx)
	emitter.Emit(ctx, "memory.retrieved", env.Message.TraceID, map[string]any{
		"context_len": len(contextText),
		"duration_ms": duration.Milliseconds(),
		"has_context": contextText != "",
	})

	return env, nil
}

// ============================================================================
// MemoryWriteStage — Pipeline 记忆写入 Stage
// ============================================================================

// MemoryWriteStage 是一个 Pipeline Stage，在消息处理的后期阶段：
// 检查 Envelope 中是否有需要写入记忆的内容（如 ActionNote 产出的备注），
// 并将其转存为 Memory Entry。
//
// MemoryWriteStage 通常放在 Pipeline 靠后的位置（如 Order=900），
// 在 ReplyStage/决策 Stage 之后执行。
//
// 它会检查 Envelope 中累积的 ActionNote 并将备注文本转为记忆条目存储。
// 这使得 NoteHandler（outbound 侧）负责立即持久化备注，
// 而 MemoryWriteStage（pipeline 侧）负责将备注转化为可检索的长期记忆。
//
// Envelope KV 注入：
//   - "memory.written": int — 本次写入的记忆条目数
//
// 旁路事件：
//   - memory.written: 记忆写入完成
type MemoryWriteStage struct {
	name   string
	store  Store
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewMemoryWriteStage 创建记忆写入 Stage。
func NewMemoryWriteStage(
	name string,
	store Store,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *MemoryWriteStage {
	return &MemoryWriteStage{
		name:   name,
		store:  store,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory"),
		logger: logger.With("component", "memory_write_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *MemoryWriteStage) Name() string { return s.name }

// Process 将 Envelope 中的 ActionNote 转存为记忆条目。
func (s *MemoryWriteStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.memory_write.process",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
		))
	defer span.End()

	// 提取 ActionNote 类型的 action
	actions := env.Actions()
	var written int

	for _, action := range actions {
		if action.Type != core.ActionNote {
			continue
		}

		text, ok := action.Payload.(string)
		if !ok || text == "" {
			continue
		}

		// 确定存储 scope
		scope := ChannelScope(env.Message.Channel)
		if env.Message.Channel == "" {
			scope = UserScope(env.Message.UserID)
		}

		// 提取分类
		category := "observation"
		if action.Metadata != nil {
			if c, ok := action.Metadata["category"]; ok {
				if cs, ok := c.(string); ok && cs != "" {
					category = cs
				}
			}
		}

		entry := Entry{
			Scope:      scope,
			Content:    text,
			Category:   category,
			Source:     "note",
			Importance: 0.5, // 默认中等重要度
			Metadata: map[string]any{
				"message_id": env.Message.ID,
				"user_id":    env.Message.UserID,
				"bot_id":     env.Message.BotID,
			},
		}

		if err := s.store.Append(ctx, entry); err != nil {
			s.logger.Warnw("memory write failed",
				"message_id", env.Message.ID,
				"err", err)
			span.RecordError(err)
			// 写入失败不阻塞 pipeline
			continue
		}

		written++
	}

	if written > 0 {
		env.Set("memory.written", written)
		span.SetAttributes(attribute.Int("memory.written", written))

		s.logger.Debugw("memory entries written",
			"message_id", env.Message.ID,
			"count", written)

		// 旁路事件
		emitter := outbound.EmitterFromContext(ctx)
		emitter.Emit(ctx, "memory.written", env.Message.TraceID, map[string]any{
			"count": written,
		})
	}

	return env, nil
}
