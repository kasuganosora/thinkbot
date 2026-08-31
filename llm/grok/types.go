package grok

import "encoding/json"

// ============================================================================
// 常量
// ============================================================================

const (
	// DefaultBaseURL 默认 API 基础地址。
	DefaultBaseURL = "https://api.x.ai"

	// RoleSystem 系统角色。
	RoleSystem = "system"
	// RoleUser 用户角色。
	RoleUser = "user"
	// RoleAssistant 助手角色。
	RoleAssistant = "assistant"
	// RoleTool 工具角色。
	RoleTool = "tool"
)

// 模型名称。
const (
	// 文本模型
	ModelGrok43              = "grok-4.3"
	ModelGrok420NonReasoning = "grok-4.20-0309-non-reasoning"
	ModelGrok420Reasoning    = "grok-4.20-0309-reasoning"
	ModelGrok420MultiAgent   = "grok-4.20-multi-agent-0309"
	ModelGrokBuild           = "grok-build-0.1"

	// 图像模型
	ModelGrokImage        = "grok-imagine-image"
	ModelGrokImageQuality = "grok-imagine-image-quality"

	// 视频模型
	ModelGrokVideo          = "grok-imagine-video"
	ModelGrokVideo15Preview = "grok-imagine-video-1.5-preview"
)

// ReasoningEffort 推理努力程度。
type ReasoningEffort string

const (
	// ReasoningNone 禁用推理。
	ReasoningNone ReasoningEffort = "none"
	// ReasoningLow 少量推理 token（默认）。
	ReasoningLow ReasoningEffort = "low"
	// ReasoningMedium 适度推理。
	ReasoningMedium ReasoningEffort = "medium"
	// ReasoningHigh 深度推理。
	ReasoningHigh ReasoningEffort = "high"
)

// FinishReason 完成原因。
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"
	FinishReasonLength        FinishReason = "length"
	FinishReasonToolCalls     FinishReason = "tool_calls"
	FinishReasonContentFilter FinishReason = "content_filter"
)

// 内容块类型。
const (
	ContentTypeText     = "text"
	ContentTypeImageURL = "image_url"
)

// ResponseFormatType 响应格式类型。
type ResponseFormatType string

const (
	ResponseFormatText       ResponseFormatType = "text"
	ResponseFormatJSONObject ResponseFormatType = "json_object"
	ResponseFormatJSONSchema ResponseFormatType = "json_schema"
)

// 图片响应格式。
const (
	ImageFormatURL    = "url"
	ImageFormatBase64 = "b64_json"
)

// 视频状态。
const (
	VideoStatusPending = "pending"
	VideoStatusDone    = "done"
	VideoStatusExpired = "expired"
	VideoStatusFailed  = "failed"
)

// 视频分辨率。
const (
	VideoResolution480p = "480p"
	VideoResolution720p = "720p"
)

// ============================================================================
// Chat Completions
// ============================================================================

// Message 表示对话中的一条消息。
//
// Content 可以是 string 或 []ContentPart。对于多模态消息（图片+文本）使用 ContentPart。
type Message struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`                     // string 或 []ContentPart
	ReasoningContent string          `json:"reasoning_content,omitempty"` // 推理内容（streaming/响应中返回）
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
}

// ContentPart 表示多模态消息的一个内容部分。
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL 图片 URL。
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto", "low", "high"
}

// ToolCall 工具调用。
type ToolCall struct {
	Index    int          `json:"index,omitempty"` // 流式 delta 中的索引
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall 函数调用。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// Tool 定义一个可调用的工具（function）。
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction 工具函数定义。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponseFormat 响应格式控制。
type ResponseFormat struct {
	Type       ResponseFormatType `json:"type"`
	JSONSchema *JSONSchemaConfig  `json:"json_schema,omitempty"`
}

// JSONSchemaConfig JSON Schema 配置。
type JSONSchemaConfig struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict *bool           `json:"strict,omitempty"`
}

// ChatCompletionRequest Chat Completions 请求体。
type ChatCompletionRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Temperature      *float64        `json:"temperature,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"` // string 或 []string
	N                *int            `json:"n,omitempty"`
	ReasoningEffort  ReasoningEffort `json:"reasoning_effort,omitempty"`
	ResponseFormat   *ResponseFormat `json:"response_format,omitempty"`
	Tools            []Tool          `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"` // string 或 object
	Seed             *int            `json:"seed,omitempty"`
	User             string          `json:"user,omitempty"`
}

// StreamOptions 流式选项。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatCompletionResponse Chat Completions 响应体。
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice 表示一个响应选项。
type Choice struct {
	Index        int          `json:"index"`
	Message      Message      `json:"message"`
	Delta        Delta        `json:"delta,omitempty"` // streaming
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// Delta 流式增量内容。
type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Usage token 用量统计。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ============================================================================
// Images
// ============================================================================

// ImageRequest 图片生成/编辑请求体。
type ImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"` // "url" 或 "b64_json"
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	Resolution     string `json:"resolution,omitempty"` // "1k" 或 "2k"
	ImageURL       string `json:"image,omitempty"`      // 编辑：图片 URL 或 data URI
}

