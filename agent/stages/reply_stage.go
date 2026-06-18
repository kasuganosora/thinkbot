package stages

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// OutputDecision — LLM 输出决策
// ============================================================================

// OutputDecision 描述 LLM 对一条消息的处理决策。
type OutputDecision string

const (
	// DecisionReply 正常回复：发送消息到 Channel。
	DecisionReply OutputDecision = "reply"
	// DecisionReplyWithNote 回复 + 备注：发送消息到 Channel 并记录内部笔记。
	DecisionReplyWithNote OutputDecision = "reply_with_note"
	// DecisionNoteOnly 只备注不回复：不发送任何消息，只记录内部笔记。
	// 适用于场景：群聊中 Bot 被 @ 但 LLM 判断不适合回复（如闲聊、无关话题），
	// 或者 Bot 观察到有价值的信息但不需要参与对话。
	DecisionNoteOnly OutputDecision = "note_only"
	// DecisionCallback 执行回调：将结果回传给任务发起方（sub-agent 场景）。
	DecisionCallback OutputDecision = "callback"
	// DecisionSilent 主动静默：什么都不做，仅记录 trace 表达"已知晓但不回应"。
	DecisionSilent OutputDecision = "silent"
	// DecisionDrop 完全跳过：既不回复也不备注。
	DecisionDrop OutputDecision = "drop"
)

// ============================================================================
// ReplyDecider — 决策函数类型
// ============================================================================

// ReplyDecider 是一个决策函数，根据 LLM 结果决定输出模式。
// 返回值：
//   - decision: 输出决策
//   - replyText: 回复文本（DecisionReply / DecisionReplyWithNote 时使用）
//   - noteText: 备注文本（DecisionReplyWithNote / DecisionNoteOnly 时使用）
//   - noteCategory: 备注分类
//
// 实现可以：
//   - 解析 LLM 输出中的结构化标记（如 JSON block 或特殊前缀）
//   - 调用独立的分类模型
//   - 使用规则引擎做简单判断
type ReplyDecider func(ctx context.Context, msg core.Message, result *llm.GenerateResult) (
	decision OutputDecision, replyText string, noteText string, noteCategory string,
)

// ============================================================================
// ReplyStage — 智能回复 Stage（支持多种输出决策）
// ============================================================================

// ReplyStage 是一个完整的 LLM 回复 Stage，支持三种输出模式：
//
//  1. 正常回复（DecisionReply）：产出 ActionReply → 发送到 Channel
//  2. 回复 + 备注（DecisionReplyWithNote）：产出 ActionReply + ActionNote
//  3. 只备注不回复（DecisionNoteOnly）：只产出 ActionNote → 记录但不打扰用户
//
// ReplyStage 在 LLMStage 基础上增加了：
//   - 输出决策机制（通过 ReplyDecider）
//   - 正确的 source_channel 设置（ChannelReplyHandler 路由必需）
//   - 正确的 reply_target 使用（统一 Channel 语义）
//   - ActionNote 支持（为后续记忆模块铺路）
//
// 使用示例：
//
//	stage := stages.NewReplyStage("llm-reply", provider, stages.ReplyStageConfig{
//	    LLM: stages.LLMConfig{SystemPrompt: "..."},
//	    Decider: myDeciderFunc,
//	})
type ReplyStage struct {
	name     string
	provider llm.Provider
	config   ReplyStageConfig
	tracer   trace.Tracer
	logger   *zap.SugaredLogger
}

// ReplyStageConfig 配置 ReplyStage。
type ReplyStageConfig struct {
	// LLM LLM 调用配置（复用 LLMConfig）。
	LLM LLMConfig

	// Decider 输出决策函数。
	// 如果为 nil，使用默认决策：始终回复（DecisionReply）。
	Decider ReplyDecider

	// DefaultNoteCategory 默认备注分类。
	DefaultNoteCategory string
}

