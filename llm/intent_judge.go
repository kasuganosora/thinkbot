package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// 用户意图快判（写操作意图护栏 Layer B 的 LLM 判定器）
// ============================================================================
//
// 背景：RequiresUserIntent 写工具（misskey follow/post/react 等）原先靠
// 关键词子串匹配判定用户请求是否显式授权。口语措辞无穷（「发条 misskey」
// 「整一个过去」「那你就发呗」），关键词表永远追不完，导致正常授权被误杀。
//
// 参照 agent/engagement.SimpleJudge 的成熟模式：极简 prompt、严格输出协议、
// 无法解析时保守拒绝（fail-closed）。与自由文本 YES/NO 不同，本判定器要求
// LLM 输出严格 JSON（{"verdict":"YES|NO","reason":"..."}），解析后校验字段
// 合法性；不合规的回复丢弃并重试，重试仍不合规才判 NO（fail-closed）。
//
// 关键词匹配仅保留为零成本快速通道（命中即放行，省一次 LLM 调用）；
// 未命中才走本判定器。
//
// 判错/超时/重试耗尽一律视为「未授权」——护栏宁可误拦也不误放。

// IntentJudgeClient 是意图快判的最小 LLM 客户端接口（与 engagement.
// SimpleLLMClient 同构，但 llm 包不依赖 agent，故独立声明）。
type IntentJudgeClient interface {
	Chat(ctx context.Context, system, user string) (string, error)
}

// providerIntentJudge 把 llm.Provider 适配为 IntentJudgeClient。
type providerIntentJudge struct {
	prov            Provider
	model           *Model
	reasoningEffort *string
}

// NewProviderIntentJudge 用现有 Provider（沿用会话模型）构建快判客户端。
// model 为 nil 时使用 Provider 默认模型。reasoningEffort 沿用会话配置（nil = 不指定），
// 只传 provider 已经接受的取值，避免给不支持的接口塞新参数。
func NewProviderIntentJudge(prov Provider, model *Model, reasoningEffort *string) IntentJudgeClient {
	return &providerIntentJudge{prov: prov, model: model, reasoningEffort: reasoningEffort}
}

