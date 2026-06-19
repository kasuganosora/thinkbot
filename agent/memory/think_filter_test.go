package memory

import (
	"context"
	"strings"
	"testing"
)

// ============================================================================
// StripThinkTags
// ============================================================================

func TestStripThinkTags_Complete(t *testing.T) {
	input := "<think>I need to analyze this carefully</think>Hello world"
	want := "Hello world"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_Thinking(t *testing.T) {
	input := "<thinking>Let me think about this</thinking>Final answer"
	want := "Final answer"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_Multiline(t *testing.T) {
	input := "<think>\nStep 1: Read input\nStep 2: Process\n</think>\nThe result is 42"
	want := "The result is 42"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_MultipleBlocks(t *testing.T) {
	input := "<think>reasoning 1</think>Answer A<think>more reasoning</think>Answer B"
	want := "Answer AAnswer B"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_CaseInsensitive(t *testing.T) {
	cases := []string{
		"<Think>secret</Think>visible",
		"<THINKING>secret</THINKING>visible",
		"<ThInK>secret</ThInK>visible",
	}
	for _, input := range cases {
		got := StripThinkTags(input)
		if got != "visible" {
			t.Errorf("case-insensitive: got %q, want %q", got, "visible")
		}
	}
}

func TestStripThinkTags_OnlyThink(t *testing.T) {
	input := "<think>This is all reasoning, no actual answer</think>"
	got := StripThinkTags(input)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestStripThinkTags_UnclosedTag(t *testing.T) {
	input := "Hello<think>this was cut off"
	want := "Hello"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("unclosed tag: got %q, want %q", got, want)
	}
}

func TestStripThinkTags_NoTags(t *testing.T) {
	input := "Just normal text without any tags"
	got := StripThinkTags(input)
	if got != input {
		t.Errorf("untagged text should be unchanged: got %q, want %q", got, input)
	}
}

func TestStripThinkTags_EmptyString(t *testing.T) {
	got := StripThinkTags("")
	if got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
}

func TestStripThinkTags_PreservesContentBetweenTags(t *testing.T) {
	input := "Before<think>reasoning</think>Middle<think>more</think>After"
	want := "BeforeMiddleAfter"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ============================================================================
// StripReasoningArray
// ============================================================================

func TestStripReasoningArray_ValidArray(t *testing.T) {
	input := `[{"text":"internal reasoning here","type":"reasoning"},{"text":"final answer","type":"text"}]`
	want := "final answer"
	got := StripReasoningArray(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripReasoningArray_MultipleTextParts(t *testing.T) {
	input := `[{"text":"thinking...","type":"reasoning"},{"text":"part1","type":"text"},{"text":"part2","type":"text"}]`
	want := "part1\npart2"
	got := StripReasoningArray(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripReasoningArray_NotAnArray(t *testing.T) {
	input := "Just a normal message"
	got := StripReasoningArray(input)
	if got != input {
		t.Errorf("non-array input should be unchanged: got %q", got)
	}
}

func TestStripReasoningArray_OnlyReasoning(t *testing.T) {
	input := `[{"text":"only reasoning","type":"reasoning"}]`
	want := ""
	got := StripReasoningArray(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripReasoningArray_NoReasoningType(t *testing.T) {
	// Array without any reasoning type → return as-is
	input := `[{"text":"just text","type":"text"}]`
	got := StripReasoningArray(input)
	if got != input {
		t.Errorf("array without reasoning type should be unchanged: got %q", got)
	}
}

func TestStripReasoningArray_InvalidJSON(t *testing.T) {
	input := "[{invalid json}]"
	got := StripReasoningArray(input)
	if got != input {
		t.Errorf("invalid JSON should be unchanged: got %q", got)
	}
}

// ============================================================================
// StripThinking (组合)
// ============================================================================

func TestStripThinking_Combined(t *testing.T) {
	input := `[{"text":"reasoning","type":"reasoning"},{"text":"<think>still thinking</think>actual content","type":"text"}]`
	want := "actual content"
	got := StripThinking(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinking_PlainText(t *testing.T) {
	input := "Hello, this is a normal response."
	got := StripThinking(input)
	if got != input {
		t.Errorf("plain text should be unchanged: got %q", got)
	}
}

// ============================================================================
// ThinkFilterStore
// ============================================================================

func TestThinkFilterStore_StripsOnAppend(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	entry := Entry{
		Scope:   ChannelScope("test"),
		Content: "<think>internal reasoning</think>actual memory content",
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

func TestThinkFilterStore_StripsReasoningArray(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	entry := Entry{
		Scope: ChannelScope("test"),
		Content: `[{"text":"reasoning","type":"reasoning"},{"text":"clean content","type":"text"}]`,
	}

	store.Append(context.Background(), entry)

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	if results[0].Content != "clean content" {
		t.Errorf("content not filtered: got %q", results[0].Content)
	}
}

func TestThinkFilterStore_PassthroughDelete(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	store.Append(context.Background(), Entry{
		Scope:   ChannelScope("test"),
		Content: "hello",
	})

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry before delete, got %d", len(results))
	}

	store.Delete(context.Background(), ChannelScope("test"), results[0].ID)

	results, _ = repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 0 {
		t.Errorf("expected 0 entries after delete, got %d", len(results))
	}
}

func TestThinkFilterStore_PassthroughClear(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	store.Append(context.Background(), Entry{Scope: ChannelScope("test"), Content: "a"})
	store.Append(context.Background(), Entry{Scope: ChannelScope("test"), Content: "b"})

	store.Clear(context.Background(), ChannelScope("test"))

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 10)
	if len(results) != 0 {
		t.Errorf("expected 0 entries after clear, got %d", len(results))
	}
}

func TestThinkFilterStore_EmptyAfterStrip(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	// Content is entirely think tags → after stripping it's empty
	store.Append(context.Background(), Entry{
		Scope:   ChannelScope("test"),
		Content: "<think>only reasoning, nothing useful</think>",
	})

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry (empty content still stored), got %d", len(results))
	}
	if results[0].Content != "" {
		t.Errorf("expected empty content after strip, got %q", results[0].Content)
	}
}

// 验证大段中文 think 标签被正确处理
func TestStripThinkTags_ChineseContent(t *testing.T) {
	input := `<think>用户问的是Go语言的并发模型，我需要解释goroutine和channel</think>Go语言的并发主要通过goroutine和channel实现。`
	want := "Go语言的并发主要通过goroutine和channel实现。"
	got := StripThinkTags(input)
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want to contain %q", got, want)
	}
}
