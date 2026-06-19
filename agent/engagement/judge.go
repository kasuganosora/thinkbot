package engagement

import (
	"context"
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// LLMJudge — Tier 2 LLM 快判
// ============================================================================

// JudgeResult 是 LLM 快判的结果。
type JudgeResult struct {
	// Engage LLM 认为是否值得参与。
	Engage bool
	// Reason LLM 给出的理由。
	Reason string
}

// LLMJudge 使用轻量 LLM 调用快速判断消息是否值得主动参与。
//
// 这一层是可选的——只有在 Tier 1 规则通过后才调用。
// 使用便宜/快速模型，prompt 极简，只返回 YES/NO + 理由。
type LLMJudge interface {
	// Judge 快速评估消息是否值得参与。
	Judge(ctx context.Context, msg *core.Message) (JudgeResult, error)
}

// ============================================================================
// PromptBuilder — 构建快判 prompt
// ============================================================================

// PromptConfig 配置 LLM 快判的 prompt。
type PromptConfig struct {
	// BotName Bot 名称/人设名。
	BotName string
	// BotPersona Bot 人格描述（简短，1-2 句话）。
	BotPersona string
	// Interests Bot 关注的话题（用于引导 LLM 判断）。
	Interests []string
}

// DefaultPromptConfig 返回默认配置。
func DefaultPromptConfig() PromptConfig {
	return PromptConfig{
		BotName:    "Bot",
		BotPersona: "一个友好的聊天机器人",
		Interests:  []string{},
	}
}

// BuildJudgePrompt 构建 LLM 快判的 system prompt 和 user prompt。
func BuildJudgePrompt(config PromptConfig, msg *core.Message) (system, user string) {
	system = fmt.Sprintf(`你是 %s 的人格判断器。
你的人设：%s
你关注的话题：%s

你正在浏览时间线，看到了一条帖子。
判断你是否会自然地想回复这条帖子（不是必须回复，是"有兴趣参与"）。
只输出一行：YES 或 NO，后面跟一句话理由。
不要输出其他任何内容。`,
		config.BotName,
		config.BotPersona,
		strings.Join(config.Interests, "、"))

	displayName := msg.UserID
	if name, ok := msg.Metadata["display_name"].(string); ok && name != "" {
		displayName = name
	}

	user = fmt.Sprintf("@%s: %s", displayName, msg.Text)
	return system, user
}

// ParseJudgeResponse 解析 LLM 快判的回复文本。
// 期望格式："YES 理由" 或 "NO 理由"。
func ParseJudgeResponse(text string) JudgeResult {
	text = strings.TrimSpace(text)
	upper := strings.ToUpper(text)

	if strings.HasPrefix(upper, "YES") {
		reason := strings.TrimSpace(text[3:])
		return JudgeResult{Engage: true, Reason: reason}
	}

	if strings.HasPrefix(upper, "NO") {
		reason := strings.TrimSpace(text[2:])
		if reason == "" {
			reason = "declined"
		}
		return JudgeResult{Engage: false, Reason: reason}
	}

	// 无法解析，保守拒绝
	return JudgeResult{Engage: false, Reason: "unparseable response"}
}

// ============================================================================
// SimpleLLMClient — 最小 LLM 客户端接口
// ============================================================================

// SimpleLLMClient 是一个最小化的 LLM 客户端接口。
// 只需要一个 Chat 方法，用于 Tier 2 快判。
// 实现者可以包装现有的 llm.Provider，只传 system + user 两条消息。
type SimpleLLMClient interface {
	// Chat 发送 system + user 消息，返回回复文本。
	Chat(ctx context.Context, system, user string) (string, error)
}

// SimpleJudge 是 LLMJudge 的默认实现，使用 SimpleLLMClient。
type SimpleJudge struct {
	client SimpleLLMClient
	config PromptConfig
}

// NewSimpleJudge 创建基于 SimpleLLMClient 的快判器。
func NewSimpleJudge(client SimpleLLMClient, config PromptConfig) *SimpleJudge {
	return &SimpleJudge{
		client: client,
		config: config,
	}
}

// Judge 实现 LLMJudge。
func (j *SimpleJudge) Judge(ctx context.Context, msg *core.Message) (JudgeResult, error) {
	system, user := BuildJudgePrompt(j.config, msg)

	resp, err := j.client.Chat(ctx, system, user)
	if err != nil {
		return JudgeResult{}, errs.Wrap(err, "llm judge")
	}

	return ParseJudgeResponse(resp), nil
}
