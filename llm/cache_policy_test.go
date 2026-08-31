package llm

import (
	"testing"
)

func TestApplyCachePolicy_None(t *testing.T) {
	params := &GenerateParams{
		Tools: []Tool{
			{Name: "a", CacheControl: EphemeralCacheControl()},
			{Name: "b"},
		},
		Messages: []Message{
			UserMessage("hello"),
		},
	}
	// Pre-set cache control on user message text
	if tp, ok := params.Messages[0].Content[0].(TextPart); ok {
		tp.CacheControl = EphemeralCacheControl()
		params.Messages[0].Content[0] = tp
	}

	ApplyCachePolicy(params, CachePolicyNone)

	for _, tool := range params.Tools {
		if tool.CacheControl != nil {
			t.Errorf("tool %s should have nil cache control", tool.Name)
		}
	}
	for _, msg := range params.Messages {
		for _, part := range msg.Content {
			if tp, ok := part.(TextPart); ok && tp.CacheControl != nil {
				t.Error("text part should have nil cache control")
			}
		}
	}
}

func TestApplyCachePolicy_Auto(t *testing.T) {
	params := &GenerateParams{
		System: "you are a helpful assistant",
		Tools: []Tool{
			{Name: "tool1"},
			{Name: "tool2"},
		},
		Messages: []Message{
			UserMessage("first message"),
			AssistantMessage("response"),
			UserMessage("second message"),
			AssistantMessage("response 2"),
			UserMessage("latest message"),
		},
	}

	ApplyCachePolicy(params, CachePolicyAuto)

	// System should have cache control
	if params.SystemCacheControl == nil {
		t.Error("system should have cache control")
	}

	// Tools: last tool should have cache control
	if params.Tools[1].CacheControl == nil {
		t.Error("last tool should have cache control")
	}
	if params.Tools[0].CacheControl != nil {
		t.Error("first tool should NOT have cache control")
	}

	// Last user message should have cache control on its text part
	lastMsg := params.Messages[4]
	hasCache := false
	for _, part := range lastMsg.Content {
		if tp, ok := part.(TextPart); ok && tp.CacheControl != nil {
			hasCache = true
		}
	}
	if !hasCache {
		t.Error("last user message should have cache control")
	}

	// Non-last messages should NOT have cache control
	for i := 0; i < len(params.Messages)-1; i++ {
		for _, part := range params.Messages[i].Content {
			if tp, ok := part.(TextPart); ok && tp.CacheControl != nil {
				t.Errorf("message at index %d should not have cache control", i)
			}
		}
	}
}

func TestApplyCachePolicy_Auto_BreakpointLimit(t *testing.T) {
	// Create many tools and messages to exceed 4 breakpoints.
	tools := make([]Tool, 10)
	for i := range tools {
		tools[i] = Tool{Name: "tool" + string(rune('A'+i))}
	}

	msgs := make([]Message, 10)
	for i := range msgs {
		msgs[i] = UserMessage("msg" + string(rune('A'+i)))
	}

	params := &GenerateParams{
		Tools:    tools,
		Messages: msgs,
	}

	ApplyCachePolicy(params, CachePolicyAuto)

	// Count breakpoints: should be at most MaxCacheBreakpoints.
	// With the latest-user-message strategy: system(0, none set) + tools(1) + last user(1) = 2 total.
	count := 0
	for _, tool := range params.Tools {
		if tool.CacheControl != nil {
			count++
		}
	}
	for _, msg := range params.Messages {
		if hasCacheControl(&msg) {
			count++
		}
	}

	if count > MaxCacheBreakpoints {
		t.Errorf("expected at most %d breakpoints, got %d", MaxCacheBreakpoints, count)
	}

	// Only last tool should have cache
	toolBreakpoints := 0
	for _, tool := range params.Tools {
		if tool.CacheControl != nil {
			toolBreakpoints++
		}
	}
	if toolBreakpoints != 1 {
		t.Errorf("expected 1 tool breakpoint (last tool only), got %d", toolBreakpoints)
	}
}

func TestFindLastUserMessageIndex(t *testing.T) {
	msgs := []Message{
		UserMessage("a"),
		AssistantMessage("b"),
		UserMessage("c"),
		AssistantMessage("d"),
		UserMessage("e"),
	}

	idx := findLastUserMessageIndex(msgs)
	if idx != 4 {
		t.Errorf("expected index 4, got %d", idx)
	}

	// No user messages
	msgs2 := []Message{
		AssistantMessage("a"),
		AssistantMessage("b"),
	}
	idx = findLastUserMessageIndex(msgs2)
	if idx != -1 {
		t.Errorf("expected -1, got %d", idx)
	}
}

func TestStripAllCacheControl(t *testing.T) {
	params := &GenerateParams{
		Tools: []Tool{
			{Name: "a", CacheControl: EphemeralCacheControl()},
		},
		Messages: []Message{
			{Role: MessageRoleUser, Content: []MessagePart{
				TextPart{Text: "hi", CacheControl: EphemeralCacheControl()},
			}},
		},
	}

	stripAllCacheControl(params)

	if params.Tools[0].CacheControl != nil {
		t.Error("tool cache should be stripped")
	}
	tp := params.Messages[0].Content[0].(TextPart)
	if tp.CacheControl != nil {
		t.Error("text part cache should be stripped")
	}
}

func TestShouldApplyCacheBreakpoints(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{"anthropic", true},
		{"bedrock", true},
		{"alibaba", true},
		{"google-vertex-anthropic", true},
		{"openai", false},
		{"google", false},
		{"grok", false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := ShouldApplyCacheBreakpoints(tt.provider); got != tt.want {
				t.Errorf("ShouldApplyCacheBreakpoints(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

func TestApplyCachePolicy_Auto_WithSystem(t *testing.T) {
	params := &GenerateParams{
		System: "you are an assistant",
		Messages: []Message{
			UserMessage("hello"),
		},
	}

	ApplyCachePolicy(params, CachePolicyAuto)

	// System should get a cache control
	if params.SystemCacheControl == nil {
		t.Error("system should have cache control when auto policy is applied")
	}

	// Last message should also get cache control
	lastMsg := params.Messages[len(params.Messages)-1]
	hasCache := false
	for _, part := range lastMsg.Content {
		if tp, ok := part.(TextPart); ok && tp.CacheControl != nil {
			hasCache = true
		}
	}
	if !hasCache {
		t.Error("last message should have cache control")
	}
}
