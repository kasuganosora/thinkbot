package llm

import (
	"context"
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
// 参照 agent/engagement.SimpleJudge 的成熟模式：极简 prompt、YES/NO 文本
// 协议、无法解析时保守拒绝（fail-closed）。关键词匹配仅保留为零成本快速
// 通道（命中即放行，省一次 LLM 调用）；未命中才走本判定器。
//
// 判错/超时/无法解析一律视为「未授权」——护栏宁可误拦也不误放。

// IntentJudgeClient 是意图快判的最小 LLM 客户端接口（与 engagement.
// SimpleLLMClient 同构，但 llm 包不依赖 agent，故独立声明）。
type IntentJudgeClient interface {
	Chat(ctx context.Context, system, user string) (string, error)
}

// providerIntentJudge 把 llm.Provider 适配为 IntentJudgeClient。
type providerIntentJudge struct {
	prov   Provider
	model  *Model
}

// NewProviderIntentJudge 用现有 Provider（沿用会话模型）构建快判客户端。
// model 为 nil 时使用 Provider 默认模型。
func NewProviderIntentJudge(prov Provider, model *Model) IntentJudgeClient {
	return &providerIntentJudge{prov: prov, model: model}
}

func (a *providerIntentJudge) Chat(ctx context.Context, system, user string) (string, error) {
	temp := 0.3
	maxTok := 64
	result, err := a.prov.DoGenerate(WithStatsFeature(ctx, "intent_judge"), GenerateParams{
		Model:       a.model,
		System:      system,
		Messages:    []Message{UserMessage(user)},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	})
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// intentJudgeSystemPrompt 快判 system prompt。只回答 YES/NO，判定标准是
// 「用户是否在显式授权/要求执行社交写操作」，查询类不算。
const intentJudgeSystemPrompt = `你是社交平台写操作的用户意图审核员。给定用户对 AI 助手说的话，判定用户是否显式地授权或要求执行社交写操作（发帖/发动态/关注/取关/点赞/转发/私信等）。

判定标准：
- YES：用户明确要求/授权执行某个写操作。如「发条 misskey」「帮我关注他」「发一个过去试试」「那就发吧」。
- NO：用户只是查询/闲聊/讨论，或授权的是只读操作。如「帮我查下 misskey 上的用户」「misskey 是什么」「看看他最近发了什么」。
- 语气反问但实质是授权（「那你尝试下发一个验证下？」）算 YES。
- 仅提到平台名、未落在写动作上的，一律 NO。

只回答 YES 或 NO 开头，后跟一句理由。`

// intentJudgeTimeout 快判超时。护栏在主链路关键路径上，必须快速失败；
// 超时即视为未授权（fail-closed）。10s 覆盖慢 provider，又不至于挂死编排。
const intentJudgeTimeout = 10 * time.Second

// IntentJudge 是写操作意图的 LLM 快判器。fail-closed：客户端错误、超时、
// 回复无法解析时一律判定为未授权。
type IntentJudge struct {
	client  IntentJudgeClient
	timeout time.Duration
}

// NewIntentJudge 创建意图快判器。timeout <=0 时默认 10s。
func NewIntentJudge(client IntentJudgeClient, timeout time.Duration) *IntentJudge {
	if timeout <= 0 {
		timeout = 10 * time.Second
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
	sb.WriteString("用户最新一条消息：\n")
	if userReq == "" {
		sb.WriteString("（空）\n")
	} else {
		sb.WriteString(userReq + "\n")
	}
	if len(recentUserMsgs) > 0 {
		sb.WriteString("\n用户在此之前的近期消息（供参考）：\n")
		for i, m := range recentUserMsgs {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, truncateForJudge(m)))
		}
	}
	sb.WriteString("\nAI 助手即将执行的工具：" + toolName + "\n")
	sb.WriteString("\n用户是否显式授权/要求此类写操作？回答 YES 或 NO。")

	cctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()
	resp, err := j.client.Chat(cctx, intentJudgeSystemPrompt, sb.String())
	if err != nil {
		return intentJudgeVerdict{grounded: false, reason: "intent judge error: " + err.Error()}
	}
	return parseIntentJudgeResponse(resp)
}

// parseIntentJudgeResponse 解析 YES/NO 回复；无法解析时保守拒绝。
func parseIntentJudgeResponse(text string) intentJudgeVerdict {
	text = strings.TrimSpace(text)
	upper := strings.ToUpper(text)
	switch {
	case strings.HasPrefix(upper, "YES"):
		return intentJudgeVerdict{grounded: true, reason: strings.TrimSpace(text[3:])}
	case strings.HasPrefix(upper, "NO"):
		return intentJudgeVerdict{grounded: false, reason: strings.TrimSpace(text[2:])}
	default:
		return intentJudgeVerdict{grounded: false, reason: "unparseable judge response"}
	}
}

func truncateForJudge(s string) string {
	if len(s) > 300 {
		return s[:300] + "...(truncated)"
	}
	return s
}
