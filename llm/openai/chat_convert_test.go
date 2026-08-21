package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// reprContent 返回 ChatMessage 序列化后 content 字段的字面值（用于断言空字符串/占位符）。
func reprContent(t *testing.T, m ChatMessage) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal ChatMessage: %v", err)
	}
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	return string(probe.Content)
}

// TestConvertUnified_AssistantReasoningOnly_NotEmptyContent 回归：
// 当 GLM 一轮只返回 reasoning_content（思考）而无 text/toolcall 时，
// orchestrate 把该 assistant 消息存为仅含 ReasoningPart 的 history 条目；
// 回传时 convertUnifiedToChatMessages 之前忽略 ReasoningPart 并生成 content:"" → GLM 报 1214。
// 修复后 content 必须非空（兜底占位符），不再触发 1214。
func TestConvertUnified_AssistantReasoningOnly_NotEmptyContent(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("审查这个文件"),
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
			llm.ReasoningPart{Text: "我先检查函数签名是否合理"}, // 只有思考，无文字结论
		}},
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d", len(out))
	}
	asst := out[1]
	if asst.Role != RoleAssistant {
		t.Fatalf("msg[1] role = %q, want assistant", asst.Role)
	}
	c := reprContent(t, asst)
	if c == `""` {
		t.Errorf("regression: assistant message content is empty string %q (GLM rejects empty content with 1214)", c)
	}
}

// TestConvertUnified_ToolResultEmptyString_NotEmptyContent 回归：
// 工具返回空字符串结果时，旧实现因二次编码意外非空（功能错误）；
// 修复后直接以 resultStr 作 RawMessage，空结果应被兜底为非空占位符，不再 1214。
func TestConvertUnified_ToolResultEmptyString_NotEmptyContent(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("运行一下脚本"),
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
			llm.ToolCallPart{ToolCallID: "call_1", ToolName: "run_code", Input: map[string]any{"cmd": "echo"}},
		}},
		llm.ToolMessage(llm.ToolResultPart{ToolCallID: "call_1", ToolName: "run_code", Result: ""}),
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var toolMsg ChatMessage
	for _, m := range out {
		if m.Role == RoleTool {
			toolMsg = m
		}
	}
	if toolMsg.Role != RoleTool {
		t.Fatalf("tool message not found")
	}
	c := reprContent(t, toolMsg)
	if c == `""` {
		t.Errorf("regression: tool message content is empty string %q (GLM rejects empty content with 1214)", c)
	}
}

// TestConvertUnified_ToolResultNotDoubleEncoded 回归：工具返回字符串结果不应被二次编码。
// 旧实现 quoteJSONString(string(json.Marshal(x))) 把 "hello" 编码成 "\"hello\""（两层引号）。
func TestConvertUnified_ToolResultNotDoubleEncoded(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("读取文件"),
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
			llm.ToolCallPart{ToolCallID: "call_2", ToolName: "read_file", Input: map[string]any{"path": "x"}},
		}},
		llm.ToolMessage(llm.ToolResultPart{ToolCallID: "call_2", ToolName: "read_file", Result: "hello world"}),
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var toolMsg ChatMessage
	for _, m := range out {
		if m.Role == RoleTool {
			toolMsg = m
		}
	}
	c := reprContent(t, toolMsg)
	want := `"hello world"`
	if c != want {
		t.Errorf("tool result double-encoded: got %q, want %q", c, want)
	}
}

// TestConvertUnified_AssistantToolCallsContentNull 回归：
// 带 tool_calls 且文本为空的 assistant 消息，content 必须为 JSON null（GLM 要求）。
func TestConvertUnified_AssistantToolCallsContentNull(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("运行"),
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
			llm.ToolCallPart{ToolCallID: "call_3", ToolName: "run", Input: map[string]any{}},
		}},
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	asst := out[1]
	c := reprContent(t, asst)
	if c != "null" {
		t.Errorf("assistant with tool_calls and empty text: content should be null, got %q", c)
	}
}

// TestConvertUnified_LeadingAssistantGetsUserPrefix 回归：
// GLM 要求首条消息 role 为 user/system。若历史首条是 assistant，应前置占位 user 消息。
func TestConvertUnified_LeadingAssistantGetsUserPrefix(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{llm.TextPart{Text: "之前的中间结论"}}},
		llm.UserMessage("继续"),
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out) < 1 || out[0].Role != RoleUser {
		t.Errorf("first message should be a leading user placeholder, got role %q", out[0].Role)
	}
	if !strings.Contains(reprContent(t, out[0]), "对话历史续接") {
		t.Errorf("leading placeholder should explain history continuation, got %q", reprContent(t, out[0]))
	}
}
