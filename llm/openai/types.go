package openai

import "encoding/json"

// ============================================================================
// 常量
// ============================================================================

const (
	// DefaultBaseURL 默认 API 基础地址。
	DefaultBaseURL = "https://api.openai.com"

	// RoleSystem 系统角色。
	RoleSystem = "system"
	// RoleUser 用户角色。
	RoleUser = "user"
	// RoleAssistant 助手角色。
	RoleAssistant = "assistant"
	// RoleDeveloper 开发者角色。
	RoleDeveloper = "developer"
)

// 模型名称。
const (
	// 文本/推理模型
	ModelO3        = "o3"
	ModelO4Mini    = "o4-mini"
	ModelGPT5      = "gpt-5"
	ModelGPT5Mini  = "gpt-5-mini"
	ModelGPT41     = "gpt-4.1"
	ModelGPT41Mini = "gpt-4.1-mini"
	ModelGPT41Nano = "gpt-4.1-nano"
	ModelGPT4o     = "gpt-4o"
	ModelGPT4oMini = "gpt-4o-mini"
	ModelGPT4Turbo = "gpt-4-turbo"
	ModelGPT4      = "gpt-4"

	// 音频模型 — TTS
	ModelTTS1         = "tts-1"
	ModelTTS1HD       = "tts-1-hd"
	ModelGPT4oMiniTTS = "gpt-4o-mini-tts"

	// 音频模型 — 转录/翻译
	ModelWhisper1        = "whisper-1"
	ModelGPT4oTranscribe = "gpt-4o-transcribe"
	ModelGPT4oMiniTrans  = "gpt-4o-mini-transcribe"
)

// TTS 语音。
const (
	VoiceAlloy   = "alloy"
	VoiceAsh     = "ash"
	VoiceBallad  = "ballad"
	VoiceCoral   = "coral"
	VoiceEcho    = "echo"
	VoiceFable   = "fable"
	VoiceNova    = "nova"
	VoiceOnyx    = "onyx"
	VoiceSage    = "sage"
	VoiceShimmer = "shimmer"
	VoiceVerse   = "verse"
	VoiceMarin   = "marin"
	VoiceCedar   = "cedar"
)

// TTS 音频格式。
const (
	AudioFormatMP3  = "mp3"
	AudioFormatOpus = "opus"
	AudioFormatAAC  = "aac"
	AudioFormatFLAC = "flac"
	AudioFormatWAV  = "wav"
	AudioFormatPCM  = "pcm"
)

// 推理努力程度。
const (
	ReasoningMinimal = "minimal"
	ReasoningLow     = "low"
	ReasoningMedium  = "medium"
	ReasoningHigh    = "high"
)

// 响应状态。
const (
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusIncomplete = "incomplete"
	StatusCancelled  = "cancelled"
)

// 输入/输出项类型。
const (
	TypeMessage              = "message"
	TypeFunctionCall         = "function_call"
	TypeFunctionCallOutput   = "function_call_output"
	TypeReasoning            = "reasoning"
	TypeFileSearchCall       = "file_search_call"
	TypeWebSearchCall        = "web_search_call"
	TypeComputerCall         = "computer_call"
	TypeComputerCallOutput   = "computer_call_output"
	TypeImageGenerationCall  = "image_generation_call"
	TypeCodeInterpreterCall  = "code_interpreter_call"
	TypeCustomToolCall       = "custom_tool_call"
	TypeCustomToolCallOutput = "custom_tool_call_output"
	TypeCompaction           = "compaction"
)

// 内容类型。
const (
	ContentTypeInputText     = "input_text"
	ContentTypeOutputText    = "output_text"
	ContentTypeInputImage    = "input_image"
	ContentTypeInputFile     = "input_file"
	ContentTypeRefusal       = "refusal"
	ContentTypeText          = "text"
	ContentTypeSummaryText   = "summary_text"
	ContentTypeReasoningText = "reasoning_text"
)

// 工具类型。
const (
	ToolTypeFunction        = "function"
	ToolTypeWebSearch       = "web_search"
	ToolTypeFileSearch      = "file_search"
	ToolTypeCodeInterpreter = "code_interpreter"
	ToolTypeComputer        = "computer"
	ToolTypeImageGen        = "image_generation"
	ToolTypeMCP             = "mcp"
)

// ============================================================================
// Responses API — 请求
// ============================================================================

