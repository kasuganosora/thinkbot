package route

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// L1 是分类任务：必须显式压低推理档，否则推理型 light 模型会把 8s 路由预算
// 烧在 thinking 上，导致大量消息超时降级。
func TestL1RoutingUsesLowReasoningEffort(t *testing.T) {
	tests := []struct {
		model string
		want  string // "" 表示不发 reasoning_effort
	}{
		{"glm-5.2", "low"},
		{"glm-5.3", "low"},
		{"deepseek-flash", "low"},
		{"glm-4.5", ""},
		{"light-test", "low"},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			var got *string
			prov := &scriptProvider{reply: func(_ int, p llm.GenerateParams) (*llm.GenerateResult, error) {
				got = p.ReasoningEffort
				return &llm.GenerateResult{
					FinishReason: llm.FinishReasonToolCalls,
					ToolCalls: []llm.ToolCall{{
						ToolCallID: "c1", ToolName: ToolOpen,
						Input: map[string]any{"intent": "chat", "model": "light"},
					}},
				}, nil
			}}
			r, err := New(Config{
				Provider: prov,
				Model:    llm.ChatModel(tc.model),
				Limits:   Limits{MaxConcurrent: 2, MaxOpens: 4, OpenWindow: time.Minute},
			})
			if err != nil {
				t.Fatalf("new router: %v", err)
			}
			r.Route(context.Background(), userEnv("hello", "trace-effort-"+tc.model))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("reasoning_effort = %q, want omitted", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("reasoning_effort = %v, want %q", got, tc.want)
			}
		})
	}
}
