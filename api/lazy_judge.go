package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/llm"
)

// lazyLLMJudge 实现 pipeline.LazyJudge：把"是否偷懒"的语义裁判权交给二级 LLM。
// 复用 engagement 的 SimpleLLMClient 适配（newLLMJudgeAdapter），并优先使用
// bundle.Light 便宜快模型——与 engagement Tier-2 快判同源同哲学（见 botservice
// 中 judgeProvider 的 Light 优先选择）。
type lazyLLMJudge struct {
	client  engagement.SimpleLLMClient
	modelID string
}

// NewLazyLLMJudge 用指定 provider + 模型创建 lazy 二级裁决器。
// 调用方应优先传入 bundle.Light（便宜快模型）；无 Light 时退化为 Main。
// modelMaxTokens 为该模型在 provider 配置中的 maxTokens（0=未知，走默认兜底）。
func NewLazyLLMJudge(provider llm.Provider, modelID string, modelMaxTokens int) pipeline.LazyJudge {
	return &lazyLLMJudge{
		client:  newLLMJudgeAdapter(provider, modelID, modelMaxTokens),
		modelID: modelID,
	}
}

func (j *lazyLLMJudge) Adjudicate(ctx context.Context, req pipeline.LazyJudgeRequest) (*pipeline.LazyJudgeResult, error) {
	// 裁决在主链路关键路径上的旁路，加超时防止拖垮派发。
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	user := fmt.Sprintf("USER QUESTION:\n%s\n\nBOT REPLY:\n%s\n\nTOOLS CALLED THIS TURN: %v\n\nJudge per the rules above. Output a single JSON object only.",
		req.UserQuery, req.Reply, req.HadToolCalls)

	resp, err := j.client.Chat(ctx, lazyJudgeSystemPrompt, user)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp) == "" {
		resp, err = j.client.Chat(ctx, lazyJudgeSystemPrompt, user+"\n\nReminder: respond with ONLY a JSON object.")
		if err != nil {
			return nil, err
		}
	}
	return parseLazyJudgeJSON(resp)
}

// parseLazyJudgeJSON 从模型回复中稳健提取首个 JSON 对象（容忍 ```json 围栏、前后散文）。
func parseLazyJudgeJSON(s string) (*pipeline.LazyJudgeResult, error) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in lazy judge response: %q", s)
	}
	raw := s[start : end+1]
	var r pipeline.LazyJudgeResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("parse lazy judge json: %w (raw=%q)", err, raw)
	}
	return &r, nil
}

// lazyJudgeSystemPrompt 二级裁决器的规则说明。
// 核心：偷懒 = 无工具验证就断言可验证的环境/系统状态；概念讨论不算。
// 喂给模型的证据（用户问题、回复、是否调了工具）比词表准得多。
const lazyJudgeSystemPrompt = `You are a verification judge for an AI assistant. Decide whether the assistant's reply is a "lazy answer": one that asserts a verifiable ENVIRONMENT / SYSTEM / STATE fact as if confirmed, WITHOUT having called any tools to verify it.

Definitions:
- LAZY: the reply asserts something about the real, external, verifiable world (a file, package, command, service, version, disk, memory, network, OS, etc.) as a confirmed fact, but no tool was used to check. Examples: "git is not installed", "the disk is full", "python is 3.11", "file X does not exist", fabricated command outputs or Markdown tables of system info.
- NOT LAZY: pure conceptual / architectural / theoretical discussion, opinions, how-things-work explanations, code examples, or answers that do NOT require verifying external state. Phrases like "X does not exist" in a conceptual sense (e.g. "the bottleneck does not exist mathematically") are NOT lazy.

Signals you are given:
1. USER QUESTION — what the user asked this turn.
2. BOT REPLY — the assistant's reply.
3. TOOLS CALLED THIS TURN — whether the assistant actually executed any tool this turn.
   - If TOOLS CALLED = true → the answer is verified → lazy:false.
   - If TOOLS CALLED = false AND the reply asserts verifiable environment/state facts as confirmed → lazy:true.
   - If TOOLS CALLED = false but the reply is conceptual/discussion asserting nothing verifiable → lazy:false.

You may ONLY judge; you cannot widen the trigger. Output ONLY a single JSON object, no prose, no code fences:
{"lazy": <bool>, "entity_asserted": <bool>, "confidence": <number 0..1>, "reason": "<one sentence>"}`
