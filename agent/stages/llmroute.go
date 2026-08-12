package stages

import (
	"context"
	"errors"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/session"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
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
	PublishToolCall(ctx context.Context, traceID, botID, toolCallID, toolName string, input any)
	PublishToolProgress(ctx context.Context, traceID, botID, toolCallID, toolName string, invocationID string, payload any)
	PublishToolResult(ctx context.Context, traceID, botID, toolCallID, toolName string, invocationID string, output any, errMsg string)
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

// chatSessionIDFromEnvelope 取前端会话 ID（web渠道在注入消息时写进 metadata）。
// 非 web 渠道没有这个概念，返回空串。
func chatSessionIDFromEnvelope(env *core.Envelope) string {
	if env == nil || env.Message.Metadata == nil {
		return ""
	}
	if sid, ok := env.Message.Metadata[agenttools.ExtraKeyChatSessionID].(string); ok {
		return sid
	}
	return ""
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

// replySuppressed 检查上游 Stage 是否要求本轮不要发送回复。
//
// 返回 (是否抑制, 原因)。原因仅用于日志与 trace —— 静默丢弃必须可解释，
// 否则运维会把「有意不回复」误判成 Bot 故障。
func replySuppressed(env *core.Envelope) (bool, string) {
	v, ok := env.Get(core.KVSuppressReply)
	if !ok {
		return false, ""
	}
	b, ok := v.(bool)
	if !ok || !b {
		return false, ""
	}
	reason := "unspecified"
	if rv, ok := env.Get(core.KVSuppressReplyReason); ok {
		if s, ok := rv.(string); ok && s != "" {
			reason = s
		}
	}
	return true, reason
}

// isLurkMode 判断当前消息是否处于潜水（只读）渠道。
func isLurkMode(env *core.Envelope) bool {
	if v, ok := env.Get(core.KVLurkMode); ok {
		if b, ok := v.(bool); ok && b {
			return true
		}
	}
	return false
}

// lurkObserverInstruction 是潜水观察者模式的系统提示词后缀（英文，遵循 LLM 提示词约定）。
// 它把 LLM 的产出从「回复」重新导向「从帖子里学到什么」，并要求无价值时显式输出 [NONE]。
const lurkObserverInstruction = `[OBSERVER MODE — LURK / READ-ONLY]
You are in lurk mode. You are silently observing a public social timeline and you will NOT send any reply to anyone. No message leaves this session.
Your job is to learn, not to respond. Analyze the post you just read through the lens of your own identity and values (defined above). Decide whether it contains anything worth remembering for future interaction with this person or community, for example:
- the speaker's technical preferences, stack, or current projects
- explicit needs, questions, or requests
- mood, relationship, or how they expect you to help
- anything that would make you more useful next time
If something is worth keeping, write a concise first-person internal note (for your own future reference — never sent). If there is nothing worth remembering, output exactly: [NONE]`

// buildLurkPrompt 构建潜水观察者 prompt：优先用 SOUL.md 人格内容，否则回退到已注入的
// base prompt，再拼接观察者指令。保证「结合 soul.md 模块分析」。
func (s *LLMStage) buildLurkPrompt(env *core.Envelope, basePrompt string) string {
	if v, ok := env.Get(core.KVSoulContent); ok {
		if soul, ok := v.(string); ok {
			if sc := strings.TrimSpace(soul); sc != "" {
				return sc + "\n\n" + lurkObserverInstruction
			}
		}
	}
	if bp := strings.TrimSpace(basePrompt); bp != "" {
		return bp + "\n\n" + lurkObserverInstruction
	}
	return lurkObserverInstruction
}

// lurkSkipMarkers 为「无笔记」语义标记集合。LLM 在认为没有值得记的内容时可能返回
// [NONE] / [无] / [没有] / 暂无 等标记。归一化（去首尾空格与常见标点、转小写）后
// 精确匹配任一标记即视为空笔记，跳过写入，避免把「无」当有效记忆污染工作记忆。
var lurkSkipMarkers = []string{
	"[none]", "none", "n/a", "na", "null", "nil",
	"无", "[无]", "（无）", "(无)", "没有", "[没有]", "暂无", "无内容", "无笔记", "无东西可记",
	"nothing", "skip", "pass",
}

// isEmptyLurkNote 判断 LLM 潜水产出是否为「无笔记」语义（空串或语义空标记）。
func isEmptyLurkNote(note string) bool {
	s := strings.ToLower(strings.TrimSpace(note))
	s = strings.Trim(s, "。.，,、()（）[]【】\"' \t\n")
	if s == "" {
		return true
	}
	for _, m := range lurkSkipMarkers {
		if s == m {
			return true
		}
	}
	return false
}

// emitLurkNote 在潜水模式下把 LLM 的思考结果作为内部学习笔记（ActionNote）写入 L0。
// 仅当模型产出有价值内容（非 [NONE]/[无]/[没有] 等空语义标记、且非空）时才发 ActionNote，避免污染工作记忆。
func (s *LLMStage) emitLurkNote(env *core.Envelope, result *llm.GenerateResult) {
	note := memory.StripThinking(result.Text)
	if isEmptyLurkNote(note) {
		s.logger.Debugw("lurk: nothing worth remembering, skip note",
			"message_id", env.Message.ID)
		return
	}
	env.AddAction(core.Action{
		Type:    core.ActionNote,
		Channel: "", // 潜水学习笔记以 bot 全局 scope 落库（note_handler 据 bot_id 判 scope），跨渠道可用
		UserID:  env.Message.UserID,
		Payload: note,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"category":       "lurk",
		},
	})
	s.logger.Infow("lurk: learning note captured to L0",
		"message_id", env.Message.ID, "note_len", len(note))
}

// ============================================================================
// LLMStage — 调用 LLM Provider 生成回复
// ============================================================================

// LLMConfig 配置 LLM Stage。
type LLMConfig struct {
	// SystemPrompt 系统提示词。
	SystemPrompt string
	// MaxSteps Orchestrate 软预算步数（0=单次, >0=多步, -1=无限）。
	// 复杂任务在持续推进时可自动延长至 HardMaxSteps，详见 llm.loopController。
	MaxSteps int
	// HardMaxSteps Orchestrate 硬上限步数（绝对天花板，仅 MaxSteps>0 时生效）。
	// <=0 表示自动取 MaxSteps*3。
	HardMaxSteps int
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

	// ApprovalHandler 可选的工具审批处理器（HITL 门禁）。
	// 非 nil 时，标记了 RequireApproval 的工具在执行前会调用此处理器决策
	// （approved/rejected/deferred）。为 nil 时不做审批拦截——这是当前默认，
	// 因为 thinkbot 均运行在 Docker 沙箱中，危险操作影响被沙箱边界限制。
	// 框架层保留此注入点，便于将来在交互式渠道（如 Web）接入确认流。
	ApprovalHandler func(ctx context.Context, call llm.ToolCall) (llm.ToolApprovalResult, error)

	// ToolDeferral 可选的延迟加载工具管理器（Claude 风格 defer_loading）。
	// 非 nil 时，标记了 DeferredLoad 的工具初始仅向模型暴露名称 + 描述，
	// 完整 input schema 经注入的 tool_search 工具或「模型直接引用」按需加载，
	// 从而节省 token 并减少工具选择错误。其状态按会话（session）隔离，
	// 使已发现的工具在同一会话内跨轮持久可用，且不会串扰到其它并发会话。
	ToolDeferral *llm.DeferralStore
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

	// 注入工具调用来源（bot + 前端会话）。工具是**静态注册**的、自身拿不到会话，
	// 需要按会话归属做事的工具从 context 读取它 —— 例如工作流提交要记录来源会话，
	// 好让前端刷新页面后把工作流卡片恢复出来。
	ctx = agenttools.ContextWithCallOrigin(ctx, agenttools.CallOrigin{
		BotID:     env.Message.BotID,
		SessionID: chatSessionIDFromEnvelope(env),
	})

	// 注入「直接回复语境」标记：对方 @ 了 Bot 或回复了 Bot（Mentioned=true）时，
	// Channel 工具可据此禁用「手动发孤立帖」类能力（如 misskey_create_note），
	// 强制走框架自动串接回复，避免重复发文。该值在 Orchestrate 全程透传到工具执行。
	ctx = llm.WithDirectReply(ctx, env.Message.Mentioned)

	// 注入更宽的「入站回复语境」标记：框架对**任何**带 reply_target 的入站帖
	// 都会自动串接回复（含未 @ Bot 的普通 timeline 帖）。此时若 Channel 工具再手动
	// 发一条新帖，同一条入站帖就会收到两条发文。据此让工具层禁用「手动发孤立帖」，
	// 强制走框架自动串接回复，避免重复发文。覆盖 IsDirectReply 未触达的未 @ 场景。
	ctx = llm.WithInboundReply(ctx, llm.InboundReply{
		Source:         env.Message.Source,
		HasReplyTarget: env.Message.Metadata["reply_target"] != "",
	})

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

	// 潜水（只读）模式：切换为「观察者」prompt —— 结合 SOUL.md 人格，把思考
	// 导向「从这条帖子里学到什么」，而非「如何回复」。仍可正常调用 LLM。
	lurkMode := isLurkMode(env)
	if lurkMode {
		// INFO 级：让「潜水模式激活」在默认日志下清晰可观测，而非只能靠下游 skip/captured 间接推断。
		logger.Infow("lurk: observing read-only channel",
			"channel", env.Message.Source,
			"platform", env.Message.Channel)
		systemPrompt = s.buildLurkPrompt(env, systemPrompt)
	} else if v, ok := env.Get(core.KVMemoryRecall); ok {
		// 非潜水模式：把召回的长期记忆（含潜水学到的经验）拼入 system prompt，
		// 让 bot 在真人交互时带「实时经验」——这是「人味」闭环的读侧。
		// 潜水模式下不注入，避免观察者自身陷入记忆回环。
		if recall, ok := v.(string); ok && recall != "" {
			systemPrompt = systemPrompt + "\n\n" + recall
		}
	}

	// 解析工具列表
	tools := resolveTools(ctx, s.config, env)
	// 潜水观察者不调用任何工具：纯推理，杜绝副作用（如经工具发帖），
	// 确保「只看不发」在工具层也成立。
	if lurkMode {
		tools = nil
	}

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
		Params:       params,
		MaxSteps:     s.config.MaxSteps,
		HardMaxSteps: s.config.HardMaxSteps,
		// 把本轮的「用户中途追加」通道透传给编排循环（Claude-CLI 风格），
		// 让生成中的用户补充能注入同一轮对话。
		InterruptCh: bot.InterruptChannelFromContext(ctx),
	}

	// 注入工具审批处理器（HITL 门禁）。为 nil 时 orchestrator 不做拦截。
	if s.config.ApprovalHandler != nil {
		cfg.ApprovalHandler = s.config.ApprovalHandler
	}

	// 注入延迟加载工具管理器（defer_loading / tool search）。为 nil 时
	// orchestrator 不做拦截。按当前会话解析各自的 ToolDeferral 实例，
	// 保证延迟加载状态在同一会话内跨轮持久、且不与其它并发会话串扰。
	if s.config.ToolDeferral != nil {
		cfg.ToolDeferral = s.config.ToolDeferral.ForSession(session.SessionIDFromEnvelope(env))
	}

	// 防偷懒门禁：环境类问题确定性强制"先调工具再作答"。
	// VerificationGateMiddleware 已在 LLMStage 之前对用户问题做确定性分类，
	// 命中时在本 Envelope 上标记 verify.required。这里把它落地为
	// OrchestrateConfig.ToolChoiceForStep：第一步强制 tool_choice=required，
	// 模型在拿到真实工具结果前无法 finalize；首次工具执行后复位为 auto，
	// 允许基于真实结果合成最终答案。无可用工具时不强制（避免 required 死循环）。
	if v, ok := env.Get("verify.required"); ok && v == true && len(tools) > 0 {
		cfg.ToolChoiceForStep = func(step int, toolsExecuted bool) any {
			if !toolsExecuted {
				return "required"
			}
			return nil
		}
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
		"hard_max_steps", s.config.HardMaxSteps,
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
			if errors.Is(err, context.Canceled) {
				logger.Debugw("llm stage: stream orchestrate canceled (client disconnected)",
					"message_id", env.Message.ID,
					"err", err)
				return env, &core.PipelineError{
					Stage:   s.name,
					Message: "LLM stream orchestrate canceled",
					Cause:   err,
				}
			}
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
			if errors.Is(err, context.Canceled) {
				logger.Debugw("llm stage: orchestrate canceled (client disconnected)",
					"message_id", env.Message.ID,
					"err", err)
				return env, &core.PipelineError{
					Stage:   s.name,
					Message: "LLM orchestrate canceled",
					Cause:   err,
				}
			}
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

	// 可观测：若编排层因工具审批被 defer（RequireApproval + ApprovalHandler
	// 返回 deferred），记录信号，便于排查“工具在等确认却没有续跑入口”的情况。
	// 当前默认无 ApprovalHandler，此分支通常不触发；保留用于将来接入 HITL。
	if result.DeferredToolApproval != nil {
		logger.Warnw("llm stage: tool approval deferred (no resume path wired yet)",
			"message_id", env.Message.ID,
			"approval_id", result.DeferredToolApproval.ApprovalID,
			"decision", result.DeferredToolApproval.Decision,
			"reason", result.DeferredToolApproval.Reason)

		// TODO(HITL): 工具审批被 defer 时，编排已暂停（工具未执行），result.Text 是
		// 半成品回复。在接入确认流之前，这里必须阻断——不能把未完成回复发给用户，
		// 也不能产生 Action。将来通过「持久化 DeferredToolApproval → 用户确认 →
		// 重新编排（携带 approval 结果）」的续跑入口恢复，而非在此直接发射。
		// 正常路径下（无 ApprovalHandler）此分支不触发，故不影响当前行为。
		env.Set("llm.result", result)
		return env, nil
	}

	// 记录使用统计
	recordUsage(ctx, s.config.UsageRecorder, env, s.config.Model, s.name, result)

	// 若 LLM 因达到输出 token 上限（length）被截断，追加提示，
	// 避免用户误以为任务已完成（实际可能只生成了半成品回复）。
	if result.FinishReason == llm.FinishReasonLength {
		result.Text += "\n\n⚠️ 提示：本次回复因达到输出 token 上限被截断，任务可能未完成。" +
			"请回复「继续」让我接着完成剩余工作。"
	}

	// 若编排循环因步数守卫（撞硬上限或陷入重复循环）而停止，追加提示，
	// 避免用户把「步数预算耗尽、Bot 主动停下」误判为卡死。实际上任务
	// 可能尚未跑完，回复「继续」即可让 Bot 接着处理剩余工作。
	if result.LoopStoppedByGuard {
		result.Text += "\n\n⚠️ 提示：本次任务因达到工具调用步数上限（" +
			result.LoopStopReason + "）被暂停，可能尚未全部完成。" +
			"请回复「继续」让我接着完成剩余工作。"
		// 归一结束原因：模型仍在 tool-calls（想继续），但本轮回合已被守卫
		// 强制结束。置为 stop 以免前端把 finish_reason=tool-calls 误判为
		// 「Bot 仍在调用工具 / 卡住」。
		result.FinishReason = llm.FinishReasonStop
	}

	// 潜水模式：只记不发 —— 把思考结果作为内部学习笔记写入 L0，绝不发帖。
	// 这一支在「回复抑制」判定之前返回：无论 engagement 是否判定发言，
	// 潜水都保持「看而学」，不产出任何 ActionReply（避免 outbound 守卫的 dropped 告警）。
	if lurkMode {
		s.emitLurkNote(env, result)
		return env, nil
	}

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

	// 抑制检查：上游（如 engagement 参与度评估）判定「此刻不该说话」时，
	// 不产出 ActionReply —— 但 LLM 已经跑完、结果仍存进 KV，
	// 供记忆写入等下游 Stage 使用。即「照样听、照样想、照样记，只是不说出口」。
	//
	// 这一步是必要的：本Stage 是全项目唯一产出 ActionReply 的地方，
	// 若不在此拦截，上游的静默决策对实际发送没有任何约束力。
	if suppressed, reason := replySuppressed(env); suppressed {
		span.SetAttributes(
			attribute.Bool("reply.suppressed", true),
			attribute.String("reply.suppress_reason", reason),
		)
		logger.Infow("reply suppressed: not sending to channel",
			"message_id", env.Message.ID,
			"reason", reason,
			"text_len", len(result.Text))
		env.Set("llm.result", result)
		return env, nil
	}

	// 清洗思考内容：部分模型（DeepSeek-R1/ GLM / QwQ 等）把推理过程以
	// <think>...</think> 内联在正文里，而非放进结构化的 Reasoning 字段。
	// 这些内容属于「心里话」，绝不能发给用户。
	// 注意：项目原先只在记忆写入侧清洗，出站链路完全没清 —— 必须在此补上。
	replyText := memory.StripThinking(result.Text)

	// 纵深防御：剥离模型从系统提示里复述出来的内部状态（记忆用量指标等），
	// 例如把 "[2,206/2,200 chars]" 写成「当前记忆已接近容量上限（2,206/2,200 字符）」
	// 公开发到时间线。思考清洗后再次过滤内部指标，确保不泄漏。
	replyText = memory.StripInternalState(replyText)
	if strings.TrimSpace(replyText) == "" {
		// 清洗后为空说明模型这轮只输出了思考内容，没有真正要说的话。
		// 此时发送空消息毫无意义（且部分 Channel 会报错），跳过发送。
		span.SetAttributes(attribute.Bool("reply.empty_after_strip", true))
		logger.Infow("reply skipped: empty after stripping thinking content",
			"message_id", env.Message.ID,
			"raw_len", len(result.Text))
		env.Set("llm.result", result)
		return env, nil
	}

	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: replyTarget,
		UserID:  env.Message.UserID,
		Payload: replyText,
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
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case part, ok := <-streamResult.Stream:
			if !ok {
				goto streamDone
			}
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
				publisher.PublishToolCall(ctx, traceID, botID, p.ToolCallID, p.ToolName, p.Input)
			case *llm.ToolProgressPart:
				publisher.PublishToolProgress(ctx, traceID, botID, p.ToolCallID, p.ToolName, p.InvocationID, p.Content)
			case *llm.StreamToolResultPart:
				result.ToolResults = append(result.ToolResults, llm.ToolResult{
					ToolCallID:   p.ToolCallID,
					ToolName:     p.ToolName,
					InvocationID: p.InvocationID,
					Output:       p.Output,
				})
				publisher.PublishToolResult(ctx, traceID, botID, p.ToolCallID, p.ToolName, p.InvocationID, p.Output, "")
			case *llm.StreamToolErrorPart:
				// 工具执行失败：把错误作为结果事件下发，使前端卡片能正常收尾
				// （error 状态），而不是永远停留在 running。
				errMsg := ""
				if p.Error != nil {
					errMsg = p.Error.Error()
				}
				result.ToolResults = append(result.ToolResults, llm.ToolResult{
					ToolCallID:   p.ToolCallID,
					ToolName:     p.ToolName,
					InvocationID: p.InvocationID,
					Output:       errMsg,
				})
				publisher.PublishToolResult(ctx, traceID, botID, p.ToolCallID, p.ToolName, p.InvocationID, nil, errMsg)
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
	}
streamDone:

	result.Steps = streamResult.Steps
	result.Messages = streamResult.Messages
	// 透传 defer 审批信号：OrchestrateStream 已将 DeferredToolApproval 挂到
	// StreamResult，但此处手动组装 GenerateResult（未走 ToResult），必须显式读回，
	// 否则流式路径下审批信号会丢失（进黑洞）。
	result.DeferredToolApproval = streamResult.DeferredToolApproval

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
