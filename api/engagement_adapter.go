package api

import (
	"context"

	"github.com/kasuganosora/thinkbot/llm"
)

// llmJudgeAdapter 包装 llm.Provider 使其满足 engagement.SimpleLLMClient 接口。
// 用于 Tier 2 LLM 快判——只需一个简单的 system + user → text 调用。
//
// maxTokens 取裁决模型在 provider 模型配置里的 maxTokens（ModelDef.MaxTokens）。
// 此前写死 100：思考模型（如 glm-5.2）推理 token 计入 max_tokens，100 常在推理阶段
// 就被截断，拿不到判定文本（lazy judge 的 JSON 也会被截断）。上限只是天花板，
// 判定本身很短，按实际输出计费。
//
// reasoning_effort / 输出封顶按 purpose（engagement_judge / lazy_judge）走 llm.InternalPolicy：
// 判定类默认 none（模型不支持时 low），避免 GLM 缺省 max 推理。
type llmJudgeAdapter struct {
	provider llm.Provider
	model    *llm.Model
	modelMax int
	policy   *llm.InternalPolicy
	purpose  string
}

func newLLMJudgeAdapter(provider llm.Provider, modelID string, modelMaxTokens int, policy *llm.InternalPolicy, purpose string) *llmJudgeAdapter {
	return &llmJudgeAdapter{
		provider: provider,
		model:    &llm.Model{ID: modelID},
		modelMax: modelMaxTokens,
		policy:   policy,
		purpose:  purpose,
	}
}

// Chat 发送 system + user 消息，返回回复文本。
func (a *llmJudgeAdapter) Chat(ctx context.Context, system, user string) (string, error) {
	temp := 0.3
	maxTok := a.policy.MaxTokens(a.purpose, a.modelMax, llm.DefaultMaxOutputTokens)
	params := llm.GenerateParams{
		Model:       a.model,
		System:      system,
		Messages:    []llm.Message{llm.UserMessage(user)},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}
	a.policy.Apply(a.purpose, &params)
	result, err := a.provider.DoGenerate(llm.WithStatsFeature(ctx, "engagement"), params)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}