// ReasoningConfig 推理配置。
type ReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`  // minimal/low/medium/high
	Summary string `json:"summary,omitempty"` // auto/concise/detailed
}

// TextFormatConfig 文本输出格式配置（结构化输出）。
type TextFormatConfig struct {
	Type string `json:"type"` // "json_schema" 或 "text"
	// JSONSchema 配置（type=json_schema 时）。
	Schema json.RawMessage `json:"json_schema,omitempty"`
	// Name JSON Schema 名称。
	Name string `json:"name,omitempty"`
	// Strict 是否严格模式。
	Strict *bool `json:"strict,omitempty"`
}

// TextConfig 文本输出配置。
type TextConfig struct {
	Format *TextFormatConfig `json:"format,omitempty"`
}

// FunctionTool 函数工具定义。
type FunctionTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// WebSearchTool 网络搜索工具。
type WebSearchTool struct {
	Type              string `json:"type"`                          // "web_search"
	SearchContextSize string `json:"search_context_size,omitempty"` // low/medium/high
}

// FileSearchTool 文件搜索工具。
type FileSearchTool struct {
	Type           string   `json:"type"` // "file_search"
	VectorStoreIDs []string `json:"vector_store_ids"`
	MaxNumResults  *int     `json:"max_num_results,omitempty"`
}

// CodeInterpreterTool 代码解释器工具。
type CodeInterpreterTool struct {
	Type      string   `json:"type"` // "code_interpreter"
	FileIDs   []string `json:"file_ids,omitempty"`
	Container string   `json:"container,omitempty"`
}

// CreateResponseRequest Responses API 创建请求体。
type CreateResponseRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input,omitempty"` // string 或 []InputItem
	Instructions       string            `json:"instructions,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	Reasoning          *ReasoningConfig  `json:"reasoning,omitempty"`
	Text               *TextConfig       `json:"text,omitempty"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage   `json:"tool_choice,omitempty"` // string 或 object
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	Truncation         string            `json:"truncation,omitempty"`
	User               string            `json:"user,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Include            []string          `json:"include,omitempty"`
}

// InputItem 表示 Responses API 输入中的一个项。
//
// Type 决定该项的类型。常用类型：
//   - "message": 消息（role + content）
//   - "function_call_output": 函数调用结果
//   - "reasoning": 推理上下文（用于多轮）
type InputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // string 或 []ContentPart
	ID      string          `json:"id,omitempty"`
	Status  string          `json:"status,omitempty"`
	// FunctionCallOutput
	CallID string `json:"call_id,omitempty"`
	Output string `json:"output,omitempty"`
	// FunctionCall
	Arguments string `json:"arguments,omitempty"`
	Name      string `json:"name,omitempty"`
	// Reasoning
	Summary          json.RawMessage `json:"summary,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
}

// SummaryItem 表示 reasoning summary 中的一个条目。
type SummaryItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ContentPart 多模态内容部分。
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
}

// ============================================================================
// Responses API — 响应
// ============================================================================

// Response Responses API 响应对象。
type Response struct {
	ID                 string             `json:"id"`
	Object             string             `json:"object"` // "response"
	CreatedAt          int64              `json:"created_at"`
	Status             string             `json:"status"`
	Model              string             `json:"model"`
	Output             []OutputItem       `json:"output"`
	Usage              *ResponseUsage     `json:"usage,omitempty"`
	Instructions       string             `json:"instructions,omitempty"`
	PreviousResponseID string             `json:"previous_response_id,omitempty"`
	Temperature        *float64           `json:"temperature,omitempty"`
	TopP               *float64           `json:"top_p,omitempty"`
	MaxOutputTokens    *int               `json:"max_output_tokens,omitempty"`
	Reasoning          *ReasoningConfig   `json:"reasoning,omitempty"`
	Tools              []json.RawMessage  `json:"tools,omitempty"`
	ToolChoice         json.RawMessage    `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool              `json:"parallel_tool_calls,omitempty"`
	Store              *bool              `json:"store,omitempty"`
	Truncation         string             `json:"truncation,omitempty"`
	User               string             `json:"user,omitempty"`
	Metadata           map[string]string  `json:"metadata,omitempty"`
	IncompleteDetails  *IncompleteDetails `json:"incomplete_details,omitempty"`
	Error              *ResponseError     `json:"error,omitempty"`
	Text               *TextConfig        `json:"text,omitempty"`
}

// OutputItem Responses API 输出项。
//
// 使用 RawMessage 存储完整 JSON，便于按类型解析。
type OutputItem struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Status  string          `json:"status,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// FunctionCall
	Arguments string `json:"arguments,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	// Reasoning
	Summary          json.RawMessage `json:"summary,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	// FunctionCallOutput
	Output string `json:"output,omitempty"`
	// WebSearchCall / FileSearchCall
	Queries []string `json:"queries,omitempty"`
	// FileSearchCall
	Results json.RawMessage `json:"results,omitempty"`
	// WebSearchCall
	Action json.RawMessage `json:"action,omitempty"`
	// ImageGenerationCall
	Result string `json:"result,omitempty"`
	// CodeInterpreterCall
	Code        string          `json:"code,omitempty"`
	ContainerID string          `json:"container_id,omitempty"`
	Outputs     json.RawMessage `json:"outputs,omitempty"`
	// CustomToolCall
	Input string `json:"input,omitempty"`
}

// ResponseUsage token 用量统计。
type ResponseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	InputTokensDetails  *TokenDetails `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *TokenDetails `json:"output_tokens_details,omitempty"`
}

// TokenDetails token 用量明细。
type TokenDetails struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// IncompleteDetails 响应不完整详情。
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

// ResponseError 响应错误。
type ResponseError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ============================================================================
// Responses API — 辅助响应类型
// ============================================================================

// MessageOutput 从 OutputItem 解析出的消息内容。
type MessageOutput struct {
	ID      string           `json:"id"`
	Role    string           `json:"role"`
	Status  string           `json:"status"`
	Content []MessageContent `json:"content"`
}

// MessageContent 消息内容项。
type MessageContent struct {
	Type        string          `json:"type"`
	Text        string          `json:"text,omitempty"`
	Refusal     string          `json:"refusal,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

