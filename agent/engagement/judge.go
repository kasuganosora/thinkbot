package engagement

import (
	"context"
	"fmt"
	"strconv"
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
	// Score 贡献价值评分（0-100）。
	// 0 表示未使用评分模式（传统 YES/NO）。
	// 1-100 表示 LLM 评估的兴趣程度（越高越值得参与）。
	// 参考 Houde et al. (2025) 论文：评分制 + 可配置阈值比二元 YES/NO 更受用户认可。
	Score int
}

// LLMJudge 使用轻量 LLM 调用快速判断消息是否值得主动参与。
//
// 这一层是可选的——只有在 Tier 1 规则通过后才调用。
// 使用便宜/快速模型，prompt 极简，只返回 YES/NO 或 0-100 分数 + 理由。
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

// BuildJudgePrompt 构建 LLM 快判的 system prompt 和 user prompt（传统 YES/NO 模式）。
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

	user = buildUserPrompt(msg)
	return system, user
}

// BuildScoredJudgePrompt 构建评分模式的 system prompt 和 user prompt。
//
// 参考 Houde et al. (2025) "Controlling AI Agent Participation in Group
// Conversations" — 论文研究二中发现 0-100 评分 + 可配置阈值
// (HIGH=90/MEDIUM=75/LOW=50) 是最受用户认可的控制方式。
//
// LLM 返回格式：分数 + 理由（如 "85 这是关于 golang 的深入讨论"）
func BuildScoredJudgePrompt(config PromptConfig, msg *core.Message) (system, user string) {
	system = fmt.Sprintf(`你是 %s 的人格判断器。
你的人设：%s
你关注的话题：%s

你正在浏览时间线，看到了一条帖子。
评估你对这条帖子的兴趣程度（0-100 分）：
- 90-100: 非常想参与，话题正是你关注的领域
- 70-89:  比较想参与，有相关的知识点或看法可以分享
- 50-69:  有点兴趣，但不确定是否有价值回复
- 30-49:  不太想参与，话题与你关系不大
- 0-29:   完全不感兴趣

只输出一行：分数 + 一句话理由（如 "85 这是关于 golang 的深入讨论"）
不要输出其他任何内容。`,
		config.BotName,
		config.BotPersona,
		strings.Join(config.Interests, "、"))

	user = buildUserPrompt(msg)
	return system, user
}

// buildUserPrompt 构建用户 prompt 部分（两种模式共用）。
func buildUserPrompt(msg *core.Message) string {
	displayName := msg.UserID
	if name, ok := msg.Metadata["display_name"].(string); ok && name != "" {
		displayName = name
	}
	return fmt.Sprintf("@%s: %s", displayName, msg.Text)
}

// ParseJudgeResponse 解析传统 YES/NO 模式的回复。
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

// ParseScoredResponse 解析评分模式的回复文本。
// 支持格式：
//   - "85 这是关于 golang 的讨论"
//   - "85: 这是关于 golang 的讨论"
//   - "Score: 85 - 这是关于 golang 的讨论"
//
// 当无法提取数字时，回退到 YES/NO 解析（向后兼容）。
func ParseScoredResponse(text string) JudgeResult {
	text = strings.TrimSpace(text)

	// 去除常见前缀
	for _, prefix := range []string{"Score:", "SCORE:", "评分:", "分数:"} {
		text = strings.TrimPrefix(text, prefix)
	}
	text = strings.TrimSpace(text)

	// 提取前导数字
	i := 0
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}

	if i == 0 {
		// 没有前导数字，回退到 YES/NO 解析
		return ParseJudgeResponse(text)
	}

	score, err := strconv.Atoi(text[:i])
	if err != nil || score < 0 || score > 100 {
		return JudgeResult{Engage: false, Score: 0, Reason: "invalid score: " + text[:i]}
	}

	reason := strings.TrimSpace(text[i:])
	reason = strings.TrimPrefix(reason, ":")
	reason = strings.TrimPrefix(reason, " -")
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = fmt.Sprintf("score %d", score)
	}

	// Engage 默认基于 score >= 50（可被 CompositePolicy 的 threshold 覆盖）
	return JudgeResult{
		Engage: score >= 50,
		Score:  score,
		Reason: reason,
	}
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
//
// 支持两种模式：
//   - 传统模式（scored=false）：YES/NO 判断
//   - 评分模式（scored=true）：0-100 评分，配合 CompositePolicy 的 threshold 使用
type SimpleJudge struct {
	client SimpleLLMClient
	config PromptConfig
	scored bool // true = 使用评分模式
}

// NewSimpleJudge 创建基于 SimpleLLMClient 的传统 YES/NO 快判器。
func NewSimpleJudge(client SimpleLLMClient, config PromptConfig) *SimpleJudge {
	return &SimpleJudge{
		client: client,
		config: config,
	}
}

// NewScoredSimpleJudge 创建基于 SimpleLLMClient 的评分快判器。
// 返回 0-100 分数，配合 CompositePolicy 的 engagementThreshold 使用。
func NewScoredSimpleJudge(client SimpleLLMClient, config PromptConfig) *SimpleJudge {
	return &SimpleJudge{
		client: client,
		config: config,
		scored: true,
	}
}

// IsScored 返回是否使用评分模式。
func (j *SimpleJudge) IsScored() bool {
	return j.scored
}

// Judge 实现 LLMJudge。
func (j *SimpleJudge) Judge(ctx context.Context, msg *core.Message) (JudgeResult, error) {
	var system, user string
	if j.scored {
		system, user = BuildScoredJudgePrompt(j.config, msg)
	} else {
		system, user = BuildJudgePrompt(j.config, msg)
	}

	resp, err := j.client.Chat(ctx, system, user)
	if err != nil {
		return JudgeResult{}, errs.Wrap(err, "llm judge")
	}

	if j.scored {
		return ParseScoredResponse(resp), nil
	}
	return ParseJudgeResponse(resp), nil
}
