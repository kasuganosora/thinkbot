package memory

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// StripToolMessages
// ============================================================================

func TestStripToolMessages_RemovesToolRoleMessages(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("hello"),
		llm.AssistantMessage("let me search"),
		llm.ToolMessage(llm.ToolResultPart{
			ToolCallID: "call-001",
			ToolName:   "search",
			Result:     `{"results": ["item1", "item2", "item3", "...very long output..."]}`,
		}),
		llm.AssistantMessage("based on the search results, the answer is 42"),
	}

	got := StripToolMessages(msgs)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages (tool role removed), got %d", len(got))
	}
	if got[2].Role != llm.MessageRoleAssistant {
		t.Errorf("last message should be assistant, got %s", got[2].Role)
	}
}

func TestStripToolMessages_RemovesToolPartsFromAssistant(t *testing.T) {
	msg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.MessagePart{
			llm.TextPart{Text: "I'll call a tool"},
			llm.ToolCallPart{
				ToolCallID: "call-001",
				ToolName:   "calculator",
				Input:      map[string]any{"expr": "1+1"},
			},
			llm.TextPart{Text: "Done"},
		},
	}

	got := StripToolMessages([]llm.Message{msg})
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if len(got[0].Content) != 2 {
		t.Fatalf("expected 2 parts (tool call removed), got %d", len(got[0].Content))
	}
	if _, ok := got[0].Content[0].(llm.TextPart); !ok {
		t.Errorf("first part should be TextPart")
	}
	if _, ok := got[0].Content[1].(llm.TextPart); !ok {
		t.Errorf("second part should be TextPart")
	}
}

func TestStripToolMessages_RemovesToolResultParts(t *testing.T) {
	msg := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.MessagePart{
			llm.TextPart{Text: "question"},
			llm.ToolResultPart{
				ToolCallID: "call-001",
				ToolName:   "search",
				Result:     "long search results...",
			},
		},
	}

	got := StripToolMessages([]llm.Message{msg})
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if len(got[0].Content) != 1 {
		t.Fatalf("expected 1 part (tool result removed), got %d", len(got[0].Content))
	}
}

func TestStripToolMessages_DropsEmptyMessages(t *testing.T) {
	// 消息仅含 ToolCallPart → 过滤后 Content 为空 → 整条消息被丢弃
	msg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.MessagePart{
			llm.ToolCallPart{
				ToolCallID: "call-001",
				ToolName:   "search",
				Input:      map[string]any{"q": "test"},
			},
		},
	}

	got := StripToolMessages([]llm.Message{msg})
	if len(got) != 0 {
		t.Fatalf("expected 0 messages (empty after filtering), got %d", len(got))
	}
}

func TestStripToolMessages_PreservesNormalMessages(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("hello"),
		llm.AssistantMessage("hi there"),
	}

	got := StripToolMessages(msgs)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages unchanged, got %d", len(got))
	}
}

func TestStripToolMessages_EmptyInput(t *testing.T) {
	got := StripToolMessages(nil)
	if len(got) != 0 {
		t.Errorf("expected 0 messages for nil input, got %d", len(got))
	}
}

// ============================================================================
// StripToolOutputFromText
// ============================================================================