func (a *providerIntentJudge) Chat(ctx context.Context, system, user string) (string, error) {
	result, err := a.prov.DoGenerate(WithStatsFeature(ctx, "intent_judge"), GenerateParams{
		Model:           a.model,
		System:          system,
		Messages:        []Message{UserMessage(user)},
		ReasoningEffort: a.reasoningEffort,
	})
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// intentJudgeSystemPrompt 快判 system prompt。要求严格 JSON 输出，判定标准是
// 「用户是否在显式授权/要求执行社交写操作」，查询类不算。
const intentJudgeSystemPrompt = `You are a user-intent auditor for social-platform write actions. Given what the user said to the AI assistant, decide whether the user explicitly authorized or requested executing a social write action (post/publish status/follow/unfollow/like/repost/renote/DM, etc.).

Criteria:
- YES: The user explicitly requested or authorized executing a write action (e.g., "post a note on Misskey", "follow him for me", "send one to test", "go ahead and post it").
- NO: The user is only querying, chatting, or discussing, or only authorized a read-only action (e.g., "look up this user on Misskey", "what is Misskey", "check what they posted recently").
- A rhetorical question that substantively authorizes the action (e.g., "why don't you try posting one to verify?") counts as YES.
- Merely mentioning a platform name without requesting a write action is ALWAYS NO.

Output ONLY a single JSON object and nothing else, in the following format:
{"verdict":"YES or NO","reason":"one-sentence reason"}`

// intentJudgeTimeout 快判整体超时（含重试）。护栏在主链路关键路径上，必须
// 快速失败；超时即视为未授权（fail-closed）。10s 覆盖慢 provider 及一次
// 重试，又不至于挂死编排。
const intentJudgeTimeout = 10 * time.Second

// intentJudgeMaxAttempts 单次判定的最大尝试次数。LLM 回复不合规时丢弃
// 重试；达到上限仍不合规则 fail-closed 判 NO。
const intentJudgeMaxAttempts = 2

// IntentJudge 是写操作意图的 LLM 快判器。fail-closed：客户端错误、超时、
// 重试耗尽仍无法解析时一律判定为未授权。
type IntentJudge struct {
	client  IntentJudgeClient
	timeout time.Duration
}

// NewIntentJudge 创建意图快判器。timeout <= 0 时默认 10s。
func NewIntentJudge(client IntentJudgeClient, timeout time.Duration) *IntentJudge {
	if timeout <= 0 {
		timeout = intentJudgeTimeout
	}
	return &IntentJudge{client: client, timeout: timeout}
}

// intentJudgeVerdict 快判结果。
type intentJudgeVerdict struct {
	grounded bool
	reason   string
}

// Grounded 判定用户是否显式授权社交写操作。recentUserMsgs 是回看窗口内的
// 近期用户消息（按时间正序），供「上一轮授权、本轮换说法重试」场景。
// 任何错误都返回 grounded=false（fail-closed），错误以 reason 说明。
func (j *IntentJudge) Grounded(ctx context.Context, toolName, userReq string, recentUserMsgs []string) intentJudgeVerdict {
	var sb strings.Builder
	sb.WriteString("Latest user message:\n")
	if userReq == "" {
		sb.WriteString("(empty)\n")
	} else {
		sb.WriteString(userReq + "\n")
	}
	if len(recentUserMsgs) > 0 {
		sb.WriteString("\nRecent prior user messages (for context):\n")
		for i, m := range recentUserMsgs {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, truncateForJudge(m)))
		}
	}
	sb.WriteString("\nTool the AI assistant is about to invoke: " + toolName + "\n")
	sb.WriteString("\nDid the user explicitly authorize or request this kind of write action? Output JSON only.")

	cctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()

	var lastReason string
	for attempt := 1; attempt <= intentJudgeMaxAttempts; attempt++ {
		resp, err := j.client.Chat(cctx, intentJudgeSystemPrompt, sb.String())
		if err != nil {
			return intentJudgeVerdict{grounded: false, reason: "intent judge error: " + err.Error()}
		}
		verdict, ok := parseIntentJudgeResponse(resp)
		if ok {
			return verdict
		}
		// 回复不合规：丢弃，带错误提示重试一次。
		lastReason = fmt.Sprintf("attempt %d: invalid judge response: %s", attempt, truncateForJudge(resp))
	}
	return intentJudgeVerdict{grounded: false, reason: lastReason}
}

// intentJudgeReply 是 LLM 应输出的 JSON 结构。
type intentJudgeReply struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// parseIntentJudgeResponse 解析并校验 LLM 的 JSON 回复。返回 ok=false 表示
// 回复不合规（应丢弃重试 / fail-closed）。
//
// 校验规则：
//   - 必须是合法 JSON 对象（允许首尾空白，允许被 ```json 围栏包裹）；
//   - verdict 必须恰为 "YES" 或 "NO"（大小写不敏感），空串/其他值不合规；
//   - reason 为自由文本，仅截断长度，不作硬性要求。
func parseIntentJudgeResponse(text string) (intentJudgeVerdict, bool) {
	text = strings.TrimSpace(text)
	// 剥离 markdown 代码围栏（部分模型习惯性加）。
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = strings.TrimSpace(text[idx+1:])
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	var reply intentJudgeReply
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		return intentJudgeVerdict{}, false
	}
	switch strings.ToUpper(strings.TrimSpace(reply.Verdict)) {
	case "YES":
		return intentJudgeVerdict{grounded: true, reason: reply.Reason}, true
	case "NO":
		return intentJudgeVerdict{grounded: false, reason: reply.Reason}, true
	default:
		return intentJudgeVerdict{}, false
	}
}

// truncateForJudge 按字符（而非字节）截断，避免把中文切成非法 UTF-8。
func truncateForJudge(s string) string {
	r := []rune(s)
	if len(r) > 300 {
		return string(r[:300]) + "...(truncated)"
	}
	return s
}
