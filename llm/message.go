package llm

// MessageRole identifies the speaker of a message.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleSystem    MessageRole = "system"
	MessageRoleTool      MessageRole = "tool"
)

// MessagePartType discriminates content blocks within a message.
type MessagePartType string

const (
	PartTypeText       MessagePartType = "text"
	PartTypeReasoning  MessagePartType = "reasoning"
	PartTypeImage      MessagePartType = "image"
	PartTypeFile       MessagePartType = "file"
	PartTypeToolCall   MessagePartType = "tool-call"
	PartTypeToolResult MessagePartType = "tool-result"
)

// MessagePart is implemented by every content block type.
type MessagePart interface {
	PartType() MessagePartType
}

// --- Text ---

// TextPart carries a text segment and optional prompt-caching hints.
type TextPart struct {
	Text             string         `json:"text"`
	CacheControl     *CacheControl  `json:"cacheControl,omitempty"`
	ProviderMetadata map[string]any `json:"providerMetadata,omitempty"`
}

func (p TextPart) PartType() MessagePartType { return PartTypeText }

// --- Reasoning ---

// ReasoningPart carries a reasoning/thinking segment from a model.
type ReasoningPart struct {
	Text string `json:"text"`
	// Signature holds the encrypted signature returned by Anthropic during
	// extended thinking. It MUST be sent back in subsequent requests that
	// include this thinking block, otherwise the API rejects the request.
	Signature        string         `json:"signature,omitempty"`
	ProviderMetadata map[string]any `json:"providerMetadata,omitempty"`
}

func (p ReasoningPart) PartType() MessagePartType { return PartTypeReasoning }

// --- Image ---

// ImagePart carries an image via URL or base64 data URI.
type ImagePart struct {
	Image        string        `json:"image"`
	MediaType    string        `json:"mediaType,omitempty"`
	CacheControl *CacheControl `json:"cacheControl,omitempty"`
}

func (p ImagePart) PartType() MessagePartType { return PartTypeImage }

// --- File ---

// FilePart carries arbitrary file data (base64-encoded).
type FilePart struct {
	Data         string        `json:"data"`
	MediaType    string        `json:"mediaType,omitempty"`
	Filename     string        `json:"filename,omitempty"`
	CacheControl *CacheControl `json:"cacheControl,omitempty"`
}

func (p FilePart) PartType() MessagePartType { return PartTypeFile }

// --- Tool Call (assistant) ---

// ToolCallPart represents a tool call in an assistant message.
type ToolCallPart struct {
	ToolCallID       string         `json:"toolCallId"`
	ToolName         string         `json:"toolName"`
	Input            any            `json:"input"`
	ProviderMetadata map[string]any `json:"providerMetadata,omitempty"`
}

func (p ToolCallPart) PartType() MessagePartType { return PartTypeToolCall }

// --- Tool Result (tool-role message) ---

// ToolResultPart carries the result of a tool execution.
type ToolResultPart struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Result     any    `json:"result"`
	IsError    bool   `json:"isError,omitempty"`
}

func (p ToolResultPart) PartType() MessagePartType { return PartTypeToolResult }

// --- CacheControl ---

// CacheControl specifies prompt-caching behaviour.
// Currently only Anthropic supports this; other providers ignore it.
type CacheControl struct {
	Type string `json:"type"`          // "ephemeral"
	TTL  string `json:"ttl,omitempty"` // "" (5 min) | "1h"
}

// EphemeralCacheControl returns a default 5-minute cache control.
func EphemeralCacheControl() *CacheControl {
	return &CacheControl{Type: "ephemeral"}
}

// --- Message ---

// Message is the unified message type used across all providers.
type Message struct {
	Role    MessageRole   `json:"role"`
	Content []MessagePart `json:"content"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// --- Convenience constructors ---

// UserMessage creates a user message with a text part plus optional extra parts.
func UserMessage(text string, extra ...MessagePart) Message {
	parts := make([]MessagePart, 0, 1+len(extra))
	parts = append(parts, TextPart{Text: text})
	parts = append(parts, extra...)
	return Message{Role: MessageRoleUser, Content: parts}
}

// SystemMessage creates a system message.
func SystemMessage(text string) Message {
	return Message{Role: MessageRoleSystem, Content: []MessagePart{TextPart{Text: text}}}
}

// AssistantMessage creates an assistant message.
func AssistantMessage(text string) Message {
	return Message{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: text}}}
}

// ToolMessage creates a tool-role message containing one or more results.
func ToolMessage(results ...ToolResultPart) Message {
	parts := make([]MessagePart, len(results))
	for i, r := range results {
		parts[i] = r
	}
	return Message{Role: MessageRoleTool, Content: parts}
}

// TextFromParts extracts concatenated text from all TextPart blocks.
func TextFromParts(parts []MessagePart) string {
	var text string
	for _, p := range parts {
		if tp, ok := p.(TextPart); ok {
			text += tp.Text
		}
	}
	return text
}
