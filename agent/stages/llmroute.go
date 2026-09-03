package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

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

// messageChannelType 取消息来源平台类型（web/telegram/misskey）。
// Channel 在构建消息时写入 Metadata["channel_type"]；缺失时返回空串，
// 消费方（交互工具）按 web 兜底。
func messageChannelType(msg *core.Message) string {
	if msg == nil || msg.Metadata == nil {
		return ""
	}
	if ct, ok := msg.Metadata["channel_type"].(string); ok {
		return ct
	}
	return ""
}

// messageReplyTarget 取 outbound 回写目标。优先 Metadata["reply_target"]，
// 缺失时回退 Message.Channel（与会话空间同址的平台，如 telegram chatID）。
func messageReplyTarget(msg *core.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Metadata != nil {
		// 注意 any(nil) != "" 的类型陷阱（见上方 hasReplyTarget 注释）。
		if rt, ok := msg.Metadata["reply_target"].(string); ok && rt != "" {
			return rt
		}
	}
	return msg.Channel
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
// suppressReasonPassiveUnmentioned 是 passive（仅被动回复）模式下，
// passive-speak enricher 对非「真人显式 @」消息设置的抑制原因。
// 它表达的是「此消息未被 @，bot 无权限主动发言」的**硬策略权限**，
// 与节奏/engagement 的「此刻该不该说」软启发式不同，绝不允许被模型的
// REPLY_CONTROL send:true 放行覆盖（否则 bot 会对没 @ 它的消息发帖）。
// 必须与 api/botservice.go passive-speak enricher 写入的 reason 字符串保持一致
// （复用 core.KVSuppressReasonPassive，单一真源）。
const suppressReasonPassiveUnmentioned = core.KVSuppressReasonPassive

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

// modelExplicitlySends 轻量判定模型是否通过 REPLY_CONTROL 显式要求出站。
// 仅用于「上游节奏/engagement 门」与「模型显式意图」冲突时的优先级裁定，
// 不做完整内容清洗；解析失败一律返回 false（即不覆盖上游抑制，行为保守）。
// 解析口径与下方出站链路一致：先剥离 <think> 思考标签，再解析控制块、提取公开内容。
func modelExplicitlySends(text string) bool {
	send, clean, ok := parseReplyControl(memory.StripThinking(text))
	if !ok || !send {
		return false
	}
	return strings.TrimSpace(extractPublicReply(clean)) != ""
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

// isHeartbeatMode 判断当前消息是否为心跳自主唤醒决策（KVHeartbeatMode=true）。
// 心跳决策是机器解析的 JSON（silent/post/note + 目标），与潜水模式同理：
// 生成结果不得追加人类可读的截断/步数守卫提示语，否则会污染 JSON 导致解析失败。
func isHeartbeatMode(env *core.Envelope) bool {
	if v, ok := env.Get(core.KVHeartbeatMode); ok {
		if b, ok := v.(bool); ok && b {
			return true
		}
	}
	return false
}

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

// ============================================================================
// 回复控制门控（Reply Control Gate）
// ============================================================================
//
// 设计背景（重要，勿回退）：
// 普通回复路径原本「无条件」把 LLM 文本当作 ActionReply 发出——唯一已有的静默出口
// 是「模型纯粹输出了 <think> 思考、清洗后变空」。当模型用自然语言写下「我决定不互动」
// 的独白时，它既不是思考标签、也非空，于是被原样发到公开时间线（见 2026-08-17 故障：
// 频道从只读改为可写后，bot 把内心独白当公开回复发出）。
//
// 修复参照其他门控（lurk 的 {"remember":bool} / heartbeat 的机器解析 JSON）的同一范式：
// 让模型在回复结尾追加一个结构化的「是否出站」控制 JSON，由代码解析裁决。缺失 / 解析
// 失败 / send:false 一律不出站（fail-closed）。这与 lurk_contract.go 的「不靠自然语言
// 枚举、用定义好的布尔信号」哲学一致，且不引入任何语言相关的兜底匹配。

// replyControlDelimiter 是控制块的起始分隔符。选罕见串，避免与普通回复正文冲突。
const replyControlDelimiter = "@@REPLY_CONTROL@@"

// replyControlInstruction 是开启门控时追加到 system prompt 的协议说明（英文，遵循 LLM 约定）。
// 它把「是否出站」的决定从模型的内心独白，变成一条可被代码解析的确定性信号。
const replyControlInstruction = `[REPLY CONTROL PROTOCOL]
After your reply text, you MUST append EXACTLY ONE control line as the final output:
  @@REPLY_CONTROL@@{"send": true}    — when you want the reply above to be posted
  @@REPLY_CONTROL@@{"send": false}   — when you decide NOT to reply/interact (e.g. you observed but choose not to engage, or have nothing useful to add)
Rules:
- The control line is MANDATORY. If it is missing or malformed, NOTHING will be posted (fail-closed).
- WRAP ALL private reasoning (judgments, complaints, internal notes, "what I really think") in <internal>...</internal> tags. NEVER write private thoughts as bare text outside tags.
- You MUST wrap the public reply in <public>...</public> tags. IMPORTANT: if your output contains <internal> private tags but NO <public> block, NOTHING is posted (the whole reply is dropped, fail-closed). So whenever you have private thoughts, you MUST also emit a <public> block for anything you actually want to say.
- Emit each tag EXACTLY ONCE per block and always close it: write <public>text</public>, never <public><public>text</public> and never leave a tag unclosed.
- A reply with no tags at all is also fine: when send:true, the plain text is posted (subject to the control block above).
- When send:false, do NOT narrate "I won't reply" as a public message — just set send:false. Any text before the control line (inside or outside tags) is treated as a private note and will NOT be posted.
- The control line itself is stripped before posting; users never see it.
- Put the actual reply text BEFORE the control line. The control line must be the very last thing you output.`

// replyControlSignal 是回复控制块的结构化产出。
type replyControlSignal struct {
	Send bool `json:"send"`
}

// parseReplyControl 从模型回复中提取结尾的控制 JSON。
//
// 返回 (send, clean, ok)：
//   - ok=false：找不到分隔符，或分隔符后 JSON 解析失败 → 调用方应 fail-closed 不出站。
//   - ok=true, send=false：模型显式声明不回复 → 调用方抑制出站。
//   - ok=true, send=true：clean 为去除控制块后的干净正文，由调用方发出。
//
// 容错仅限于与语言无关的格式噪音（markdown 围栏、前后多余文本），复用 lurk 契约的同款
// 辅助函数。绝不做任何自然语言兜底——解析失败即 fail-closed，与「不靠枚举」原则一致。
func parseReplyControl(text string) (send bool, clean string, ok bool) {
	idx := strings.LastIndex(text, replyControlDelimiter)
	if idx < 0 {
		return false, text, false
	}
	rest := text[idx+len(replyControlDelimiter):]
	rest = stripCodeFence(rest)
	rest = strings.TrimSpace(rest)
	obj := extractFirstJSONObject(rest)
	if obj == "" {
		return false, text, false
	}
	var sig replyControlSignal
	if err := json.Unmarshal([]byte(obj), &sig); err != nil {
		return false, text, false
	}
	// 干净正文 = 控制块之前的内容（去除尾部空白），控制块本身不发出。
	clean = strings.TrimRight(text[:idx], " \t\n\r")
	return sig.Send, clean, true
}

// StripReplyControlBlock 从文本末尾剥离 reply-control 协议控制块
// （@@REPLY_CONTROL@@{"send": ...}），返回干净正文。
//
// 用途：SSE 流式路径的 fullText 是从 EventLLMTextDelta 逐字累积的原始文本，
// 包含控制块。在发送 sseDone 事件前调用此函数剥离，避免内部协议标记泄漏到前端。
func StripReplyControlBlock(text string) string {
	idx := strings.LastIndex(text, replyControlDelimiter)
	if idx < 0 {
		return text
	}
	rest := text[idx+len(replyControlDelimiter):]
	rest = stripCodeFence(rest)
	rest = strings.TrimSpace(rest)
	obj := extractFirstJSONObject(rest)
	if obj == "" {
		return text
	}
	var sig replyControlSignal
	if err := json.Unmarshal([]byte(obj), &sig); err != nil {
		return text
	}
	return strings.TrimRight(text[:idx], " \t\n\r")
}

// publicTagRe 匹配 <public>...</public> 显式公开回复区（回复控制协议的 opt-in 标签）。
// 模型用它明确「只发这段」，门控据此丢弃其余所有内容（含 <internal> 心里话），彻底防泄漏。
var publicTagRe = regexp.MustCompile(`(?is)<public>\s*(.*?)\s*</public>`)

// internalOnlyRe 匹配「含 <internal> 私密标签但未用 <public> 标出公开区」的情形：
// 完整对 <internal>...</internal>，或裸开标签 <internal>（未闭合/流式截断）。
// 命中即视为模型泄露了私密意图却没给出公开出口 —— 整段 fail-closed 不发，
// 避免把 internal 之外的文本也带出去（取巧逻辑：<internal> 是强私密信号，无 <public> 则整段沉）。
var internalOnlyRe = regexp.MustCompile(`(?is)<internal>.*?</internal>|<internal>`)

// strayTagRe 兜底剥离所有残留的结构化 / HTML 标签（含模型偶发的畸形形态）。
//
// 覆盖三类：
//  1. 正常标签：<public> </public> <internal> <long> <p> <ul> <li> <b> ...
//  2. 畸形开标签（缺 <）：模型偶发把 <public> 写成 /public>，首条内存回复就裸发了 "/public>"。
//  3. 嵌套/重复开标签：<public>\n<public>正文</public> 内层字面 <public> 被 publicTagRe 当正文捕获。
//
// 必须兜底剥离的原因：Misskey / Telegram 不渲染裸 HTML，<p>/<ul>/<li> 会原样显示成奇怪标签；
// 畸形 public 标签（/public>）同样会原样发出的帖子。与「不做自然语言兜底」原则不冲突——
// 这里清洗的是结构化标签文本，不是做语义解析。
//
// 注意：起始符限定为 `<` 或 `/`，避免误伤正文里 "dog>" 这类正常英文（要求标签有明显起始符号）。
var strayTagRe = regexp.MustCompile(`(?is)(?:</?|/)[a-zA-Z][a-zA-Z0-9_-]*[^>]*>`)

// extractPublicReply 从清洗后的干净正文里提取「应公开发送」的内容（取巧三态）：
//
//  1. 有 <public> 标签 → 只拼接各 <public> 区块内文（模型明确"就发这些"，
//     其余包括任何 <internal> 心里话一律丢弃，最防泄漏）；
//  2. 含 <internal> 但无 <public> → 返回空（整段 fail-closed 不发——既然暴露了私密意图
//     又没给公开出口，连 internal 之外的文本也不该带出）；
//  3. 纯文本（无任何标签）→ 原样返回，由上层 control 块决定（send:true 发全文）。
//
// 无论哪条路径，私有标签（心里话）都不会进入出站 payload。
func extractPublicReply(clean string) string {
	// 路径 1：<public> 公开区优先，只发其内文（最防泄漏）。
	if blocks := publicTagRe.FindAllStringSubmatch(clean, -1); len(blocks) > 0 {
		var b strings.Builder
		for _, m := range blocks {
			b.WriteString(m[1])
			b.WriteString("\n")
		}
		// 公开区内也可能夹带 <internal>，再剥一次确保心里话不外发。
		out := memory.StripInternalTags(strings.TrimSpace(b.String()))
		// 去重：LLM 偶发在多个 <public> 块里输出相同内容（如 tool_choice 解封后续生成
		// 把回复写了两遍），导致最终帖子正文出现完全重复的两段文字。
		// 检测「前半段 ≈ 后半段开头」的整段重复模式，只保留第一份。
		out = deduplicatePublicContent(out)
		// 再兜底剥离任何残留标签（含畸形 public / 嵌套字面标签 / HTML 标签），
		// 避免标签文本外发到帖子或消息。
		return strings.TrimSpace(strayTagRe.ReplaceAllString(out, ""))
	}
	// 路径 2：含 <internal> 私密标签却没给 <public> 公开出口 → 整段不发。
	if internalOnlyRe.MatchString(clean) {
		return ""
	}
	// 路径 3：纯文本（无标签）→ 仍有概率含模型偶发的畸形标签（如 /public>）或 HTML，
	// 出站前统一兜底剥离，避免任何标签文本裸发到帖子/消息。
	return strings.TrimSpace(strayTagRe.ReplaceAllString(clean, ""))
}

// deduplicatePublicContent 检测并去除 extractPublicReply 拼接多 <public> 块后
// 可能产生的整段重复文本。
//
// 触发场景：LLM 在同一次输出中写出两个内容相同的 <public>...</public> 块
//（例如 tool_choice 解封后续生成时把回复写了两遍），导致最终帖子正文
// 出现完全重复的两段文字。
//
// 算法：检测 s = X + sep + X 模式（sep 为 0~2 字符的空白/换行）。
// 从中点向外搜索分割点，找到两半完全一致则只保留第一份 X。
// 最短匹配长度 40 字符，避免短句/签名等正常重复被误删。
func deduplicatePublicContent(s string) string {
	trimmed := strings.TrimRight(s, " \t\n\r")
	if len(trimmed) < 80 {
		return s
	}

	// 尝试 0~2 字符分隔符（\n 或空格），从中点附近搜索分割点
	for sepLen := 0; sepLen <= 2; sepLen++ {
		half := (len(trimmed) - sepLen) / 2
		if half < 40 || half+sepLen+half > len(trimmed) {
			continue
		}
		if trimmed[:half] == trimmed[half+sepLen:2*half+sepLen] {
			return trimmed[:half]
		}
	}

	// 宽松模式：允许尾部有少量不匹配字符（LLM 第二份可能略有差异）
	// 在中点 ±10 字符范围内搜索最佳分割点
	for offset := 0; offset <= 10; offset++ {
		for _, mid := range []int{len(trimmed)/2 - offset, len(trimmed)/2 + offset} {
			if mid < 40 || mid >= len(trimmed) {
				continue
			}
			prefix := trimmed[:mid]
			suffix := trimmed[mid:]
			// 允许 suffix 比 prefix 长一点（第二份可能多了结尾标点）
			minLen := min(len(prefix), len(suffix))
			if minLen < 40 {
				continue
			}
			// 检查前 minLen 字符是否完全一致
			if prefix[:minLen] == suffix[:minLen] {
				return prefix
			}
		}
	}

	return s
}

// explicitPublicReply 仅当正文含**显式且成对**的 <public>...</public> 区块时，返回其公开
// 内文；否则返回空串。
//
// 用途：控制块（@@REPLY_CONTROL@@）缺失时的降级出站判定。模型偶发漏掉结尾控制行，
// 但已用 <public> 亲手标注了「这段给外面看」—— 此时按 public 内文出站，其防泄漏强度与
// 控制块存在时的路径 1 **完全相同**（同样只发 public 内文、丢弃 <internal> 心里话）。
// 反之，模型若不想说话只会写 <internal> 而不写 <public>，那条路径仍旧 fail-closed。
//
// 刻意要求成对闭合：未闭合的 <public> 可能是流式截断的半句话，不可出站。
func explicitPublicReply(clean string) string {
	if !publicTagRe.MatchString(clean) {
		return ""
	}
	return extractPublicReply(clean)
}

// emitLurkNote 在潜水模式下把 LLM 的结构化产出作为内部学习笔记（ActionNote）写入 L0。
//
// 判定完全依赖契约 JSON 的 remember 布尔（见 lurk_contract.go），与模型的思考语言无关。
// 这里 **刻意不做** 任何自然语言兜底：解析失败由上游重试处理，重试耗尽即放弃不落库。
// 历史上这里曾枚举 [NONE]/[无]/[なし]/「无需记忆」等标记，属打地鼠，已彻底移除，勿回退。
func (s *LLMStage) emitLurkNote(env *core.Envelope, result *llm.GenerateResult) {
	out, outcome := parseLurkOutput(result.Text)
	switch outcome {
	case lurkParseSkip:
		s.logger.Debugw("lurk: model reported nothing worth remembering, skip note",
			"message_id", env.Message.ID)
		return
	case lurkParseInvalid:
		// 走到这里说明重试已耗尽（重试在 Process 内完成）。安全失败：宁可丢一条
		// 观察，也不把无法解析的半成品写进记忆。计数用于观测「昨晚丢了多少条」。
		s.logger.Warnw("lurk: json contract unsatisfied after retries, abandon note",
			"message_id", env.Message.ID,
			"finish_reason", result.FinishReason,
			"raw_len", len(result.Text))
		return
	}

	env.AddAction(core.Action{
		Type:    core.ActionNote,
		Channel: "", // 潜水学习笔记以 bot 全局 scope 落库（note_handler 据 bot_id 判 scope），跨渠道可用
		UserID:  env.Message.UserID,
		Payload: out.Note,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"category":       "lurk",
			// speaker=observer：标明这是 bot 的「观察」而非用户原话，也不是 bot 的自述产出。
			// dreaming 归因护栏据此保留观察记忆、且不把 @handle 洗成「用户/此人」。
			"speaker": "observer",
			// ephemeral 由模型结构化给出，替代过去按「开播/放送」等短语正则判时效（语言枚举）。
			// dreaming 晋升阶段据此决定不进 L1。
			"ephemeral": out.Ephemeral,
			// importance 需为 0.0~1.0 的 float64：note_handler 只接受 float64，
			// 且 Entry.Importance 语义是 0~1（参与召回打分）。模型给的是 1~5 整数，
			// 这里做量纲转换，不可直接透传 int（会被静默忽略并落回默认 0.5）。
			"importance":     lurkImportanceToScore(out.Importance),
			"speaker_handle": out.SpeakerHandle,
		},
	})
	s.logger.Infow("lurk: learning note captured to L0",
		"message_id", env.Message.ID,
		"note_len", len(out.Note),
		"ephemeral", out.Ephemeral,
		"importance", out.Importance,
		"speaker_handle", out.SpeakerHandle)
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
	// Temperature 采样温度。跟随模型（ModelDef.Temperature），不归 bot/全局管。
	Temperature *float64
	// TopP 核采样参数（nucleus sampling）。跟随模型（ModelDef.TopP），nil 时由 Provider 用默认。
	TopP *float64
	// FrequencyPenalty 重复惩罚（GLM-5.x 推荐 0.1）：抑制模型反复输出相同 token。
	FrequencyPenalty *float64
	// PresencePenalty 存在惩罚（GLM-5.x 推荐 0.05）：抑制已出现 token 再次出现。
	PresencePenalty *float64
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

	// Compaction 可选的主 Agent 对话历史自动压缩配置（nil=禁用，沿用
	// ReductionConfig 的开关范式）。非 nil 时，主链路在每步编排前检测 token 用量，
	// 超阈值（llm.DefaultCompactionConfig 保守取 ~44k 可用 token）自动用 LLM 生成
	// 结构化摘要替代旧消息——修复「长会话退化成混乱调用循环、任务做不完」的根因
	// （主生成链路此前漏接了压缩钩子，只有 subagent 有）。Compactor 状态按会话(sid)
	// 隔离，使压缩摘要在同一会话内跨轮持久、且不串扰并发会话。
	Compaction *llm.CompactionConfig

	// HardTimeout 主 Agent 编排回路的墙钟硬上限（0=不启用）。
	// 仅当传入的 ctx 本身没有 deadline 时才生效（若上游 worker/channel 已设
	// 了 deadline，则尊重上游、不覆盖）——这与 subagent 的 chatWithHardTimeout
	// 同源设计：兜底「无客户端的后台渠道（如 Misskey）ctx 永不取消」导致的
	// 编排永久挂起（如某工具/LLM 流假活不返回）。启用后，编排总时长超过该值即
	// 被强制终止（context.DeadlineExceeded），避免单条消息无限占用 goroutine
	// 与下游资源。典型值 15min，远大于单步 LLM 客户端超时（~120s）与绝大多数
	// 正常任务耗时；真要跑更久的任务应拆细而非依赖单次长编排。
	HardTimeout time.Duration

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

	// ToolOutputSink 落盘指针接收器（见 llm.ToolOutputOffloadSink）。
	// 非 nil 时，被截断的工具输出会把完整原文写入 bot 工作空间，主上下文仅留
	// 预览+指针+子 agent 委托提示，从而把深挖代码的代价隔离到独立子 agent 上下文。
	// 由 Bot 装配时注入（仅当 tool_output.offload 开启）。
	ToolOutputSink llm.ToolOutputOffloadSink

	// ToolOutput 工具输出截断阈值（行/字节），透传到 OrchestrateConfig.ToolOutput。
	// 零值字段由 runTool 回退到 llm.DefaultToolOutputConfig()；由 Bot 装配时从
	// tool_output.{max_lines,max_bytes} 配置填充。
	ToolOutput llm.ToolOutputConfig

	// DeferredApprovalStore HITL 续跑锚点存储。非 nil 时，工具审批被 defer 会持久化
	// 一条待确认记录，供人类确认后由 ResumeDeferredApproval 续跑恢复。为 nil 时不持久化
	// （仅记日志），仍不阻断默认路径（默认无 ApprovalHandler，此分支不触发）。
	DeferredApprovalStore DeferredApprovalStore

	// ResumeDispatch HITL 续跑入口：给定原始入站消息，重新走完整编排管线。
	// 由 Bot 装配时接入 Engine.ProcessSync，使 ResumeDeferredApproval 能真正重跑该消息。
	ResumeDispatch func(ctx context.Context, msg core.Message) (*core.Envelope, error)

	// RequireReplyControl 回复控制门控（opt-in，默认 false）。
	// 开启后，模型必须在回复正文之后追加一个结构化控制 JSON：
	//   @@REPLY_CONTROL@@{"send": true}    —— 允许将上文作为回复发出
	//   @@REPLY_CONTROL@@{"send": false}   —— 决定不互动/不回复（正文不会被发出）
	// 缺失控制块 / 解析失败 / send:false → 一律不出站（fail-closed），与用户要求的
	// 「读取失败，或者没有则不会出站」一致。用于治理「模型决定不互动却把独白当回复发出」。
	//
	// 标签协议（replyControlInstruction）：所有私有心里话必须包进 <internal>...</internal>，
	// 公开回复可包进 <public>...</public>。出站时门控按三态提取（见 extractPublicReply）：
	//   - 有 <public> → 只发其内文；
	//   - 含 <internal> 但无 <public> → 整段不发（<internal> 是强私密信号，无公开出口即沉）；
	//   - 纯文本无标签 → send:true 发全文。
	// 既防泄漏又不吞真实回复，仅对显式开启的 bot 生效。
	RequireReplyControl bool
}

// ============================================================================
type LLMStage struct {
	name     string
	provider llm.Provider
	config   LLMConfig
	tracer   trace.Tracer
	logger   *zap.SugaredLogger
	// compactors 按会话(sid)隔离的 *llm.Compactor 实例。主链路压缩钩子需要跨轮
	// 持久化摘要状态（previousSummary 增量更新），故以 sid 为 key 惰性创建；
	// sync.Map 免锁，生命周期与 bot 进程同寿（并发会话数有界，无泄漏风险）。
	compactors sync.Map
}

// getCompactor 返回指定会话的 *llm.Compactor（惰性创建）。
// 返回 (compactor, true)；当 s.config.Compaction 为 nil 时返回 (nil, false)。
// 按 sid 隔离使压缩摘要状态在同一会话内跨轮持久、且不与并发会话串扰。
func (s *LLMStage) getCompactor(sid string) (*llm.Compactor, bool) {
	if s.config.Compaction == nil {
		return nil, false
	}
	if sid == "" {
		sid = "__default__"
	}
	if v, ok := s.compactors.Load(sid); ok {
		return v.(*llm.Compactor), true
	}
	c := llm.NewCompactor(*s.config.Compaction).SetLogger(s.logger)
	actual, _ := s.compactors.LoadOrStore(sid, c)
	return actual.(*llm.Compactor), true
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

// SetToolOutputSink 注入落盘指针接收器。在 Bot 装配完成后、真正编排前调用，
// 避免改动 NewLLMStage 签名（其测试调用方众多）。
func (s *LLMStage) SetToolOutputSink(sink llm.ToolOutputOffloadSink) { s.config.ToolOutputSink = sink }

// SetToolOutputConfig 注入工具输出截断阈值（行/字节）。同上，避免改动 NewLLMStage 签名。
func (s *LLMStage) SetToolOutputConfig(cfg llm.ToolOutputConfig) { s.config.ToolOutput = cfg }

// SetDeferredApprovalStore 注入 HITL 续跑锚点存储（nil 表示不持久化，仅记日志）。
func (s *LLMStage) SetDeferredApprovalStore(store DeferredApprovalStore) {
	s.config.DeferredApprovalStore = store
}

// SetResumeDispatch 注入 HITL 续跑入口（重新编排原始消息）。通常由 Bot 装配时接入
// Engine.ProcessSync，使 ResumeDeferredApproval 能真正重跑被 defer 的消息。
func (s *LLMStage) SetResumeDispatch(fn func(ctx context.Context, msg core.Message) (*core.Envelope, error)) {
	s.config.ResumeDispatch = fn
}

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

	// 注入本轮消息元信息（平台类型 / 会话空间 / 回写目标）：交互类工具
	// （user_choice 等）据此判断应答平台与渲染路径。
	ctx = agenttools.ContextWithMessageMeta(ctx, agenttools.MessageMeta{
		BotID:       env.Message.BotID,
		ChatID:      env.Message.Channel,
		ChannelType: messageChannelType(&env.Message),
		ReplyTarget: messageReplyTarget(&env.Message),
	})

	// 注入「直接回复语境」标记：对方 @ 了 Bot 或回复了 Bot（Mentioned=true）时，
	// Channel 工具可据此禁用「手动发孤立帖」类能力（如 misskey_create_note），
	// 强制走框架自动串接回复，避免重复发文。该值在 Orchestrate 全程透传到工具执行。
	ctx = llm.WithDirectReply(ctx, env.Message.Mentioned)

	// 注入更宽的「入站回复语境」标记：框架对**任何**带 reply_target 的入站帖
	// 都会自动串接回复（含未 @ Bot 的普通 timeline 帖）。此时若 Channel 工具再手动
	// 发一条新帖，同一条入站帖就会收到两条发文。据此让工具层禁用「手动发孤立帖」，
	// 强制走框架自动串接回复，避免重复发文。覆盖 IsDirectReply 未触达的未 @ 场景。
	// HasReplyTarget：Metadata 是 map[string]any，缺失 key 时返回 any(nil)，
	// 直接用 `!= ""` 比较会得到 any(nil) != any("") == true（永远为真），
	// 导致工具层「手动发孤立帖」永远被禁。必须类型断言判非空字符串。
	hasReplyTarget := false
	if v, ok := env.Message.Metadata["reply_target"].(string); ok && v != "" {
		hasReplyTarget = true
	}
	ctx = llm.WithInboundReply(ctx, llm.InboundReply{
		Source:         env.Message.Source,
		HasReplyTarget: hasReplyTarget,
	})

	// 构建消息
	var messages []llm.Message
	if s.config.MessageBuilder != nil {
		messages = s.config.MessageBuilder(env.Message)
	} else {
		content := env.Message.Text
		// 心跳等触发源用独立 InjectContext 作为对话内容：Text 故意留空，
		// 避免被 note_capture 当作「用户原文」写入 L0 长期记忆（见 docs/heartbeat-redesign.md §7）。
		if content == "" && env.Message.InjectContext != "" {
			content = env.Message.InjectContext
		}
		messages = []llm.Message{llm.UserMessage(content)}
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
	heartbeatMode := isHeartbeatMode(env)
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

	// 回复控制门控（opt-in）：开启时追加协议说明，让模型在回复结尾追加结构化
	// 「是否出站」控制 JSON。仅对显式开启的 bot 生效，不影响其它 bot。
	//
	// 心跳唤醒例外：它是系统自主触发、不是回复用户消息，其出站由 heartbeat 的
	// decision 字段经 ChannelPoster 单独处理，REPLY_CONTROL 的 send 对它无意义。
	// 而心跳的 InjectContext 已经要求「只输出一个 JSON 对象」，两套协议同时出现时
	// 模型会把它们合并成 [{decision…}, {"send":false}] 数组
	// （2026-08-29 实测：决策被静默降级丢弃）。故心跳路径不注入本协议。
	if s.config.RequireReplyControl && env.Message.Source != core.SourceHeartbeat {
		systemPrompt = systemPrompt + "\n\n" + replyControlInstruction
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
		Model:            s.config.Model,
		System:           systemPrompt,
		Messages:         messages,
		Tools:            tools,
		Temperature:      s.config.Temperature,
		TopP:             s.config.TopP,
		FrequencyPenalty: s.config.FrequencyPenalty,
		PresencePenalty:  s.config.PresencePenalty,
		MaxTokens:        s.config.MaxTokens,
		ReasoningEffort:  reasoningEffortPtr(s.config.ReasoningEffort),
	}

	// 潜水模式：强制结构化 JSON 输出，使「是否值得记忆」成为一个与语言无关的布尔，
	// 而非需要代码去枚举匹配的自然语言标记（[NONE]/[なし]/「无需记忆」……）。
	//
	// 用 json_object 而非 json_schema：实测 bigmodel 接受 json_schema 但 **不强制**
	// required 字段（少返字段照样 200），依赖它会得到虚假的安全感 —— 字段校验一律
	// 在 Go 侧做（parseLurkOutput）。
	// 同时把采样温度压低：结构化输出不需要创造性，低温显著提升格式合规率。
	if lurkMode {
		params.ResponseFormat = &llm.ResponseFormat{Type: llm.ResponseFormatJSONObject}
		params.Temperature = lurkTemperature()
	}

	// 心跳自主唤醒决策模式：与潜水模式同机制——强制 JSON 结构化输出 + 低温，
	// 使「是否发言 / 发到哪个渠道 / 发什么」成为结构化字段，杜绝自由文本歧义
	// （LLM 换个说法表达「静默」就被程序解析失败）。仅心跳消息（KVHeartbeatMode=true）
	// 生效，正常对话无影响。决策产出的真实发帖由心跳 Executor 经 ChannelPoster 路由，
	// 不走通用出站（心跳恒设 KVSuppressReply）。
	if v, ok := env.Get(core.KVHeartbeatMode); ok {
		if b, ok := v.(bool); ok && b {
			params.ResponseFormat = &llm.ResponseFormat{Type: llm.ResponseFormatJSONObject}
			params.Temperature = lurkTemperature()
		}
	}

	cfg := &llm.OrchestrateConfig{
		Params:       params,
		MaxSteps:     s.config.MaxSteps,
		HardMaxSteps: s.config.HardMaxSteps,
		// 把本轮的「用户中途追加」通道透传给编排循环（Claude-CLI 风格），
		// 让生成中的用户补充能注入同一轮对话。
		InterruptCh: bot.InterruptChannelFromContext(ctx),
		// 写操作意图护栏（Layer B）：把触发本轮的用户请求文本透传，供标记了
		// RequiresUserIntent 的写工具（如 misskey follow/post/react）判定调用
		// 是否根植于用户显式意图。子代理场景下 env.Message.Text 即其任务描述。
		UserRequest: env.Message.Text,
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

	// 注入落盘指针接收器：被截断的工具输出把完整原文写入工作空间，主上下文
	// 仅留预览+指针+子 agent 委托提示，把深挖代码的代价隔离到独立子 agent 上下文。
	if s.config.ToolOutputSink != nil {
		cfg.ToolOutputSink = s.config.ToolOutputSink
		// botID 直接用消息归属（env.Message.BotID），比依赖 ctx 透传的 CallOrigin 更稳健。
		cfg.BotID = env.Message.BotID
	}
	// 截断阈值：从 LLMConfig.ToolOutput 透传（零值字段在 runTool 内回退默认）。
	cfg.ToolOutput = s.config.ToolOutput

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

	// 上下文压缩：两阶段。
	//   阶段1（OnToolResults）：工具执行后截断超大单条输出（ReductionConfig）。
	//   阶段2（PrepareStep）：模型调用前压缩旧消息历史。
	//     - ReductionConfig → reducer 钩子（历史 pruning）。
	//     - Compaction → compactor 钩子（超阈值时 LLM 摘要，修复长会话退化）。
	//   二者可叠加：先跑 reducer 原地压缩，再交 compactor 判定是否需 LLM 摘要。
	//   compactor 按会话隔离（getCompactor），摘要状态跨轮持久、不串扰并发会话。
	var prepareStep func(*llm.GenerateParams) *llm.GenerateParams
	if s.config.ReductionConfig != nil {
		rc := *s.config.ReductionConfig
		cfg.OnToolResults = llm.NewOnToolResultsCallback(rc)
		prepareStep = llm.NewReducePrepareStepCallback(rc)
	}
	if s.config.Compaction != nil {
		if compactor, ok := s.getCompactor(session.SessionIDFromEnvelope(env)); ok {
			compactHook := llm.CompactionPrepareStepWithProvider(compactor, s.provider)(ctx)
			base := prepareStep
			prepareStep = func(p *llm.GenerateParams) *llm.GenerateParams {
				if base != nil {
					if bp := base(p); bp != nil {
						p = bp
					}
				}
				if cp := compactHook(p); cp != nil {
					return cp
				}
				return p
			}
		}
	}
	if prepareStep != nil {
		cfg.PrepareStep = prepareStep
	}

	logger.Debugw("llm stage: starting orchestrate",
		"message_id", env.Message.ID,
		"provider", s.provider.Name(),
		"max_steps", s.config.MaxSteps,
		"hard_max_steps", s.config.HardMaxSteps,
		"streaming", s.config.StreamPublisher != nil)

	var result *llm.GenerateResult
	// 墙钟硬上限兜底：仅当传入 ctx 无 deadline 时才叠加（尊重上游已有的
	// deadline，不覆盖）。这与 subagent.chatWithHardTimeout 同源——后台渠道
	//（Misskey）的 ctx 无客户端可取消，若编排内某工具/LLM 流假活不返回，
	// 会导致单条消息永久挂起 + goroutine/资源泄漏。此处用墙钟 deadline 收口。
	workCtx, workCancel := ctx, func() {}
	if s.config.HardTimeout > 0 {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			workCtx, workCancel = context.WithTimeout(ctx, s.config.HardTimeout)
		}
	}
	defer workCancel()
	// WithStatsSkip: StatsRecordingProvider 会跳过 Orchestrate 内部的每次调用，
	// 由下方 recordUsage() 统一记录合并后的总用量到 journal + stats
	statsCtx := llm.WithStatsSkip(workCtx)
	// 潜水模式强制走非流式：潜水产出没有实时观众（不发帖、只落库），
	// 流式只会让「解析 JSON → 不合规则重试」的控制流复杂化。
	if s.config.StreamPublisher != nil && !lurkMode && !heartbeatMode {
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
			if errors.Is(err, context.DeadlineExceeded) {
				// 墙钟硬上限触发（HardTimeout）：编排总时长超限被强制终止，
				// 避免后台渠道消息无限挂起。明确归因，区别于普通 provider 错误。
				logger.Warnw("llm stage: stream orchestrate hard-timeout (wall-clock cap exceeded)",
					"message_id", env.Message.ID,
					"hard_timeout", s.config.HardTimeout.String(),
					"err", err)
				return env, &core.PipelineError{
					Stage:   s.name,
					Message: "LLM stream orchestrate exceeded hard timeout",
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
			if errors.Is(err, context.DeadlineExceeded) {
				// 墙钟硬上限触发（HardTimeout）：编排总时长超限被强制终止。
				logger.Warnw("llm stage: orchestrate hard-timeout (wall-clock cap exceeded)",
					"message_id", env.Message.ID,
					"hard_timeout", s.config.HardTimeout.String(),
					"err", err)
				return env, &core.PipelineError{
					Stage:   s.name,
					Message: "LLM orchestrate exceeded hard timeout",
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

	// ── 重复退化安检（流式 + 非流式共用）───────────────────────────────
	// 流式路径已在 processStream 内做增量检测，此处作为兜底再次检查
	// （防御流式检测阈值未触发但完整文本已明显退化的边界情况）。
	// 非流式路径则完全依赖此处的静态检测。
	if cleaned, truncated := llm.DetectStaticRepetition(result.Text); truncated {
		logger.Warnw("repetition collapse detected in final output, truncating",
			"message_id", env.Message.ID,
			"original_len", len(result.Text),
			"truncated_len", len(cleaned))
		result.Text = cleaned
	}

	// 记录 OTel 属性
	span.SetAttributes(
		attribute.Int("llm.steps", len(result.Steps)),
		attribute.Int("llm.total_tokens", result.Usage.TotalTokens),
		attribute.Int("llm.input_tokens", result.Usage.InputTokens),
		attribute.Int("llm.output_tokens", result.Usage.OutputTokens),
		attribute.String("llm.finish_reason", string(result.FinishReason)),
	)

	// 可观测：把 bot 最终回复正文落日志，便于事后 grep「bot 到底说了什么」。
	replyLog := result.Text
	if len(replyLog) > 4000 {
		replyLog = replyLog[:4000] + "...(truncated)"
	}
	logger.Infow("llm stage: generation complete",
		"message_id", env.Message.ID,
		"steps", len(result.Steps),
		"tokens", result.Usage.TotalTokens,
		"finish_reason", result.FinishReason,
		"reply", replyLog)

	// 可观测：若编排层因工具审批被 defer（RequireApproval + ApprovalHandler
	// 返回 deferred），记录信号并落锚点，供人类确认后续跑恢复。
	// 当前默认无 ApprovalHandler，此分支通常不触发（见下方注释）。
	if result.DeferredToolApproval != nil {
		da := result.DeferredToolApproval
		logger.Warnw("llm stage: tool approval deferred (HITL pending)",
			"message_id", env.Message.ID,
			"approval_id", da.ApprovalID,
			"tool", da.ToolName,
			"reason", da.Reason)

		// 锚点：持久化被 defer 的审批，供续跑恢复（resume 入口）。
		// store 为 nil 时安全跳过（仅记日志）；默认路径无 ApprovalHandler，此分支不触发。
		if s.config.DeferredApprovalStore != nil {
			if rec, berr := BuildDeferredApproval(da, env.Message); berr != nil {
				logger.Warnw("hitl: build deferred record failed", "err", berr, "approval_id", da.ApprovalID)
			} else if berr := s.config.DeferredApprovalStore.Persist(context.Background(), rec); berr != nil {
				logger.Warnw("hitl: persist deferred approval failed", "err", berr, "approval_id", da.ApprovalID)
			}
		}
		// 事件锚点（进入可观测轨迹）。
		if sink := core.EventSinkFromContext(ctx); sink != nil {
			sink.Emit(ctx, core.Event{
				Kind:   core.EventHitlDeferred,
				Source: "hitl",
				Payload: map[string]any{
					"approval_id": da.ApprovalID,
					"tool":        da.ToolName,
					"message_id":  env.Message.ID,
					"reason":      da.Reason,
				},
			})
		}

		// 阻断半成品回复：本 Stage 是唯一产出 ActionReply 之处，下方 return 已在
		// ActionReply 生成（见文件末尾）之前，故回复天然被阻断；标记供下游/可观测识别。
		env.Set(core.KVLLMDeferred, true)
		env.Set("llm.result", result)
		return env, nil
	}

	// 记录使用统计
	recordUsage(ctx, s.config.UsageRecorder, env, s.config.Model, s.name, result)

	// 潜水模式：校验结构化产出，不合规则重试。必须放在下方 FinishReasonLength
	// 提示语拼接之前 —— 那段会污染 result.Text，破坏 JSON 解析。
	if lurkMode {
		result = s.retryLurkUntilValid(statsCtx, cfg, result, logger, env)
	}

	// 若 LLM 因达到输出 token 上限被截断，追加提示，
	// 避免用户误以为任务已完成（实际可能只生成了半成品回复）。
	// 潜水模式不拼接：产出是给机器解析的 JSON，不是给人看的回复。
	//
	// 注意：部分 provider（如 GLM-5.2）在「思考+正文」共享预算耗尽时，
	// 把 finish_reason 报成 "stop" 而非 "length"，但 usage.outputTokens 已达
	// max_tokens 上限。因此不能只看 FinishReasonLength，还要用 usage 触顶兜底，
	// 否则会被静默截断（回复写到一半断掉、毫无提示）。
	hitCap := result.FinishReason == llm.FinishReasonLength
	if !hitCap && s.config.MaxTokens != nil && *s.config.MaxTokens > 0 &&
		result.Usage.OutputTokens >= *s.config.MaxTokens {
		hitCap = true
	}
	if hitCap && !lurkMode && !heartbeatMode {
		result.Text += "\n\n⚠️ 提示：本次回复因达到输出 token 上限被截断，任务可能未完成。" +
			"请回复「继续」让我接着完成剩余工作。"
	}

	// 若编排循环因步数守卫（撞硬上限或陷入重复循环）而停止，追加提示，
	// 避免用户把「步数预算耗尽、Bot 主动停下」误判为卡死。实际上任务
	// 可能尚未跑完，回复「继续」即可让 Bot 接着处理剩余工作。
	if result.LoopStoppedByGuard && !heartbeatMode {
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

	// 抑制检查：上游（如 engagement 参与度评估 / 节奏门）判定「此刻不该说话」时，
	// 默认不产出 ActionReply —— 但 LLM 已经跑完、结果仍存进 KV，
	// 供记忆写入等下游 Stage 使用。即「照样听、照样想、照样记，只是不说出口」。
	//
	// 这一步是必要的：本Stage 是全项目唯一产出 ActionReply 的地方，
	// 若不在此拦截，上游的静默决策对实际发送没有任何约束力。
	//
	// 优先级裁定（重要，勿回退）：当 REPLY_CONTROL 协议开启、且模型在结尾显式给出
	// send:true 并带有可公开发送的内容时，以模型的确定性信号为准、放行出站；
	// 上游节奏/engagement 启发式门退化为「模型未显式 opt-in 时的兜底」。这与本项目
	// 「出站与否由模型确定性信号裁决，而非启发式」的整体设计一致（见本文件
	// REPLY_CONTROL 设计注释）。修复前节奏门（rhythm_speak_tendency）会在模型已用
	// <public>+send:true 给出真实回复（如群内被问技术问题时）仍整条吞掉。
	// 注：潜水(lurk)/全局静音(mute) 由更早分支或 OutboundGuard 兜底，不会因此漏出发。
	if suppressed, reason := replySuppressed(env); suppressed {
		// 模型显式 send:true 仅覆盖「软启发式」抑制门（节奏/engagement 节流判断）：
		// 这类门表达的是「此刻该不该说」，模型经 REPLY_CONTROL 给出确定性意图时放行合理。
		// 但 passive 模式的 passive_mode_unmentioned 是**硬权限门**（「此消息未被 @，
		// bot 无权主动发言」），性质不同，绝不被模型放行覆盖——否则被动回复 bot 会对
		// 没 @ 它的消息发帖，违背「只被动回复」契约（见 2026-08-25 日志审计发现的回归）。
		// 注：潜水(lurk)/mute 在更早分支已 return，不会到达本门（见上方 lurk 分支）。
		override := s.config.RequireReplyControl && modelExplicitlySends(result.Text) &&
			reason != suppressReasonPassiveUnmentioned
		if !override {
			span.SetAttributes(
				attribute.Bool("reply.suppressed", true),
				attribute.String("reply.suppress_reason", reason),
			)
			logger.Infow("reply suppressed: not sending to channel",
				"message_id", env.Message.ID,
				"reason", reason,
				"text_len", len(result.Text))
			env.Set("llm.result", result)
			// 告知 note-capture：本条虽被门禁抑制（不说出口），用户原文仍须落 L0
			// 记忆——「照样记，只是不说出口」。修复前抑制分支不产出 ActionReply，
			// note-capture 无 reply 可据 → passive 模式下记忆零写入（2026-09-01 定位）。
			env.Set(core.KVCaptureSuppressedExchange, true)
			return env, nil
		}
		// 模型显式 send:true → 放行（仅软启发式门被覆盖，硬权限门已排除）。
		span.SetAttributes(attribute.Bool("reply.override_suppress_by_model", true))
		logger.Infow("reply suppress overridden by model REPLY_CONTROL send:true",
			"message_id", env.Message.ID, "original_reason", reason)
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

	// 纵深防御：剥离入站阶段注入的上下文标记（[Reply to ...] / [Renote from ...] /
	// [note_id: ...]）。这些标记仅供模型理解上下文，但模型偶发会原样回显到回复开头，
	// 导致对外帖子出现诡异方括号前缀。在出站前兜底剥离（与 StripThinking / StripInternalState 同一思路）。
	// 由于后续 clean / pub 均由 replyText 派生，此处一处调用即可覆盖所有出站路径。
	replyText = memory.StripContextMarkers(replyText)

	// 回复控制门控（opt-in）：解析结尾控制 JSON，失败/缺失/send:false 一律不出站。
	// 放在「清洗后空检查」之前——若模型 send:true 但正文为空，后续空检查会照常拦截；
	// 若 send:false，这里已提前 return，独白绝不外发。
	if s.config.RequireReplyControl {
		send, clean, parsed := parseReplyControl(replyText)
		switch {
		case !parsed:
			// 控制块缺失或解析失败。先做一次降级判定：模型偶发漏掉结尾控制行，但已用
			// <public> 显式标注了公开区 —— 那是它亲手给出的「这段给外面看」，据此出站的
			// 防泄漏强度与控制块存在时完全一致（只发 public 内文、丢弃 <internal>）。
			// 不做这个降级，就会因为一行格式噪音把「真人 @ 提及」的回复整条静默吞掉
			// （实测 GLM 漏控制行 + 重复 <public> 开标签，导致被 @ 后完全没回复）。
			pub := explicitPublicReply(replyText)
			if pub == "" {
				// 无显式公开区 → fail-closed，不出站（独白绝不外发）。
				span.SetAttributes(attribute.Bool("reply.control_missing", true))
				logger.Infow("reply suppressed: missing/invalid reply-control block (fail-closed)",
					"message_id", env.Message.ID,
					"raw_len", len(result.Text))
				env.Set("llm.result", result)
				return env, nil
			}
			span.SetAttributes(
				attribute.Bool("reply.control_missing", true),
				attribute.Bool("reply.public_block_fallback", true),
			)
			logger.Infow("reply-control block missing, falling back to explicit <public> block",
				"message_id", env.Message.ID,
				"raw_len", len(result.Text),
				"public_len", len(pub))
			replyText = pub

		case !send:
			span.SetAttributes(attribute.Bool("reply.model_declined", true))
			logger.Infow("reply suppressed: model declared send=false",
				"message_id", env.Message.ID)
			env.Set("llm.result", result)
			return env, nil

		default:
			// send:true —— 提取应公开发送的内容（见 extractPublicReply 的三态逻辑）。
			replyText = extractPublicReply(clean)
			if strings.TrimSpace(replyText) == "" {
				// send:true 但提取不到公开内容：模型只写了 <internal>（忘了 <public>），
				// 或正文确实为空。两种情况都不应外发（fail-closed，绝不泄漏私密）。
				span.SetAttributes(attribute.Bool("reply.internal_only_no_public", true))
				logger.Infow("reply suppressed: send:true but no public content (internal-only or empty)",
					"message_id", env.Message.ID)
				env.Set("llm.result", result)
				return env, nil
			}
		}
	}

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

// ResumeDeferredApproval 是 HITL 续跑入口：人类确认（approve/reject）后调用。
//
// 流程：加载被 defer 的审批记录 → 标记 resolved → 把人类决策按「工具名」注入预批准
// context → 通过 ResumeDispatch 重新编排原始消息（携带该 context）。重新编排时，
// 编排层在 executeTools 中命中预批准表，直接采用该决策而不再二次挂起，从而真正
// 完成此前被暂停的工具调用并产出最终回复。
//
// decision 取值："approved" / "rejected"。
func (s *LLMStage) ResumeDeferredApproval(ctx context.Context, approvalID, decision, reason string) error {
	store := s.config.DeferredApprovalStore
	if store == nil {
		return fmt.Errorf("hitl: no deferred approval store configured")
	}
	rec, err := store.Load(ctx, approvalID)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("hitl: approval %s not found", approvalID)
	}
	if rec.Status == "resolved" {
		return fmt.Errorf("hitl: approval %s already resolved", approvalID)
	}
	if err := store.MarkResolved(ctx, approvalID, decision, reason); err != nil {
		return err
	}

	// 重建原始入站消息（保留 Metadata / reply_target 等下游路由字段）。
	var msg core.Message
	if err := json.Unmarshal([]byte(rec.MessageJSON), &msg); err != nil {
		return fmt.Errorf("hitl: restore message failed: %w", err)
	}

	// 人类决策 → 按工具名预批准。
	dec := llm.ToolApprovalApproved
	if decision == "rejected" {
		dec = llm.ToolApprovalRejected
	}
	pre := llm.PreApprovalMap{
		rec.ToolName: {
			Decision:   dec,
			ApprovalID: approvalID,
			Reason:     reason,
		},
	}
	rctx := llm.WithPreApproval(ctx, pre)

	if sink := core.EventSinkFromContext(rctx); sink != nil {
		sink.Emit(rctx, core.Event{
			Kind:   core.EventHitlResumed,
			Source: "hitl",
			Payload: map[string]any{
				"approval_id": approvalID,
				"tool":        rec.ToolName,
				"decision":    decision,
				"reason":      reason,
			},
		})
	}

	if s.config.ResumeDispatch == nil {
		return fmt.Errorf("hitl: no resume dispatch configured for approval %s", approvalID)
	}
	if _, err := s.config.ResumeDispatch(rctx, msg); err != nil {
		return fmt.Errorf("hitl: resume dispatch failed: %w", err)
	}
	return nil
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

	// 重复退化检测器：增量检测流式输出中的重复 collapse（如 "NN BB NN BB..."）。
	// 触发后立即停止消费 stream channel，截断已累积文本至最后正常位置。
	repGuard := llm.NewRepetitionGuard()

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
				// 重复退化检测：在发布到前端之前先检查增量是否导致 collapse
				if p.Text != "" && !repGuard.Feed(p.Text) {
					// 检测到重复退化：截断已累积文本，停止消费流
					result.Text = repGuard.Text()
					logger.Warnw("repetition collapse detected in stream, truncating",
						"message_id", env.Message.ID,
						"cut_index", repGuard.CutIndex(),
						"original_len", len(result.Text)+len(p.Text),
						"truncated_len", len(result.Text))
					goto streamDone
				}
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
		At:        time.Now(),
		Model:     modelID,
		Feature:   feature,
		Channel:   env.Message.Channel,
		Usage:     result.Usage,
		ToolCalls: toolCalls,
		Steps:     steps,
	})
}
