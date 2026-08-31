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

// TestConvertUnified_NoParamToolGetsProperties 回归：
// 当工具被延迟加载剥离掉 Parameters（如 text_hash/uuid/random 等 DeferredLoad 工具
// 未加载时 Parameters 被置 nil）时，旧实现发出 {"type":"object"}（缺 properties 键），
// GLM 以 1210 "API 调用参数有误" 整请求拒收。修复后必须补齐 "properties":{}，
// 使其满足 GLM 的函数 schema 结构要求。
func TestConvertUnified_NoParamToolGetsProperties(t *testing.T) {
	tools := []llm.Tool{
		{Name: "uuid", Description: "generate uuid"}, // nil Parameters（延迟加载剥离后）
		{Name: "has_params", Description: "d", Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"q": map[string]any{"type": "string", "description": "query"}},
			"required":   []string{"q"},
		}},
		{Name: "nested_obj", Description: "d", Parameters: map[string]any{
			"type": "object", // 故意省略 properties，模拟一个裸 object 类型
		}},
	}
	out := convertUnifiedToChatTools(tools)
	if len(out) != 3 {
		t.Fatalf("want 3 tools, got %d", len(out))
	}
	for _, c := range out {
		var s struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(c.Function.Parameters, &s); err != nil {
			t.Fatalf("tool %s: unmarshal params: %v", c.Function.Name, err)
		}
		if s.Type != "object" {
			t.Errorf("tool %s: top type = %q, want object", c.Function.Name, s.Type)
		}
		if s.Properties == nil {
			t.Errorf("tool %s: missing properties key (would trigger GLM 1210)", c.Function.Name)
		}
	}
}