// NewReplyStage 创建智能回复 Stage。
func NewReplyStage(name string, provider llm.Provider, config ReplyStageConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *ReplyStage {
	if name == "" {
		name = "reply"
	}
	return &ReplyStage{
		name:     name,
		provider: provider,
		config:   config,
		tracer:   tp.Tracer("github.com/kasuganosora/thinkbot/agent/stages/reply"),
		logger:   logger,
	}
}

// Name 返回 Stage 名称。
func (s *ReplyStage) Name() string { return s.name }

// Process 调用 LLM 生成回复并根据决策函数产出对应 Action 组合。
func (s *ReplyStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.reply.process",
		trace.WithAttributes(
			attribute.String("llm.provider", s.provider.Name()),
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.source", env.Message.Source),
		))
	defer span.End()

	// 构建 LLM 消息
	var messages []llm.Message
	if s.config.LLM.MessageBuilder != nil {
		messages = s.config.LLM.MessageBuilder(env.Message)
	} else {
		messages = []llm.Message{llm.UserMessage(env.Message.Text)}
	}

	// 解析 system prompt：优先从 Envelope KV 读取动态组装的 prompt（PromptStage 注入），
	// 回退到 LLMConfig.SystemPrompt 静态配置（向后兼容）。
	systemPrompt := s.config.LLM.SystemPrompt
	if v, ok := env.Get("system.prompt"); ok {
		if sp, ok := v.(string); ok && sp != "" {
			systemPrompt = sp
		}
	}

	// 调用 LLM
	params := llm.GenerateParams{
		Model:       s.config.LLM.Model,
		System:      systemPrompt,
		Messages:    messages,
		Tools:       s.config.LLM.Tools,
		Temperature: s.config.LLM.Temperature,
		MaxTokens:   s.config.LLM.MaxTokens,
	}

	cfg := &llm.OrchestrateConfig{
		Params:   params,
		MaxSteps: s.config.LLM.MaxSteps,
	}

	s.logger.Debugw("reply stage: calling LLM",
		"message_id", env.Message.ID,
		"provider", s.provider.Name())

	result, err := llm.OrchestrateGenerate(ctx, s.provider, cfg)
	if err != nil {
		span.RecordError(err)
		s.logger.Errorw("reply stage: LLM failed",
			"message_id", env.Message.ID, "err", err)
		return env, &core.PipelineError{
			Stage:   s.name,
			Message: "LLM generation failed",
			Cause:   err,
		}
	}

	span.SetAttributes(
		attribute.Int("llm.steps", len(result.Steps)),
		attribute.Int("llm.total_tokens", result.Usage.TotalTokens),
		attribute.String("llm.finish_reason", string(result.FinishReason)),
	)

	// 存储 LLM 结果到 Envelope KV
	env.Set("llm.result", result)

	// 执行决策
	decision, replyText, noteText, noteCategory := s.decide(ctx, env.Message, result)
	span.SetAttributes(attribute.String("output.decision", string(decision)))

	s.logger.Infow("reply stage: decision made",
		"message_id", env.Message.ID,
		"decision", decision,
		"reply_len", len(replyText),
		"note_len", len(noteText))

	// 解析公共 outbound 参数
	replyTarget := resolveReplyTarget(env.Message)
	sourceChannel := env.Message.Source

	// 根据决策产出 Action
	switch decision {
	case DecisionReply:
		s.addReplyAction(env, replyTarget, sourceChannel, replyText, result)

	case DecisionReplyWithNote:
		s.addReplyAction(env, replyTarget, sourceChannel, replyText, result)
		s.addNoteAction(env, sourceChannel, noteText, noteCategory)

	case DecisionNoteOnly:
		s.addNoteAction(env, sourceChannel, noteText, noteCategory)

	case DecisionCallback:
		s.addCallbackAction(env, sourceChannel, replyText, noteCategory)

	case DecisionSilent:
		s.addSilentAction(env, sourceChannel, noteText)

	case DecisionDrop:
		s.logger.Debugw("reply stage: decision is drop, no actions",
			"message_id", env.Message.ID)
	}

	return env, nil
}

// decide 执行决策逻辑。如果没有自定义 Decider，默认始终回复。
func (s *ReplyStage) decide(ctx context.Context, msg core.Message, result *llm.GenerateResult) (
	OutputDecision, string, string, string,
) {
	if s.config.Decider != nil {
		return s.config.Decider(ctx, msg, result)
	}
	// 默认决策：始终回复
	return DecisionReply, result.Text, "", ""
}

// addReplyAction 添加回复 Action 到 Envelope。
func (s *ReplyStage) addReplyAction(env *core.Envelope, replyTarget, sourceChannel, text string, result *llm.GenerateResult) {
	if text == "" {
		s.logger.Warnw("reply stage: empty reply text, skipping reply action",
			"message_id", env.Message.ID,
			"finish_reason", string(result.FinishReason))
		return
	}
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: replyTarget,
		UserID:  env.Message.UserID,
		Payload: text,
		Metadata: map[string]any{
			"source_channel": sourceChannel,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"finish_reason":  string(result.FinishReason),
			"usage":          result.Usage,
			"steps":          len(result.Steps),
		},
	})
}

// addNoteAction 添加备注 Action 到 Envelope。
func (s *ReplyStage) addNoteAction(env *core.Envelope, sourceChannel, text, category string) {
	if text == "" {
		return
	}
	if category == "" {
		category = s.config.DefaultNoteCategory
	}
	if category == "" {
		category = "observation"
	}
	env.AddAction(core.Action{
		Type:    core.ActionNote,
		Channel: env.Message.Channel, // 会话空间标识（用于记忆关联）
		UserID:  env.Message.UserID,
		Payload: text,
		Metadata: map[string]any{
			"source_channel": sourceChannel,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"category":       category,
		},
	})
}

