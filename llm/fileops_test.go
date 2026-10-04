package llm

import (
	"strings"
	"testing"
)

func TestAttachSandboxPathsPinsWrittenFile(t *testing.T) {
	head := []Message{
		{Role: MessageRoleAssistant, Content: []MessagePart{
			ToolCallPart{ToolCallID: "c1", ToolName: "sandbox_write_file", Input: map[string]any{"path": "/data/deck.html"}},
		}},
		{Role: MessageRoleTool, Content: []MessagePart{
			ToolResultPart{ToolCallID: "c1", ToolName: "sandbox_write_file", Result: "wrote"},
		}},
	}
	out := AttachSandboxPaths("summary without paths", head)
	if !strings.Contains(out, "<modified-files>") || !strings.Contains(out, "/data/deck.html") {
		t.Fatalf("path not pinned: %s", out)
	}
	again := AttachSandboxPaths(out, nil)
	if strings.Count(again, "/data/deck.html") != 1 {
		t.Fatalf("path duplicated: %s", again)
	}
}

func TestNearestSafeBoundaryKeepsToolPair(t *testing.T) {
	msgs := []Message{
		UserMessage("start"),
		{Role: MessageRoleAssistant, Content: []MessagePart{
			ToolCallPart{ToolCallID: "c1", ToolName: "sandbox_read_file", Input: map[string]any{"path": "a.go"}},
		}},
		{Role: MessageRoleTool, Content: []MessagePart{ToolResultPart{ToolCallID: "c1", Result: "body"}}},
		UserMessage("next"),
	}
	if got := nearestSafeBoundary(msgs, 2); got != 1 {
		t.Fatalf("boundary = %d, want the assistant tool call", got)
	}
	c := NewCompactor(CompactionConfig{TailTurns: 1})
	if idx := c.selectTailSplit(msgs); !IsSafeCompactionBoundary(msgs, idx) {
		t.Fatalf("tail split %d splits a tool pair", idx)
	}
}

func TestTrackedPathsCap(t *testing.T) {
	var ops fileOps
	for i := 0; i < 60; i++ {
		ops.note("sandbox_write_file", "f"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	if ops.n > maxTrackedFilePaths {
		t.Fatalf("tracked %d", ops.n)
	}
}