// TestNormalizeToolParameters_Recursive 回归：嵌套 object 缺 properties 也应被补齐。
func TestNormalizeToolParameters_Recursive(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"meta":{"type":"object"},"tags":{"type":"array","items":{"type":"object"}}}}`)
	out := normalizeToolParameters(raw)
	var s map[string]any
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props, _ := s["properties"].(map[string]any)
	meta, _ := props["meta"].(map[string]any)
	if _, ok := meta["properties"]; !ok {
		t.Errorf("nested object 'meta' should have properties key added")
	}
	tags, _ := props["tags"].(map[string]any)
	items, _ := tags["items"].(map[string]any)
	if _, ok := items["properties"]; !ok {
		t.Errorf("array item object should have properties key added")
	}
}

// TestConvertUnified_CoalesceConsecutiveUser 回归：
// GLM 要求消息严格交替 role，连续 user/user 会被 1210 "API 调用参数有误" 整请求拒收。
// 真实事故：工作流完成「系统通知」被多次注入为 user role，连成 5 条连续 user。
// 修复后连续同 role 文本消息应被合并为一条。
func TestConvertUnified_CoalesceConsecutiveUser(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("你 review 下代码"),
		llm.UserMessage("系统通知：工作流已完成"),
		llm.UserMessage("系统通知：工作流已完成"),
		llm.UserMessage("重新 review"),
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// 4 条连续 user 应被合并为 1 条
	userCount := 0
	for _, m := range out {
		if m.Role == RoleUser {
			userCount++
		}
	}
	if userCount != 1 {
		t.Fatalf("want 1 user message after coalesce, got %d (roles: %v)", userCount, rolesOf(out))
	}
	if !strings.Contains(reprContent(t, out[0]), "你 review 下代码") ||
		!strings.Contains(reprContent(t, out[0]), "系统通知：工作流已完成") ||
		!strings.Contains(reprContent(t, out[0]), "重新 review") {
		t.Errorf("merged user content missing some original parts: %s", reprContent(t, out[0]))
	}
}

// TestConvertUnified_ToolMessagesNotMerged 回归：tool 消息即使连续也必须保留独立
// （每条带不同 tool_call_id），合并会破坏与 assistant tool_call 的配对。
func TestConvertUnified_ToolMessagesNotMerged(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
			llm.ToolCallPart{ToolCallID: "c1", ToolName: "read_file", Input: map[string]any{"path": "a"}},
		}},
		{Role: llm.MessageRoleTool, Content: []llm.MessagePart{llm.ToolResultPart{ToolCallID: "c1", Result: "x"}}},
		{Role: llm.MessageRoleTool, Content: []llm.MessagePart{llm.ToolResultPart{ToolCallID: "c2", Result: "y"}}},
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	toolCount := 0
	for _, m := range out {
		if m.Role == RoleTool {
			toolCount++
		}
	}
	if toolCount != 2 {
		t.Fatalf("tool messages must stay separate, got %d tool messages (want 2)", toolCount)
	}
}

func rolesOf(msgs []ChatMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role))
	}
	return out
}

// TestConvertUnified_ToolResultIsString 回归：
// tool role 消息的 content 必须是 JSON 字符串（OpenAI/GLM 严格要求），不能是 JSON 对象。
// 旧实现以 RawMessage（对象 {"count":18}）直接嵌入，GLM 整请求拒收 1210。
// 修复后 content 应为被转义的字符串："{\"count\":18,...}"。
func TestConvertUnified_ToolResultIsString(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
			llm.ToolCallPart{ToolCallID: "c1", ToolName: "list_dir", Input: map[string]any{"path": "."}},
		}},
		{Role: llm.MessageRoleTool, Content: []llm.MessagePart{
			llm.ToolResultPart{ToolCallID: "c1", Result: map[string]any{"count": 18, "ok": true}},
		}},
	}
	out, err := convertUnifiedToChatMessages("", msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var toolMsg *ChatMessage
	for i := range out {
		if out[i].Role == RoleTool {
			toolMsg = &out[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool message produced")
	}
	// content 解码后应是一个 Go string（而非 object）
	var asStr string
	if err := json.Unmarshal(toolMsg.Content, &asStr); err != nil {
		t.Fatalf("tool content should be a JSON string, got: %s (err: %v)", string(toolMsg.Content), err)
	}
	// 内容本身应是合法 JSON（工具结果），即再次解析应为 object
	var inner map[string]any
	if err := json.Unmarshal([]byte(asStr), &inner); err != nil {
		t.Fatalf("tool content string should contain valid JSON result, got: %q", asStr)
	}
	if inner["count"] != float64(18) {
		t.Errorf("tool result field lost: %v", inner)
	}
}

// TestEnsureUserMessage_SystemOnly 回归：GLM（BigModel）在整段对话缺失 user 角色时
// 拒收 1214 "messages 参数非法"（OpenAI 允许 system-only，但 GLM 更严格）。delegate
// 节点重试/首轮仅注入 system prompt 时会出现 system-only 请求。修复后应在 system 之后
// 补一条非空占位 user 消息。已用真实请求 replay 验证：补 user 后 GLM 由 1214 变为 200。
func TestEnsureUserMessage_SystemOnly(t *testing.T) {
	in := []ChatMessage{
		{Role: RoleSystem, Content: json.RawMessage(`"你是 Go 专家"`)},
	}
	out := ensureUserMessage(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages after inject, got %d", len(out))
	}
	if out[0].Role != RoleSystem {
		t.Errorf("first message should stay system, got %s", out[0].Role)
	}
	if out[1].Role != RoleUser {
		t.Fatalf("second message should be injected user, got %s", out[1].Role)
	}
	if strings.Contains(reprContent(t, out[1]), `""`) {
		t.Errorf("injected user content must be non-empty, got %s", reprContent(t, out[1]))
	}
}

// TestEnsureUserMessage_NoSystem 当序列无 system 时，占位 user 应插到最前。
func TestEnsureUserMessage_NoSystem(t *testing.T) {
	in := []ChatMessage{{Role: RoleAssistant, Content: json.RawMessage(`"hi"`)}}
	out := ensureUserMessage(in)
	if len(out) != 2 || out[0].Role != RoleUser {
		t.Fatalf("expected injected user at front, got %v", out)
	}
}

// TestEnsureUserMessage_AlreadyHasUser 已有 user 时不改动。
func TestEnsureUserMessage_AlreadyHasUser(t *testing.T) {
	in := []ChatMessage{
		{Role: RoleSystem, Content: json.RawMessage(`"sys"`)},
		{Role: RoleUser, Content: json.RawMessage(`"task"`)},
	}
	out := ensureUserMessage(in)
	if len(out) != 2 || out[1].Role != RoleUser {
		t.Fatalf("should be unchanged, got %v", out)
	}
}