// FunctionCallOutput 从 OutputItem 解析出的函数调用。
type FunctionCallOutput struct {
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

// ============================================================================
// 流式事件
// ============================================================================

// StreamEvent 表示一个 Responses API 流式事件。
type StreamEvent struct {
	Type         string          `json:"type"`
	Response     *Response       `json:"response,omitempty"`
	Item         *OutputItem     `json:"item,omitempty"`
	ItemID       string          `json:"item_id,omitempty"`
	OutputIndex  int             `json:"output_index,omitempty"`
	ContentIndex int             `json:"content_index,omitempty"`
	Delta        string          `json:"delta,omitempty"`
	Text         string          `json:"text,omitempty"`
	Parsed       json.RawMessage `json:"parsed,omitempty"`
	Sequence     int             `json:"sequence_number,omitempty"`
}

// 流式事件类型常量。
const (
	EventResponseCreated                    = "response.created"
	EventResponseInProgress                 = "response.in_progress"
	EventResponseOutputItemAdded            = "response.output_item.added"
	EventResponseContentPartAdded           = "response.content_part.added"
	EventResponseOutputTextDelta            = "response.output_text.delta"
	EventResponseOutputTextDone             = "response.output_text.done"
	EventResponseRefusalDelta               = "response.refusal.delta"
	EventResponseRefusalDone                = "response.refusal.done"
	EventResponseFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	EventResponseFunctionCallArgumentsDone  = "response.function_call_arguments.done"
	EventResponseContentPartDone            = "response.content_part.done"
	EventResponseOutputItemDone             = "response.output_item.done"
	EventResponseWebSearchCallSearching     = "response.web_search_call.searching"
	EventResponseWebSearchCallCompleted     = "response.web_search_call.completed"
	EventResponseFileSearchCallSearching    = "response.file_search_call.searching"
	EventResponseFileSearchCallCompleted    = "response.file_search_call.completed"
	EventResponseCompleted                  = "response.completed"
	EventResponseFailed                     = "response.failed"
	EventResponseIncomplete                 = "response.incomplete"
	EventResponseReasoningDelta             = "response.reasoning.delta"
	EventResponseReasoningDone              = "response.reasoning.done"
	EventResponseError                      = "error"
)

// ============================================================================
// Audio — Speech (TTS)
// ============================================================================

// SpeechRequest TTS 请求体。
type SpeechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format,omitempty"` // mp3/opus/aac/flac/wav/pcm
	Speed          *float64 `json:"speed,omitempty"`
	Instructions   string   `json:"instructions,omitempty"`
	StreamFormat   string   `json:"stream_format,omitempty"` // sse/audio
}

// ============================================================================
// Audio — Translation
// ============================================================================

// TranslationRequest 翻译请求参数（multipart/form-data）。
type TranslationRequest struct {
	File           []byte
	Filename       string
	Model          string
	Prompt         string
	ResponseFormat string // json/text/srt/verbose_json/vtt
	Temperature    *float64
}

// TranslationResponse 翻译响应。
type TranslationResponse struct {
	Text     string               `json:"text,omitempty"`
	Duration float64              `json:"duration,omitempty"`
	Language string               `json:"language,omitempty"`
	Segments []TranslationSegment `json:"segments,omitempty"`
}

// TranslationSegment 翻译片段。
type TranslationSegment struct {
	ID               int     `json:"id"`
	Text             string  `json:"text"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Temperature      float64 `json:"temperature,omitempty"`
	AvgLogprob       float64 `json:"avg_logprob,omitempty"`
	CompressionRatio float64 `json:"compression_ratio,omitempty"`
	NoSpeechProb     float64 `json:"no_speech_prob,omitempty"`
	Seek             int     `json:"seek,omitempty"`
	Tokens           []int   `json:"tokens,omitempty"`
}

// ============================================================================
// Audio — Voice Creation
// ============================================================================

// VoiceCreateRequest 创建自定义语音请求（multipart/form-data）。
type VoiceCreateRequest struct {
	AudioSample []byte
	Filename    string
	ContentType string // MIME type
	Consent     string
	Name        string
}

// Voice 自定义语音。
type Voice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Object    string `json:"object"` // "audio.voice"
	CreatedAt int64  `json:"created_at"`
}

// ============================================================================
// Models
// ============================================================================

// Model 模型信息。
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ListModelsResponse 模型列表响应。
type ListModelsResponse struct {
	Object string  `json:"object"` // "list"
	Data   []Model `json:"data"`
}

// ============================================================================
// Error
// ============================================================================

// APIError OpenAI API 错误。
type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// ErrorResponse API 错误响应体。
type ErrorResponse struct {
	Error APIError `json:"error"`
}
