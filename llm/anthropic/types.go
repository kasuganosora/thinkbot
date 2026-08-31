package anthropic

import "encoding/json"

// ============================================================================
// 常量
// ============================================================================

const (
	// APIVersion Anthropic API 版本。
	APIVersion = "2023-06-01"

	// DefaultBaseURL 默认 API 基础地址。
	DefaultBaseURL = "https://api.anthropic.com"

	// RoleUser 用户角色。
	RoleUser = "user"
	// RoleAssistant 助手角色。
	RoleAssistant = "assistant"
)

// StopReason 停止原因。
type StopReason string

const (
	StopReasonEndTurn      StopReason = "end_turn"
	StopReasonMaxTokens    StopReason = "max_tokens"
	StopReasonStopSequence StopReason = "stop_sequence"
	StopReasonToolUse      StopReason = "tool_use"
)

// ============================================================================
// Message & Content
// ============================================================================

// Message 表示对话中的一条消息。
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// MessageContent 可以是简单字符串或 ContentBlock 数组。
// 使用自定义 MarshalJSON / UnmarshalJSON 实现多态。
type MessageContent []ContentBlock

// MarshalJSON 实现 json.Marshaler。
// 如果只有一个 text block 且无额外字段，输出为字符串。
func (c MessageContent) MarshalJSON() ([]byte, error) {
	if len(c) == 1 && c[0].Type == ContentTypeText && c[0].CacheControl == nil {
		return json.Marshal(c[0].Text)
	}
	return json.Marshal([]ContentBlock(c))
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (c *MessageContent) UnmarshalJSON(data []byte) error {
	// 尝试解析为字符串
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = MessageContent{{Type: ContentTypeText, Text: s}}
		return nil
	}
	// 解析为数组
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	*c = MessageContent(blocks)
	return nil
}

// TextContent 快捷创建文本内容。
func TextContent(text string) MessageContent {
	return MessageContent{{Type: ContentTypeText, Text: text}}
}

// ContentType 内容块类型。
type ContentType string

const (
	ContentTypeText       ContentType = "text"
	ContentTypeImage      ContentType = "image"
	ContentTypeToolUse    ContentType = "tool_use"
	ContentTypeToolResult ContentType = "tool_result"
	ContentTypeDocument   ContentType = "document"
	ContentTypeThinking   ContentType = "thinking"
)

