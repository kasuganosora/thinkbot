package api

import (
	"strings"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/prompt"
)

// promptStageOrder 是主链路 PromptStage 的位置：在节奏门控（95）之后、LLM（100）之前，
// 被前置关卡丢弃的消息不必组装。
const promptStageOrder = 97

// judgePersonaRunes 是 engagement judge 所用简短人格的长度上限（judge 用轻量模型、
// 每条候选帖都调用一次，不应携带完整 system prompt）。
const judgePersonaRunes = 600

// newMainPromptStage 创建主链路的 system prompt 组装 Stage。
//
// 组装内容（全部来自该 bot 的 prompt.Registry，按 (Order, Name) 确定性排序）：
//
//	identity(0)               SOUL.md（SoulLoader 注册并热重载；未加载时回退为 bot 的 system_prompt）
//	operator_instructions(10) bot 的 system_prompt（有 SOUL 时，见 prompt.ComposeIdentity）
//	skill_trigger(150)        技能使用说明（清单与正文经 use_skill 按需获取）
//	tool_*(300-325)           工具使用指引（ToolDef.PromptSection）
//
// 技能正文段落 skill_<name>（Order 500）不渲染：SkillManager 会把所有已启用技能的
// 正文都注册进 Registry，而线上 50 个技能正文合计约 42 万字符（约 10 万 token），
// 每轮全量注入会撑爆上下文，也违背 skill_trigger 的按需加载设计；技能正文由
// use_skill 以 tool_result 返回并留在会话历史中。
//
// 这些都是稳定内容，构成可被模型服务端前缀缓存的 system prompt 前缀；逐轮变化的
// 内容（pipeline 警告、记忆召回、回复控制协议）由 LLMStage 追加在其后。
// 记忆召回走 KVMemoryRecall 由 LLMStage 追加，因此这里关闭 memory.context 注入，避免重复。
func newMainPromptStage(reg *prompt.Registry, tp trace.TracerProvider, logger *zap.SugaredLogger) *prompt.PromptStage {
	cfg := prompt.DefaultPromptStageConfig()
	cfg.InjectMemoryContext = false
	acfg := prompt.DefaultAssemblerConfig()
	acfg.Exclude = isSkillBodySection
	return prompt.NewPromptStage("prompt", prompt.NewAssembler(reg, acfg), cfg, tp, logger)
}

// skillTriggerSectionName 与 skill 包注册的技能使用说明段落同名。
const skillTriggerSectionName = "skill_trigger"

// isSkillBodySection 判断段落是否为技能正文（skill_<name>，技能使用说明段落除外）。
func isSkillBodySection(name string) bool {
	return strings.HasPrefix(name, "skill_") && name != skillTriggerSectionName
}

// botIdentity 返回 bot 的身份文本（SOUL.md + system_prompt，规则同 PromptStage）。
// b 为 nil（未运行）时只用 fallbackSystemPrompt。
func botIdentity(b *bot.Bot, fallbackSystemPrompt string) string {
	soul, sp := "", fallbackSystemPrompt
	if b != nil {
		sp = b.Config.SystemPrompt
		if l := b.SoulLoader(); l != nil && l.Loaded() {
			soul = l.Content()
		}
	}
	return prompt.ComposeIdentity(soul, sp)
}

// judgePersona 返回 engagement judge 的简短人格（每次调用读取，跟随 SOUL.md 热重载）。
func judgePersona(b *bot.Bot, fallbackSystemPrompt string) string {
	return prompt.ShortPersona(strings.TrimSpace(botIdentity(b, fallbackSystemPrompt)), judgePersonaRunes)
}
