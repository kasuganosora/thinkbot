package api

import (
	"context"

	"github.com/kasuganosora/thinkbot/llm"
)

// llmJudgeAdapter 包装 llm.Provider 使其满足 engagement.SimpleLLMClient 接口。
// 用于 Tier 2 LLM 快判——只需一个简单的 system + user → text 调用。
type llmJudgeAdapter struct {
	provider llm.Provider
	model    *llm.Model
}

func newLLMJudgeAdapter(provider llm.Provider, modelID string) *llmJudgeAdapter {
	return &llmJudgeAdapter{
		provider: provider,
		model:    &llm.Model{ID: modelID},
	}
}

// Chat 发送 system + user 消息，返回回复文本。
func (a *llmJudgeAdapter) Chat(ctx context.Context, system, user string) (string, error) {
	temp := 0.3
	maxTok := 100
	result, err := a.provider.DoGenerate(ctx, llm.GenerateParams{
		Model:       a.model,
		System:      system,
		Messages:    []llm.Message{llm.UserMessage(user)},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	})
	if err != nil {
		return "", err
	}
	return result.Text, nil
}