// ImageResponse 图片生成/编辑响应体。
type ImageResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
	Model   string      `json:"model,omitempty"`
}

// ImageData 单张图片数据。
type ImageData struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ============================================================================
// Video
// ============================================================================

// VideoGenerationRequest 视频生成请求体。
type VideoGenerationRequest struct {
	Model       string      `json:"model"`
	Prompt      string      `json:"prompt"`
	Image       *VideoImage `json:"image,omitempty"`
	Duration    *int        `json:"duration,omitempty"`
	AspectRatio string      `json:"aspect_ratio,omitempty"`
	Resolution  string      `json:"resolution,omitempty"`
}

// VideoImage 视频生成的输入图片。
type VideoImage struct {
	URL string `json:"url"`
}

// VideoStartResponse 视频生成请求的初始响应。
type VideoStartResponse struct {
	RequestID string `json:"request_id"`
}

// VideoStatusResponse 视频生成状态响应。
type VideoStatusResponse struct {
	Status string       `json:"status"` // pending, done, expired, failed
	Video  *VideoResult `json:"video,omitempty"`
	Model  string       `json:"model,omitempty"`
	Error  *VideoError  `json:"error,omitempty"`
}

// VideoResult 视频生成结果。
type VideoResult struct {
	URL               string `json:"url"`
	Duration          int    `json:"duration,omitempty"`
	RespectModeration *bool  `json:"respect_moderation,omitempty"`
}

// VideoError 视频生成错误。
type VideoError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ============================================================================
// Audio — Text to Speech
// ============================================================================

// TTSRequest 文本转语音请求体。
type TTSRequest struct {
	Text                     string           `json:"text"`
	VoiceID                  string           `json:"voice_id,omitempty"`
	Language                 string           `json:"language"`
	OutputFormat             *TTSOutputFormat `json:"output_format,omitempty"`
	Speed                    *float64         `json:"speed,omitempty"`
	OptimizeStreamingLatency *int             `json:"optimize_streaming_latency,omitempty"`
	TextNormalization        *bool            `json:"text_normalization,omitempty"`
}

// TTSOutputFormat TTS 输出格式。
type TTSOutputFormat struct {
	Codec      string `json:"codec,omitempty"`       // mp3, wav, pcm, mulaw, alaw
	SampleRate int    `json:"sample_rate,omitempty"` // Hz
	BitRate    int    `json:"bit_rate,omitempty"`    // MP3 only
}

// TTSVoice TTS 语音。
type TTSVoice struct {
	VoiceID string `json:"voice_id"`
	Name    string `json:"name"`
}

// ListVoicesResponse 语音列表响应。
type ListVoicesResponse struct {
	Voices []TTSVoice `json:"voices"`
}

// ============================================================================
// Audio — Speech to Text
// ============================================================================

// STTRequest 语音转文本请求参数。
type STTRequest struct {
	// File 音频文件内容（与 URL 二选一）。
	File []byte
	// Filename 文件名。
	Filename string
	// URL 远程音频文件 URL（与 File 二选一）。
	URL string
	// AudioFormat 原始音频格式提示：pcm, mulaw, alaw。
	AudioFormat string
	// SampleRate 采样率（Hz），仅原始音频需要。
	SampleRate int
	// Language 语言代码。
	Language string
	// Format 启用文本格式化。
	Format bool
	// Multichannel 多通道独立转录。
	Multichannel bool
	// Channels 音频通道数（2-8），仅多通道原始音频需要。
	Channels int
	// Diarize 说话人分离。
	Diarize bool
	// KeyTerms 关键词列表（最多 100 个，每个最多 50 字符）。
	KeyTerms []string
	// FillerWords 保留填充词。
	FillerWords bool
}

// STTResponse 语音转文本响应。
type STTResponse struct {
	Text     string       `json:"text"`
	Language string       `json:"language,omitempty"`
	Duration float64      `json:"duration,omitempty"`
	Words    []STTWord    `json:"words,omitempty"`
	Channels []STTChannel `json:"channels,omitempty"`
}

// STTWord 词级时间戳。
type STTWord struct {
	Text    string  `json:"text"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker int     `json:"speaker,omitempty"`
}

// STTChannel 通道转录结果。
type STTChannel struct {
	Index int       `json:"index"`
	Text  string    `json:"text"`
	Words []STTWord `json:"words,omitempty"`
}

// ============================================================================
// Files
// ============================================================================

// FileInfo 文件元数据。
type FileInfo struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Object    string `json:"object"` // "file"
	TeamID    string `json:"team_id,omitempty"`
}

// ListFilesResponse 文件列表响应。
type ListFilesResponse struct {
	Data            []FileInfo `json:"data"`
	Object          string     `json:"object"`
	PaginationToken string     `json:"pagination_token,omitempty"`
}

// DeleteFileResponse 删除文件响应。
type DeleteFileResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "file"
	Deleted bool   `json:"deleted"`
}

// ============================================================================
// Error
// ============================================================================

// APIError xAI API 错误。
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