func TestStripToolOutputFromText_ToolCallTag(t *testing.T) {
	input := "<tool_call>{\"name\": \"search\", \"args\": {\"q\": \"test\"}}</tool_call>Final answer"
	want := "Final answer"
	got := StripToolOutputFromText(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToolOutputFromText_FunctionCallTag(t *testing.T) {
	input := "<function_call>calc(1+1)</function_call>The answer is 2"
	want := "The answer is 2"
	got := StripToolOutputFromText(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToolOutputFromText_ToolResultTag(t *testing.T) {
	input := "Before<tool_result>{\"output\": \"very long data...\"}</tool_result>After"
	want := "BeforeAfter"
	got := StripToolOutputFromText(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToolOutputFromText_ToolResultWithAttributes(t *testing.T) {
	input := `<tool_result tool_use_id="call-001" name="search">{"results": []}</tool_result>Visible text`
	want := "Visible text"
	got := StripToolOutputFromText(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToolOutputFromText_MultipleBlocks(t *testing.T) {
	input := "<tool_call>call1</tool_call>A<tool_result>result1</tool_result>B<tool_call>call2</tool_call>C"
	want := "ABC"
	got := StripToolOutputFromText(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToolOutputFromText_UnclosedTag(t *testing.T) {
	input := "Hello<tool_call>this was cut off"
	want := "Hello"
	got := StripToolOutputFromText(input)
	if got != want {
		t.Errorf("unclosed tag: got %q, want %q", got, want)
	}
}

func TestStripToolOutputFromText_Multiline(t *testing.T) {
	input := "<tool_call>\n" +
		`{"name": "search", "args": {"q": "very long query"}}` + "\n" +
		"</tool_call>\n" +
		"Based on the results, I found 3 items."
	want := "Based on the results, I found 3 items."
	got := StripToolOutputFromText(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToolOutputFromText_NoTags(t *testing.T) {
	input := "Just normal conversation text"
	got := StripToolOutputFromText(input)
	if got != input {
		t.Errorf("untagged text should be unchanged: got %q, want %q", got, input)
	}
}

func TestStripToolOutputFromText_EmptyString(t *testing.T) {
	got := StripToolOutputFromText("")
	if got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
}

func TestStripToolOutputFromText_OnlyToolContent(t *testing.T) {
	input := "<tool_call>{\"name\": \"noop\"}</tool_call>"
	got := StripToolOutputFromText(input)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// ============================================================================
// StripToolOutput (组合)
// ============================================================================

func TestStripToolOutput_Combined(t *testing.T) {
	input := `[{"text":"reasoning","type":"reasoning"},{"text":"<tool_call>hidden call</tool_call>actual content","type":"text"}]`
	want := "actual content"
	got := StripToolOutput(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToolOutput_PlainText(t *testing.T) {
	input := "Hello, this is a normal response."
	got := StripToolOutput(input)
	if got != input {
		t.Errorf("plain text should be unchanged: got %q, want %q", got, input)
	}
}

// ============================================================================
// ToolOutputFilterStore
// ============================================================================

func TestToolOutputFilterStore_StripsOnAppend(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewToolOutputFilterStore(repo)

	entry := Entry{
		Scope:   ChannelScope("test"),
		Content: `<tool_call>{"name": "search", "args": {"q": "test"}}</tool_call>actual memory content`,
	}

	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatal(err)
	}

	results, err := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	if results[0].Content != "actual memory content" {
		t.Errorf("content not filtered: got %q", results[0].Content)
	}
}

func TestToolOutputFilterStore_StripsToolResult(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewToolOutputFilterStore(repo)

	entry := Entry{
		Scope: ChannelScope("test"),
		Content: `User asked about weather.` +
			`<tool_result tool_use_id="call-001">{"temp": 25, "humidity": 60}</tool_result>` +
			`The temperature is 25°C.`,
	}

	_ = store.Append(context.Background(), entry)

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	if results[0].Content != "User asked about weather.The temperature is 25°C." {
		t.Errorf("content not filtered: got %q", results[0].Content)
	}
}

func TestToolOutputFilterStore_PassthroughDelete(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewToolOutputFilterStore(repo)

	_ = store.Append(context.Background(), Entry{
		Scope:   ChannelScope("test"),
		Content: "hello",
	})

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry before delete, got %d", len(results))
	}

	_ = store.Delete(context.Background(), ChannelScope("test"), results[0].ID)

	results, _ = repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 0 {
		t.Errorf("expected 0 entries after delete, got %d", len(results))
	}
}

func TestToolOutputFilterStore_PassthroughClear(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewToolOutputFilterStore(repo)

	_ = store.Append(context.Background(), Entry{Scope: ChannelScope("test"), Content: "a"})
	_ = store.Append(context.Background(), Entry{Scope: ChannelScope("test"), Content: "b"})

	_ = store.Clear(context.Background(), ChannelScope("test"))

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 10)
	if len(results) != 0 {
		t.Errorf("expected 0 entries after clear, got %d", len(results))
	}
}

func TestToolOutputFilterStore_EmptyAfterStrip(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewToolOutputFilterStore(repo)

	_ = store.Append(context.Background(), Entry{
		Scope:   ChannelScope("test"),
		Content: `<tool_call>{"name": "noop"}</tool_call>`,
	})

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry (empty content still stored), got %d", len(results))
	}
	if results[0].Content != "" {
		t.Errorf("expected empty content after strip, got %q", results[0].Content)
	}
}

// ============================================================================
// 组合验证：ThinkFilterStore + ToolOutputFilterStore
// ============================================================================

func TestCompositeFilters_ThinkAndToolOutput(t *testing.T) {
	repo := NewMemoryRepository()
	// 先过滤 think 标签，再过滤工具输出（或反过来）
	store := NewThinkFilterStore(NewToolOutputFilterStore(repo))

	entry := Entry{
		Scope: ChannelScope("test"),
		Content: `<think>Let me analyze this</think>` +
			`<tool_call>{"name": "search", "args": {"q": "test"}}</tool_call>` +
			`The user likes Go programming.`,
	}

	_ = store.Append(context.Background(), entry)

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	if results[0].Content != "The user likes Go programming." {
		t.Errorf("content not fully filtered: got %q", results[0].Content)
	}
}
