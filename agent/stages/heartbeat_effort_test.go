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
	if got := replyEffort("medium", llm.ChatModel("glm-5.3")); got == nil || *got != "medium" {
		t.Fatal("configured values are sent unchanged")
	}
	if got := replyEffort("", llm.ChatModel("glm-5.3")); got != nil {
		t.Fatal("empty = not sent")
	}
	if got := replyEffort("none", llm.ChatModel("glm-5.3")); got == nil || *got != "low" {
		t.Fatalf("none on GLM-5.3 → low, got %v", got)
	}
	if got := replyEffort("none", llm.ChatModel("glm-5.2")); got == nil || *got != "none" {
		t.Fatalf("none on GLM-5.2 stays none, got %v", got)
	}
}
