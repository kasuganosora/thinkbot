package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestToolCallsJSONFromResult(t *testing.T) {
	if toolCallsJSONFromResult(nil) != "" || toolCallsJSONFromResult(&llm.GenerateResult{Text: "hi"}) != "" {
		t.Fatal("no tool calls → empty string")
	}
	focus := strings.Repeat("keep branch fix/x and path llm/intent_judge.go; ", 20)
	res := &llm.GenerateResult{
		Steps: []llm.StepResult{{Messages: []llm.Message{
			{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
				llm.TextPart{Text: "trying"},
				llm.ToolCallPart{ToolCallID: "c1", ToolName: "compact_context", Input: map[string]any{"keep_recent": 5, "focus": focus}},
				llm.ToolCallPart{ToolCallID: "c2", ToolName: "exec", Input: map[string]any{"cmd": "ls"}},
			}},
			llm.ToolMessage(llm.ToolResultPart{ToolCallID: "c1", ToolName: "compact_context", InvocationID: "inv-1", Result: map[string]any{"status": "compacted"}}),
			llm.ToolMessage(llm.ToolResultPart{ToolCallID: "c2", ToolName: "exec", Result: strings.Repeat("x", 5000), IsError: true}),
		}}},
	}
	raw := toolCallsJSONFromResult(res)
	var calls []map[string]any
	if err := json.Unmarshal([]byte(raw), &calls); err != nil || len(calls) != 2 {
		t.Fatalf("decode: %v %q", err, raw)
	}
	c1 := calls[0]
	if c1["name"] != "compact_context" || c1["status"] != "success" || c1["invocationId"] != "inv-1" ||
		c1["input"].(map[string]any)["focus"] != focus || !strings.Contains(c1["output"].(string), "compacted") {
		t.Fatalf("compact_context entry wrong: %+v", c1)
	}
	c2 := calls[1]
	if c2["status"] != "error" || c2["outputTruncated"] != true || len([]rune(c2["output"].(string))) != maxPersistedToolOutputRunes {
		t.Fatalf("exec entry wrong: status=%v truncated=%v", c2["status"], c2["outputTruncated"])
	}
}