// ContentBlock 表示消息中的一个内容块。
//
// 不同 Type 使用不同字段：
//   - text:        Text
//   - image:       Source
//   - document:    Source (PDF 等)
//   - tool_use:    ID, Name, Input
//   - tool_result: ToolUseID, Content (string 或 []ContentBlock)
//   - thinking:    Thinking, Signature
type ContentBlock struct {
	Type ContentType `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image / document
	Source *ImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID     string          `json:"tool_use_id,omitempty"`
	ResultContent json.RawMessage `json:"content,omitempty"` // string 或 []ContentBlock
	IsError       bool            `json:"is_error,omitempty"`

	// thinking（扩展思考）
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// prompt caching
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	// citations
	Citations []Citation `json:"citations,omitempty"`
}

// ImageSource 图片来源。
//
// 支持三种来源类型：
//   - base64: MediaType + Data
//   - url:    URL
//   - file:   FileID（需配合 Files API beta header 使用）
type ImageSource struct {
	Type      string `json:"type"`                 // "base64", "url" 或 "file"
	MediaType string `json:"media_type,omitempty"` // base64 时必须
	Data      string `json:"data,omitempty"`       // base64 时必须
	URL       string `json:"url,omitempty"`        // url 时必须
	FileID    string `json:"file_id,omitempty"`    // file 时必须
}

// 图片 MIME 类型常量。
const (
	ImageJPEG = "image/jpeg"
	ImagePNG  = "image/png"
	ImageGIF  = "image/gif"
	ImageWebP = "image/webp"
)

// Base64ImageSource 创建 base64 编码的图片来源。
func Base64ImageSource(mediaType, data string) *ImageSource {
	return &ImageSource{Type: "base64", MediaType: mediaType, Data: data}
}

// URLImageSource 创建 URL 图片来源。
func URLImageSource(url string) *ImageSource {
	return &ImageSource{Type: "url", URL: url}
}

// FileImageSource 创建 Files API file_id 图片来源。
func FileImageSource(fileID string) *ImageSource {
	return &ImageSource{Type: "file", FileID: fileID}
}

// CacheControl 提示缓存控制。
//
// 用于自动缓存（请求级）或显式断点（内容块级）。
// TTL 默认 5 分钟，设为 "1h" 可使用 1 小时缓存。
type CacheControl struct {
	Type string `json:"type"`          // "ephemeral"
	TTL  string `json:"ttl,omitempty"` // "5m"（默认）或 "1h"
}

// 常用 TTL 值。
const (
	CacheTTL5m = "5m"
	CacheTTL1h = "1h"
)

// EphemeralCacheControl 创建 5 分钟 TTL 的缓存控制。
func EphemeralCacheControl() *CacheControl {
	return &CacheControl{Type: "ephemeral"}
}

// EphemeralCacheControl1h 创建 1 小时 TTL 的缓存控制。
func EphemeralCacheControl1h() *CacheControl {
	return &CacheControl{Type: "ephemeral", TTL: CacheTTL1h}
}

// Citation 引用。
type Citation struct {
	Type          string `json:"type"`
	CitedText     string `json:"cited_text"`
	DocumentIndex int    `json:"document_index"`
	DocumentTitle string `json:"document_title,omitempty"`
	Start         int    `json:"start,omitempty"`
	End           int    `json:"end,omitempty"`
}

// ============================================================================
// Tool
// ============================================================================

// 工具选择类型常量。
const (
	ToolChoiceAuto = "auto" // 让 Claude 自行决定是否调用工具（默认）
	ToolChoiceAny  = "any"  // 必须使用某个工具
	ToolChoiceTool = "tool" // 必须使用指定的工具（需设置 Name）
	ToolChoiceNone = "none" // 禁止使用工具
)

// Tool 定义一个可调用的工具。
type Tool struct {
	Name          string           `json:"name"`
	Description   string           `json:"description,omitempty"`
	InputSchema   any              `json:"input_schema"`             // JSON Schema 对象
	Strict        *bool            `json:"strict,omitempty"`         // 严格模式：保证工具调用输入严格匹配 schema
	InputExamples []map[string]any `json:"input_examples,omitempty"` // 输入示例（帮助 Claude 理解复杂参数）
	CacheControl  *CacheControl    `json:"cache_control,omitempty"`
}

// ToolChoice 工具选择策略。
type ToolChoice struct {
	Type            string `json:"type"`           // ToolChoiceAuto | ToolChoiceAny | ToolChoiceTool | ToolChoiceNone
	Name            string `json:"name,omitempty"` // Type=tool 时必须
	DisableParallel bool   `json:"disable_parallel_tool_use,omitempty"`
}

// ============================================================================
// Request & Response
// ============================================================================

// MessageRequest Messages API 请求体。
type MessageRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"` // string 或 []TextBlock
	MaxTokens     int             `json:"max_tokens"`
	CacheControl  *CacheControl   `json:"cache_control,omitempty"` // 自动缓存（请求级）
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`      // 扩展思考配置
	Metadata      *Metadata       `json:"metadata,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
}

// ThinkingConfig 扩展思考配置。
type ThinkingConfig struct {
	Type         string `json:"type"`          // "enabled"
	BudgetTokens int    `json:"budget_tokens"` // 思考 token 预算
}

// ThinkingEnabled 创建一个启用扩展思考的配置。
func ThinkingEnabled(budgetTokens int) *ThinkingConfig {
	return &ThinkingConfig{Type: "enabled", BudgetTokens: budgetTokens}
}

// Metadata 请求元数据。
type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

// SystemBlock 表示 system 消息数组中的一个文本块。
// 用于显式 system 缓存断点。
type SystemBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// SystemText 创建一个无缓存的 SystemBlock 数组（用于 System 字段）。
func SystemText(text string) json.RawMessage {
	data, _ := json.Marshal([]SystemBlock{{Type: "text", Text: text}})
	return data
}

// SystemTextWithCache 创建一个带缓存断点的 SystemBlock 数组。
func SystemTextWithCache(text string, cc *CacheControl) json.RawMessage {
	data, _ := json.Marshal([]SystemBlock{{Type: "text", Text: text, CacheControl: cc}})
	return data
}

// SystemBlocks 将多个 SystemBlock 转为 json.RawMessage（用于 System 字段）。
func SystemBlocks(blocks ...SystemBlock) json.RawMessage {
	data, _ := json.Marshal(blocks)
	return data
}

// MessageResponse Messages API 响应体。
type MessageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   StopReason     `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// Usage token 用量统计。
type Usage struct {
	InputTokens         int                 `json:"input_tokens"`
	OutputTokens        int                 `json:"output_tokens"`
	CacheCreationTokens int                 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int                 `json:"cache_read_input_tokens,omitempty"`
	CacheCreation       *CacheCreationUsage `json:"cache_creation,omitempty"` // TTL 拆分明细
}

// CacheCreationUsage 按缓存 TTL 拆分的写入 token 数。
type CacheCreationUsage struct {
	Ephemeral5mTokens int `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hTokens int `json:"ephemeral_1h_input_tokens,omitempty"`
}

