package openai

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func toolChoiceTestParams(model string, choice any) llm.GenerateParams {
	return llm.GenerateParams{
		Model:      llm.ChatModel(model),
		Messages:   []llm.Message{llm.UserMessage("ping")},
		Tools:      []llm.Tool{{Name: "ping", Description: "ping"}},
		ToolChoice: choice,
	}
}

// Chat Completions 与 Responses API 两条请求构造路径都必须降级，
// 否则防偷懒门禁的首步 tool_choice=required 会把整轮请求打成 400。
func TestParamsToChatRequest_ToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		choice any
		want   string
	}{
		{"required degraded to auto", "deepseek-flash", "required", `"auto"`},
		{"other model keeps required", "glm-5.3", "required", `"required"`},
		{"none unchanged", "deepseek-flash", "none", `"none"`},
		{"nil omits the field", "deepseek-flash", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := toolChoiceTestParams(tt.model, tt.choice)
			req, err := paramsToChatRequest(&p)
			if err != nil {
				t.Fatalf("paramsToChatRequest: %v", err)
			}
			if got := string(req.ToolChoice); got != tt.want {
				t.Fatalf("tool_choice = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParamsToOpenAIRequest_ToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		choice any
		want   string
	}{
		{"required degraded to auto", "deepseek-flash", "required", `"auto"`},
		{"other model keeps required", "glm-5.3", "required", `"required"`},
		{"none unchanged", "deepseek-flash", "none", `"none"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := toolChoiceTestParams(tt.model, tt.choice)
			req, err := paramsToOpenAIRequest(&p)
			if err != nil {
				t.Fatalf("paramsToOpenAIRequest: %v", err)
			}
			if got := string(req.ToolChoice); got != tt.want {
				t.Fatalf("tool_choice = %s, want %s", got, tt.want)
			}
		})
	}
}
