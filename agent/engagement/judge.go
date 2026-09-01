package engagement

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// LLMJudge — Tier 2 LLM 快判
// ============================================================================

// JudgeRecord 一次判定的结果快照，供落库观测判定质量。
//
// 为什么需要：判定结果此前只用于派生「参不参与」的决策，用完即弃——
// 改了 prompt、换了模型，无从判断变好还是变坏。落库后可以按 bot / model
// 分组统计判定率与分数分布，把「调 prompt」从拍脑袋变成可度量。
//
// 刻意不做统计检验（p 值 / 显著性）：这里的样本量支撑不了 A/B 结论，
// 用小样本算 p 值是制造虚假确定性。只做描述性统计（率与分布），
// 判断交给人。
type JudgeRecord struct {
	BotID     string
	Channel   string
	Model     string // 可能为空，见 SimpleJudge.modelID
	Engage    bool
	Score     int // 0 = 未使用评分模式
	Reason    string
	Tier      string
	LatencyMS int64
}

// JudgeRecordSink 判定结果的落库目标。
//
// 接口定义在**消费方**（本包）而非实现方，实现由 stats 包提供——
// 这样本包不必依赖 stats，包边界保持清晰。
//
// 实现必须满足：非阻塞、失败不影响主决策。落库是旁路观测，
// 不能让它把参与决策拖垮。
type JudgeRecordSink interface {
	RecordJudge(ctx context.Context, rec JudgeRecord)
}

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

	// Model 实际使用的模型标识（可能为空——SimpleLLMClient 接口不暴露模型）。
	// 供落库后按模型分组评估判定质量：换模型后判定率是变好还是变坏，
	// 没有这一列就无从归因。
	Model string

	// LatencyMS 本次判定的耗时（毫秒）。
	// 快判在主链路的关键路径上，耗时本身就是需要观测的指标。
	LatencyMS int64
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
		BotPersona: "a friendly chat bot",
		Interests:  []string{},
	}
}

// BuildJudgePrompt 构建 LLM 快判的 system prompt 和 user prompt（传统 YES/NO 模式）。
func BuildJudgePrompt(config PromptConfig, msg *core.Message) (system, user string) {
	system = fmt.Sprintf(`You are the persona judge for %s, a gatekeeper that decides whether this bot would naturally want to join a conversation.

Persona: %s
Topics of interest: %s

You are browsing a timeline and have just seen one post. Decide whether you would naturally want to reply to it — not whether you are obligated to reply, but whether you are genuinely interested in participating.

# Output format

Output exactly one line: YES or NO, followed by a one-sentence reason.

<example>
YES the post is a deep discussion about golang, which is squarely my area
</example>

<example>
NO the post is small talk unrelated to anything I follow
</example>

CRITICAL: NEVER output anything other than that single line.`,
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
	system = fmt.Sprintf(`You are the persona judge for %s, a scorer that rates how interested this bot would be in a post.

Persona: %s
Topics of interest: %s

You are browsing a timeline and have just seen one post. Score your interest in it from 0 to 100:

- 90-100: Strongly want to engage — the topic is squarely in your area of interest
- 70-89:  Want to engage — you have relevant knowledge or a view worth sharing
- 50-69:  Mildly interested, but unsure a reply would add value
- 30-49:  Not really interested — the topic has little to do with you
- 0-29:   Not interested at all

# Output format

Output exactly one line: the score, then a one-sentence reason.

<example>
85 这是关于 golang 的深入讨论
</example>

<example>
20 话题与我关注的领域无关
</example>

CRITICAL: NEVER output anything other than that single line.`,
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
	// modelID 快判所用模型的标识，仅用于落库归因。
	// 为空表示调用方未提供——SimpleLLMClient 接口不暴露模型，
	// 故只能由构造方显式传入。落库时该列为空不影响其余字段。
	modelID string
	// sink 判定结果落库目标（可选，nil 则不落库）。
	sink JudgeRecordSink
}

// JudgeModelOption 配置 SimpleJudge 的可选项。
type JudgeModelOption func(*SimpleJudge)

// WithJudgeModel 设置快判所用模型的标识，用于判定结果的落库归因。
// 不设置时 Model 列为空，判定结果仍会落库（其余维度可用）。
func WithJudgeModel(modelID string) JudgeModelOption {
	return func(j *SimpleJudge) { j.modelID = modelID }
}

// WithJudgeSink 设置判定结果的落库目标。
//
// 不设置则判定结果用完即弃——改动前的行为。设置后每次判定都会落库，
// 使「改 prompt / 换模型后判定质量是变好还是变坏」成为可回答的问题。
func WithJudgeSink(sink JudgeRecordSink) JudgeModelOption {
	return func(j *SimpleJudge) { j.sink = sink }
}

// ModelID 返回快判所用模型标识（可能为空）。
func (j *SimpleJudge) ModelID() string { return j.modelID }

// NewSimpleJudge 创建基于 SimpleLLMClient 的传统 YES/NO 快判器。
// opts 可设置模型标识（WithJudgeModel）与落库目标（WithJudgeSink）。
func NewSimpleJudge(client SimpleLLMClient, config PromptConfig, opts ...JudgeModelOption) *SimpleJudge {
	j := &SimpleJudge{
		client: client,
		config: config,
	}
	for _, o := range opts {
		o(j)
	}
	return j
}

// NewScoredSimpleJudge 创建基于 SimpleLLMClient 的评分快判器。
// 返回 0-100 分数，配合 CompositePolicy 的 engagementThreshold 使用。
func NewScoredSimpleJudge(client SimpleLLMClient, config PromptConfig, opts ...JudgeModelOption) *SimpleJudge {
	j := &SimpleJudge{
		client: client,
		config: config,
		scored: true,
	}
	for _, o := range opts {
		o(j)
	}
	return j
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

	start := time.Now()
	resp, err := j.client.Chat(ctx, system, user)
	latencyMS := time.Since(start).Milliseconds()
	if err != nil {
		return JudgeResult{}, errs.Wrap(err, "llm judge")
	}

	var res JudgeResult
	if j.scored {
		res = ParseScoredResponse(resp)
	} else {
		res = ParseJudgeResponse(resp)
	}
	res.Model = j.modelID
	res.LatencyMS = latencyMS

	// 落库放在这里而非 Policy 层：判定与记录在一起才不会漏，
	// 也不要求调用方记得额外接线。记录的是**原始判定**（engage/score），
	// 不含阈值判断——保留原始值才能事后评估阈值定得合不合理。
	//
	// 非阻塞且失败不影响返回：落库是旁路观测，不能拖垮参与决策。
	if j.sink != nil {
		j.sink.RecordJudge(ctx, JudgeRecord{
			BotID:     msg.BotID,
			Channel:   msg.Channel,
			Model:     res.Model,
			Engage:    res.Engage,
			Score:     res.Score,
			Reason:    res.Reason,
			Tier:      string(TierLLM),
			LatencyMS: res.LatencyMS,
		})
	}

	return res, nil
}