// ============================================================================
// Count Tokens
// ============================================================================

// CountTokensRequest count_tokens 请求体。
type CountTokensRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	System   json.RawMessage `json:"system,omitempty"`
	Tools    []Tool          `json:"tools,omitempty"`
	Thinking *ThinkingConfig `json:"thinking,omitempty"` // 扩展思考配置
}

// CountTokensResponse count_tokens 响应体。
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// ============================================================================
// Models
// ============================================================================

// Model 表示一个可用模型。
type Model struct {
	Type        string `json:"type"` // "model"
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// ListModelsResponse list models 响应体。
type ListModelsResponse struct {
	Data    []Model `json:"data"`
	FirstID string  `json:"first_id"`
	LastID  string  `json:"last_id"`
	HasMore bool    `json:"has_more"`
}

// ============================================================================
// Streaming
// ============================================================================

// StreamEventType 流式事件类型。
type StreamEventType string

const (
	EventMessageStart      StreamEventType = "message_start"
	EventContentBlockStart StreamEventType = "content_block_start"
	EventContentBlockDelta StreamEventType = "content_block_delta"
	EventContentBlockStop  StreamEventType = "content_block_stop"
	EventMessageDelta      StreamEventType = "message_delta"
	EventMessageStop       StreamEventType = "message_stop"
	EventPing              StreamEventType = "ping"
	EventError             StreamEventType = "error"
)

// StreamEvent 表示一个流式事件。
//
// 根据 Type 不同，有效字段不同：
//   - message_start:       Message
//   - content_block_start: Index, ContentBlock
//   - content_block_delta: Index, Delta
//   - content_block_stop:  Index
//   - message_delta:       Delta, Usage
//   - message_stop:        (无额外字段)
//   - ping:                (无额外字段)
//   - error:               Error
type StreamEvent struct {
	Type         StreamEventType  `json:"type"`
	Message      *MessageResponse `json:"message,omitempty"`
	Index        *int             `json:"index,omitempty"`
	ContentBlock *ContentBlock    `json:"content_block,omitempty"`
	Delta        *Delta           `json:"delta,omitempty"`
	Usage        *Usage           `json:"usage,omitempty"`
	Error        *APIError        `json:"error,omitempty"`
}

// Delta 表示增量内容。
//
// Type 不同时字段不同：
//   - text_delta:      Text
//   - input_json_delta: PartialJSON
//   - thinking_delta:  Thinking
//   - signature_delta: Signature
//   - message_delta:   StopReason, StopSequence
type Delta struct {
	Type         string     `json:"type"`
	Text         string     `json:"text,omitempty"`
	PartialJSON  string     `json:"partial_json,omitempty"`
	StopReason   StopReason `json:"stop_reason,omitempty"`
	StopSequence string     `json:"stop_sequence,omitempty"`
	Thinking     string     `json:"thinking,omitempty"`
	Signature    string     `json:"signature,omitempty"`
}

// ============================================================================
// Error
// ============================================================================

// APIError Anthropic API 错误。
type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return e.Type + ": " + e.Message
}

// ErrorResponse API 错误响应体。
type ErrorResponse struct {
	Type  string   `json:"type"` // "error"
	Error APIError `json:"error"`
}

// ============================================================================
// Files API (Beta)
// ============================================================================

// BetaFilesAPI Files API 所需的 beta header 值。
const BetaFilesAPI = "files-api-2025-04-14"

// FileScope 文件作用域（例如 session）。
type FileScope struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "session"
}

// FileMetadata 文件元数据。
type FileMetadata struct {
	ID           string     `json:"id"`
	CreatedAt    string     `json:"created_at"`
	Filename     string     `json:"filename"`
	MimeType     string     `json:"mime_type"`
	SizeBytes    int64      `json:"size_bytes"`
	Type         string     `json:"type"` // "file"
	Downloadable *bool      `json:"downloadable,omitempty"`
	Scope        *FileScope `json:"scope,omitempty"`
}

// ListFilesResponse 列表文件响应体。
type ListFilesResponse struct {
	Data    []FileMetadata `json:"data"`
	FirstID string         `json:"first_id,omitempty"`
	HasMore bool           `json:"has_more,omitempty"`
	LastID  string         `json:"last_id,omitempty"`
}

// DeletedFile 删除文件响应体。
type DeletedFile struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "file_deleted"
}
