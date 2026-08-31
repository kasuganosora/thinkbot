package llm

import (
	"testing"
)

func TestPatchToolCalls_NoDangling(t *testing.T) {
	msgs := []Message{
		UserMessage("hello"),
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "call_1", ToolName: "search", Input: "foo"},
			},
		},
		ToolMessage(ToolResultPart{ToolCallID: "call_1", ToolName: "search", Result: "bar"}),
	}

	out := PatchToolCalls(msgs)
	if len(out) != len(msgs) {
		t.Errorf("expected unchanged (same length %d), got %d", len(msgs), len(out))
	}
}

func TestPatchToolCalls_ReturnsSameSliceWhenNoPatch(t *testing.T) {
	msgs := []Message{
		UserMessage("hello"),
		AssistantMessage("hi"),
	}
	out := PatchToolCalls(msgs)
	// Should return the exact same slice reference.
	if &out[0] != &msgs[0] {
		t.Errorf("expected same slice reference when no patching needed")
	}
}

func TestPatchToolCalls_AllDangling(t *testing.T) {
	msgs := []Message{
		UserMessage("search for foo"),
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "call_1", ToolName: "search", Input: "foo"},
				ToolCallPart{ToolCallID: "call_2", ToolName: "read", Input: "bar"},
			},
		},
	}

	out := PatchToolCalls(msgs)
	// Should be: user + assistant + tool(placeholder for both)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	if out[2].Role != MessageRoleTool {
		t.Fatalf("expected tool message at index 2, got %s", out[2].Role)
	}
	if len(out[2].Content) != 2 {
		t.Fatalf("expected 2 tool results, got %d", len(out[2].Content))
	}
	for _, part := range out[2].Content {
		tr := part.(ToolResultPart)
		if tr.Result != DefaultPatchPlaceholder {
			t.Errorf("expected placeholder, got %v", tr.Result)
		}
	}
}

func TestPatchToolCalls_PartialDangling(t *testing.T) {
	msgs := []Message{
		UserMessage("test"),
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "call_1", ToolName: "search", Input: "foo"},
				ToolCallPart{ToolCallID: "call_2", ToolName: "read", Input: "bar"},
			},
		},
		ToolMessage(ToolResultPart{ToolCallID: "call_1", ToolName: "search", Result: "found"}),
	}

	out := PatchToolCalls(msgs)
	// Should be: user + assistant + tool(call_1 result) + tool(call_2 placeholder)
	if len(out) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out))
	}
	if out[3].Role != MessageRoleTool {
		t.Fatalf("expected tool message at index 3, got %s", out[3].Role)
	}
	tr := out[3].Content[0].(ToolResultPart)
	if tr.ToolCallID != "call_2" {
		t.Errorf("expected call_2 placeholder, got %s", tr.ToolCallID)
	}
}

func TestPatchToolCalls_CustomPlaceholder(t *testing.T) {
	msgs := []Message{
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "x", ToolName: "t", Input: nil},
			},
		},
	}

	custom := "[skipped]"
	out := PatchToolCallsWith(msgs, custom)
	tr := out[1].Content[0].(ToolResultPart)
	if tr.Result != custom {
		t.Errorf("expected custom placeholder %q, got %v", custom, tr.Result)
	}
}

func TestPatchToolCalls_MultipleAssistantMessages(t *testing.T) {
	msgs := []Message{
		// First assistant: all answered
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "a", ToolName: "t", Input: nil},
			},
		},
		ToolMessage(ToolResultPart{ToolCallID: "a", ToolName: "t", Result: "ok"}),
		// Second assistant: dangling
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "b", ToolName: "t", Input: nil},
			},
		},
	}

	out := PatchToolCalls(msgs)
	// Should be: assistant(a) + tool(a) + assistant(b) + tool(b placeholder)
	if len(out) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out))
	}
	lastTool := out[3].Content[0].(ToolResultPart)
	if lastTool.ToolCallID != "b" {
		t.Errorf("expected placeholder for 'b', got %s", lastTool.ToolCallID)
	}
}

func TestPatchToolCalls_EmptyMessages(t *testing.T) {
	out := PatchToolCalls(nil)
	if out != nil {
		t.Errorf("expected nil for empty input")
	}
}

func TestPatchToolCalls_Idempotent(t *testing.T) {
	msgs := []Message{
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "x", ToolName: "t", Input: nil},
			},
		},
	}

	// First patch inserts placeholder.
	patched := PatchToolCalls(msgs)
	// Second patch should be a no-op (all calls now have responses).
	double := PatchToolCalls(patched)
	if len(double) != len(patched) {
		t.Errorf("expected idempotent result (length %d), got %d", len(patched), len(double))
	}
}