// addCallbackAction 添加回调 Action 到 Envelope。
// payload 作为回调结果数据，category 作为回调状态使用。
func (s *ReplyStage) addCallbackAction(env *core.Envelope, sourceChannel, payload, status string) {
	// 从 Envelope KV 或 Metadata 中查找 callback_id
	callbackID := ""
	if env.Message.Metadata != nil {
		if id, ok := env.Message.Metadata["callback_id"]; ok {
			if idStr, ok := id.(string); ok {
				callbackID = idStr
			}
		}
	}
	// 也尝试从 Envelope values 中取（Pipeline 中间 Stage 可能设置）
	if callbackID == "" {
		if v, ok := env.Get("callback_id"); ok {
			if idStr, ok := v.(string); ok {
				callbackID = idStr
			}
		}
	}

	if status == "" {
		status = "success"
	}

	env.AddAction(core.Action{
		Type:    core.ActionCallback,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: payload,
		Metadata: map[string]any{
			"source_channel": sourceChannel,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"callback_id":    callbackID,
			"status":         status,
		},
	})
}

// addSilentAction 添加静默 Action 到 Envelope。
// reason 记录静默原因（供 trace/分析使用）。
func (s *ReplyStage) addSilentAction(env *core.Envelope, sourceChannel, reason string) {
	if reason == "" {
		reason = "llm_decision"
	}
	env.AddAction(core.Action{
		Type:    core.ActionSilent,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Metadata: map[string]any{
			"source_channel": sourceChannel,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"reason":         reason,
		},
	})
}

// resolveReplyTarget 从消息中解析 outbound 回复目标。
// 优先使用 Metadata["reply_target"]，回退到 msg.Channel。
func resolveReplyTarget(msg core.Message) string {
	if msg.Metadata != nil {
		if rt, ok := msg.Metadata["reply_target"]; ok {
			if s, ok := rt.(string); ok && s != "" {
				return s
			}
		}
	}
	return msg.Channel
}

// ============================================================================
// DefaultDecider — 基于文本前缀的简单决策函数
// ============================================================================

// PrefixDecider 是一个基于 LLM 输出前缀的简单决策函数。
// LLM 在 system prompt 中被指示使用特定前缀标记输出类型：
//
//	[REPLY] 正常回复的文本
//	[NOTE] 只记录的备注
//	[REPLY+NOTE] 分隔符前是回复，分隔符后是备注
//	[SKIP] 不做任何事
//
// 如果没有前缀，默认为 DecisionReply（全文作为回复）。
func PrefixDecider(_ context.Context, _ core.Message, result *llm.GenerateResult) (
	OutputDecision, string, string, string,
) {
	text := result.Text
	if text == "" {
		return DecisionDrop, "", "", ""
	}

	// [SKIP] 不做任何事
	if strings.HasPrefix(text, "[SKIP]") {
		return DecisionDrop, "", "", ""
	}

	// [NOTE] 只备注
	if strings.HasPrefix(text, "[NOTE]") {
		noteText := strings.TrimPrefix(text, "[NOTE]")
		category := "observation"
		return DecisionNoteOnly, "", strings.TrimSpace(noteText), category
	}

	// [REPLY+NOTE] 回复 + 备注（用 [---] 分割）
	if strings.HasPrefix(text, "[REPLY+NOTE]") {
		body := strings.TrimPrefix(text, "[REPLY+NOTE]")
		// 查找分隔符
		sepIdx := strings.Index(body, "[---]")
		if sepIdx >= 0 {
			replyText := body[:sepIdx]
			noteText := body[sepIdx+5:]
			return DecisionReplyWithNote, strings.TrimSpace(replyText), strings.TrimSpace(noteText), "insight"
		}
		// 没有分隔符，全文作为回复
		return DecisionReply, strings.TrimSpace(body), "", ""
	}

	// [REPLY] 正常回复
	if strings.HasPrefix(text, "[REPLY]") {
		return DecisionReply, strings.TrimSpace(strings.TrimPrefix(text, "[REPLY]")), "", ""
	}

	// 无前缀：默认回复
	return DecisionReply, text, "", ""
}

// SystemPromptWithDecision 是一个辅助函数，将决策指令附加到 system prompt 末尾。
// 用于配合 PrefixDecider 使用。
func SystemPromptWithDecision(basePrompt string) string {
	return basePrompt + fmt.Sprintf(`

## Output Format Rules

You MUST prefix your response with ONE of these tags to indicate your decision:

- [REPLY] — Normal reply to the user. Use this when you have something meaningful to say.
- [NOTE] — Internal note only. The user will NOT see this. Use when you observe something worth remembering but don't need to respond (e.g., the conversation doesn't need your input, or the topic is irrelevant to you).
- [REPLY+NOTE] — Reply to the user AND record a private note. Separate them with [---]. Use when you want to reply but also remember something for later.
- [SKIP] — Do nothing. Use when the message is completely irrelevant.

Examples:
- "[REPLY] Sure, I can help with that!"
- "[NOTE] User seems frustrated about the deployment. Keep this in mind for future interactions."
- "[REPLY+NOTE] Here's the code fix you asked for.[---]User's codebase uses Go 1.21 with generics heavily."
- "[SKIP]"

IMPORTANT: Always include the prefix tag. If unsure, default to [REPLY].
`)
}
