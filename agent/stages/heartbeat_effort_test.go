package stages

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestHeartbeatEffort(t *testing.T) {
	cur := "medium"
	if got := heartbeatEffort("", llm.ChatModel("glm-5.3"), &cur); got != &cur {
		t.Fatal("empty setting keeps the reply effort")
	}
	if got := heartbeatEffort("provider", llm.ChatModel("glm-5.3"), &cur); got != nil {
		t.Fatal("provider = not sent")
	}
	if got := heartbeatEffort("none", llm.ChatModel("glm-5.3"), &cur); got == nil || *got != "low" {
		t.Fatalf("GLM-5.3 maps none to low: %v", got)
	}
	if got := heartbeatEffort("low", llm.ChatModel("glm-4.6"), &cur); got != nil {
		t.Fatal("models without reasoning_effort support get nothing")
	}
}

func TestReplyEffort(t *testing.T) {
	cases := []struct {
		name, effort, model, want string
	}{
		{"medium mapped into low/high/max vocabulary", "medium", "glm-5.3", "high"},
		{"unknown family unchanged", "medium", "gpt-4o", "medium"},
		{"max kept", "max", "glm-5.3", "max"},
		{"empty = not sent", "", "glm-5.3", ""},
		{"provider = not sent", "provider", "glm-5.3", ""},
		{"off = not sent", "off", "glm-5.3", ""},
		{"default = not sent", "default", "glm-5.3", ""},
		{"no-effort family = not sent", "low", "glm-4.6", ""},
		{"none on GLM-5.3 → low", "none", "glm-5.3", "low"},
		{"none on GLM-5.2 stays none", "none", "glm-5.2", "none"},
		{"DeepSeek medium → high", "medium", "deepseek-flash", "high"},
		{"DeepSeek none → low", "none", "deepseek-v4-pro", "low"},
		{"DeepSeek with namespace", "medium", "vendor/deepseek-v4-pro", "high"},
	}
	for _, c := range cases {
		got := replyEffort(c.effort, llm.ChatModel(c.model))
		gotStr := ""
		if got != nil {
			gotStr = *got
		}
		if gotStr != c.want {
			t.Errorf("%s: got %q, want %q", c.name, gotStr, c.want)
		}
	}
}
