package openai

import "encoding/json"

// ============================================================================
// Chat Completions API — 类型定义
//
// Chat Completions 是 OpenAI 经典的对话补全 API（/v1/chat/completions），
// 与 Responses API（/v1/responses）并列。许多 OpenAI 兼容供应商（如智谱
// BigModel、DeepSeek、Moonshot 等）仅实现了 Chat Completions。
// ============================================================================

// 常量
const (
	// RoleTool 工具角色。
	RoleTool = "tool"
)

// ChatMessage 表示 Chat Completions 对话中的一条消息。
//
// Content 可以是 string 或 []ChatContentPart。对于多模态消息使用 ContentPart。
type ChatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`                     // string 或 []ChatContentPart
	ReasoningContent string          `json:"reasoning_content,omitempty"` // 推理内容（部分模型返回）
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ChatToolCall  `json:"tool_calls,omitempty"`
}

// ChatContentPart 多模态消息内容部分。
type ChatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
}

// ChatImageURL 图片 URL。
type ChatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ChatToolCall 工具调用。
type ChatToolCall struct {
	Index    int              `json:"index,omitempty"` // 流式 delta 中的索引
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ChatFunctionCall `json:"function"`
}

// ChatFunctionCall 函数调用信息。
type ChatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ChatTool 工具定义。
type ChatTool struct {
	Type     string           `json:"type"` // "function"
	Function ChatToolFunction `json:"function"`
}

// ChatToolFunction 工具函数定义。
type ChatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatResponseFormat 响应格式控制。
type ChatResponseFormat struct {
	Type       string                `json:"type"` // "text" | "json_object" | "json_schema"
	JSONSchema *ChatJSONSchemaConfig `json:"json_schema,omitempty"`
}

// ChatJSONSchemaConfig JSON Schema 配置。
type ChatJSONSchemaConfig struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict *bool           `json:"strict,omitempty"`
}

// ChatStreamOptions 流式选项。
type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatCompletionRequest Chat Completions 请求体。
type ChatCompletionRequest struct {
	Model            string              `json:"model"`
	Messages         []ChatMessage       `json:"messages"`
	Temperature      *float64            `json:"temperature,omitempty"`
	MaxTokens        *int                `json:"max_tokens,omitempty"`
	TopP             *float64            `json:"top_p,omitempty"`
	FrequencyPenalty *float64            `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64            `json:"presence_penalty,omitempty"`
	Stream           bool                `json:"stream,omitempty"`
	StreamOptions    *ChatStreamOptions  `json:"stream_options,omitempty"`
	Stop             json.RawMessage     `json:"stop,omitempty"` // string 或 []string
	N                *int                `json:"n,omitempty"`
	ResponseFormat   *ChatResponseFormat `json:"response_format,omitempty"`
	Tools            []ChatTool          `json:"tools,omitempty"`
	ToolChoice       json.RawMessage     `json:"tool_choice,omitempty"` // string 或 object
	Seed             *int                `json:"seed,omitempty"`
	User             string              `json:"user,omitempty"`
}

// ChatCompletionResponse Chat Completions 响应体。
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"` // "chat.completion"
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatChoice 表示一个响应选项。
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	Delta        ChatDelta   `json:"delta,omitempty"` // 流式增量
	FinishReason string      `json:"finish_reason,omitempty"`
}

// ChatDelta 流式增量内容。
type ChatDelta struct {
	Role             string         `json:"role,omitempty"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}

// ChatUsage token 用量统计。
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ContentStr 返回 ChatMessage.Content 的字符串形式。
func (m ChatMessage) ContentStr() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return string(m.Content)
}

// 消息构造辅助函数

// ChatSystemMessage 创建系统消息。
func ChatSystemMessage(content string) ChatMessage {
	return ChatMessage{Role: RoleSystem, Content: json.RawMessage(quoteJSONString(content))}
}

// ChatUserMessage 创建用户消息。
func ChatUserMessage(content string) ChatMessage {
	return ChatMessage{Role: RoleUser, Content: json.RawMessage(quoteJSONString(content))}
}

// ChatAssistantMessage 创建助手消息。
func ChatAssistantMessage(content string) ChatMessage {
	return ChatMessage{Role: RoleAssistant, Content: json.RawMessage(quoteJSONString(content))}
}

// ChatToolMessage 创建工具结果消息。
func ChatToolMessage(toolCallID, content string) ChatMessage {
	return ChatMessage{Role: RoleTool, ToolCallID: toolCallID, Content: json.RawMessage(quoteJSONString(content))}
}
