package google

import "encoding/json"

// ============================================================================
// 常量
// ============================================================================

const (
	// DefaultBaseURL 默认 API 基础地址。
	DefaultBaseURL = "https://generativelanguage.googleapis.com"

	// RoleUser 用户角色。
	RoleUser = "user"
	// RoleModel 模型角色。
	RoleModel = "model"
)

// FinishReason 完成原因。
type FinishReason string

const (
	FinishReasonStop                  FinishReason = "STOP"
	FinishReasonMaxTokens             FinishReason = "MAX_TOKENS"
	FinishReasonSafety                FinishReason = "SAFETY"
	FinishReasonRecitation            FinishReason = "RECITATION"
	FinishReasonOther                 FinishReason = "OTHER"
	FinishReasonBlocklist             FinishReason = "BLOCKLIST"
	FinishReasonProhibited            FinishReason = "PROHIBITED"
	FinishReasonSPII                  FinishReason = "SPII"
	FinishReasonMalformedFunctionCall FinishReason = "MALFORMED_FUNCTION_CALL"
)

// HarmCategory 伤害类别。
type HarmCategory string

const (
	HarmCategoryHarassment       HarmCategory = "HARM_CATEGORY_HARASSMENT"
	HarmCategoryHateSpeech       HarmCategory = "HARM_CATEGORY_HATE_SPEECH"
	HarmCategorySexuallyExplicit HarmCategory = "HARM_CATEGORY_SEXUALLY_EXPLICIT"
	HarmCategoryDangerousContent HarmCategory = "HARM_CATEGORY_DANGEROUS_CONTENT"
	HarmCategoryCivicIntegrity   HarmCategory = "HARM_CATEGORY_CIVIC_INTEGRITY"
)

// HarmProbability 伤害概率。
type HarmProbability string

const (
	HarmProbabilityNegligible HarmProbability = "NEGLIGIBLE"
	HarmProbabilityLow        HarmProbability = "LOW"
	HarmProbabilityMedium     HarmProbability = "MEDIUM"
	HarmProbabilityHigh       HarmProbability = "HIGH"
)

// HarmBlockThreshold 伤害阻断阈值。
type HarmBlockThreshold string

const (
	HarmBlockThresholdUnspecified         HarmBlockThreshold = "HARM_BLOCK_THRESHOLD_UNSPECIFIED"
	HarmBlockThresholdBlockLowAndAbove    HarmBlockThreshold = "BLOCK_LOW_AND_ABOVE"
	HarmBlockThresholdBlockMediumAndAbove HarmBlockThreshold = "BLOCK_MEDIUM_AND_ABOVE"
	HarmBlockThresholdBlockOnlyHigh       HarmBlockThreshold = "BLOCK_ONLY_HIGH"
	HarmBlockThresholdBlockNone           HarmBlockThreshold = "BLOCK_NONE"
	HarmBlockThresholdOff                 HarmBlockThreshold = "OFF"
)

// FunctionCallingMode 函数调用模式。
type FunctionCallingMode string

const (
	FunctionCallingModeAuto      FunctionCallingMode = "AUTO"
	FunctionCallingModeAny       FunctionCallingMode = "ANY"
	FunctionCallingModeNone      FunctionCallingMode = "NONE"
	FunctionCallingModeValidated FunctionCallingMode = "VALIDATED"
)

// ThinkingLevel 思考级别（Gemini 3）。
type ThinkingLevel string

const (
	ThinkingLevelMinimal ThinkingLevel = "minimal"
	ThinkingLevelLow     ThinkingLevel = "low"
	ThinkingLevelMedium  ThinkingLevel = "medium"
	ThinkingLevelHigh    ThinkingLevel = "high"
)

// ResponseModality 响应模态类型。
type ResponseModality string

const (
	ModalityText  ResponseModality = "TEXT"
	ModalityImage ResponseModality = "IMAGE"
)

// MediaResolution 媒体分辨率（控制每张图片/视频帧分配的 token 数）。
type MediaResolution string

const (
	MediaResolutionUnspecified MediaResolution = "MEDIA_RESOLUTION_UNSPECIFIED"
	MediaResolutionLow         MediaResolution = "MEDIA_RESOLUTION_LOW"
	MediaResolutionMedium      MediaResolution = "MEDIA_RESOLUTION_MEDIUM"
	MediaResolutionHigh        MediaResolution = "MEDIA_RESOLUTION_HIGH"
)

// AspectRatio 宽高比常量。
const (
	AspectRatio1_1  = "1:1"
	AspectRatio1_4  = "1:4"
	AspectRatio1_8  = "1:8"
	AspectRatio2_3  = "2:3"
	AspectRatio3_2  = "3:2"
	AspectRatio3_4  = "3:4"
	AspectRatio4_1  = "4:1"
	AspectRatio4_3  = "4:3"
	AspectRatio4_5  = "4:5"
	AspectRatio5_4  = "5:4"
	AspectRatio8_1  = "8:1"
	AspectRatio9_16 = "9:16"
	AspectRatio16_9 = "16:9"
	AspectRatio21_9 = "21:9"
)

// ImageSize 图片分辨率常量。
const (
	ImageSize05K = "512"
	ImageSize1K  = "1K"
	ImageSize2K  = "2K"
	ImageSize4K  = "4K"
)

// MIME 类型常量 — 图片。
const (
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
	MIMEWEBP = "image/webp"
	MIMEHEIC = "image/heic"
	MIMEHEIF = "image/heif"
)

// MIME 类型常量 — 音频。
const (
	MIMEWAV  = "audio/wav"
	MIMEMP3  = "audio/mp3"
	MIMEAIFF = "audio/aiff"
	MIMEAAC  = "audio/aac"
	MIMEOGG  = "audio/ogg"
	MIMEFLAC = "audio/flac"
)

// MIME 类型常量 — 视频。
const (
	MIMEVideoMP4  = "video/mp4"
	MIMEVideoMPEG = "video/mpeg"
	MIMEVideoMOV  = "video/mov"
	MIMEVideoAVI  = "video/avi"
	MIMEVideoFLV  = "video/x-flv"
	MIMEVideoMKV  = "video/x-matroska"
	MIMEVideoWebM = "video/webm"
	MIMEVideoWMV  = "video/wmv"
	MIMEVideo3GPP = "video/3gpp"
)

// 图片生成模型名称。
const (
	ModelGemini31FlashImage = "gemini-3.1-flash-image" // Nano Banana 2
	ModelGemini3ProImage    = "gemini-3-pro-image"     // Nano Banana Pro
	ModelGemini25FlashImage = "gemini-2.5-flash-image" // Nano Banana
)

// 虚拟思考签名（用于跳过签名验证或转移历史记录）。
//
// 当从其他模型转移历史记录或注入没有关联签名的自定义函数调用块时，
// 可使用这些虚拟值跳过 Gemini 3 的签名验证。
// Google 强烈建议不要注入自定义函数调用块。
const (
	ThoughtSignatureSkip  = "skip_thought_signature_validator"
	ThoughtSignatureDummy = "context_engineering_is_the_way_to_go"
)

// ============================================================================
// Content & Part
// ============================================================================

// Content 表示对话中的一条消息。
type Content struct {
	Role  string `json:"role,omitempty"` // "user" 或 "model"
	Parts []Part `json:"parts"`
}

// Part 表示内容中的一个部分（文本、图片、函数调用等）。
//
// 不同类型使用不同字段：
//   - 文本:         Text
//   - 内联数据:      InlineData（base64 编码的图片/音频等）
//   - 文件引用:      FileData（通过 URI 引用）
//   - 函数调用:      FunctionCall
//   - 函数响应:      FunctionResponse
//   - 思考摘要:      Text + Thought = true（仅响应中）
type Part struct {
	// 文本内容
	Text string `json:"text,omitempty"`

	// 内联二进制数据
	InlineData *Blob `json:"inlineData,omitempty"`

	// 文件引用
	FileData *FileData `json:"fileData,omitempty"`

	// 函数调用
	FunctionCall *FunctionCall `json:"functionCall,omitempty"`

	// 函数响应
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`

	// 可执行代码
	ExecutableCode *ExecutableCode `json:"executableCode,omitempty"`

	// 代码执行结果
	CodeExecutionResult *CodeExecutionResult `json:"codeExecutionResult,omitempty"`

	// 思考摘要标记（仅响应中，Text 为思考摘要内容）
	Thought bool `json:"thought,omitempty"`

	// 思考签名（用于多轮函数调用上下文）
	ThoughtSignature string `json:"thoughtSignature,omitempty"`

	// 视频元数据（仅对视频 Part 有效）
	VideoMetadata *VideoMetadata `json:"videoMetadata,omitempty"`
}

// TextPart 创建一个文本 Part。
func TextPart(text string) Part {
	return Part{Text: text}
}

// ThoughtPart 创建一个思考摘要 Part（仅出现在响应中）。
func ThoughtPart(text string) Part {
	return Part{Text: text, Thought: true}
}

// InlineDataPart 创建一个内联数据 Part。
func InlineDataPart(mimeType, base64Data string) Part {
	return Part{InlineData: &Blob{MimeType: mimeType, Data: base64Data}}
}

// FileDataPart 创建一个文件引用 Part。
func FileDataPart(mimeType, fileURI string) Part {
	return Part{FileData: &FileData{MimeType: mimeType, FileURI: fileURI}}
}

// FunctionCallPart 创建一个函数调用 Part。
func FunctionCallPart(name string, args map[string]any) Part {
	return Part{FunctionCall: &FunctionCall{Name: name, Args: args}}
}

// FunctionCallPartWithID 创建一个带 ID 的函数调用 Part。
func FunctionCallPartWithID(name, id string, args map[string]any) Part {
	return Part{FunctionCall: &FunctionCall{Name: name, ID: id, Args: args}}
}

// FunctionResponsePart 创建一个函数响应 Part。
func FunctionResponsePart(name string, response map[string]any) Part {
	return Part{FunctionResponse: &FunctionResponse{Name: name, Response: response}}
}

// FunctionResponsePartWithID 创建一个带 ID 的函数响应 Part。
func FunctionResponsePartWithID(name, id string, response map[string]any) Part {
	return Part{FunctionResponse: &FunctionResponse{Name: name, ID: id, Response: response}}
}

// FunctionCallPartWithSignature 创建一个带思考签名的函数调用 Part。
//
// 在多轮函数调用中，必须将模型返回的 thoughtSignature 原样传回。
func FunctionCallPartWithSignature(name, signature string, args map[string]any) Part {
	return Part{FunctionCall: &FunctionCall{Name: name, Args: args}, ThoughtSignature: signature}
}

// FunctionCallPartWithIDAndSignature 创建一个带 ID 和思考签名的函数调用 Part。
func FunctionCallPartWithIDAndSignature(name, id, signature string, args map[string]any) Part {
	return Part{FunctionCall: &FunctionCall{Name: name, ID: id, Args: args}, ThoughtSignature: signature}
}

// TextPartWithSignature 创建一个带思考签名的文本 Part。
//
// 模型响应中的最后一个内容部分（如 text）可能携带 thought_signature，
// 建议在下一轮中原样传回。
func TextPartWithSignature(text, signature string) Part {
	return Part{Text: text, ThoughtSignature: signature}
}

// SignaturePart 创建一个仅包含思考签名的 Part。
//
// 流式传输中，thought_signature 可能出现在包含空文本的部分中。
// 使用此构造器在多轮对话中回传此类签名。
func SignaturePart(signature string) Part {
	return Part{ThoughtSignature: signature}
}

// ============================================================================
// 多模态 Part 构造器
// ============================================================================

// ImagePart 创建一个内联图片 Part（base64 编码）。
//
// mimeType 应为 image/png、image/jpeg 等。
// base64Data 为图片的 base64 编码字符串。
func ImagePart(mimeType, base64Data string) Part {
	return Part{InlineData: &Blob{MimeType: mimeType, Data: base64Data}}
}

// ImageFilePart 创建一个通过 File URI 引用的图片 Part。
//
// mimeType 应为 image/png、image/jpeg 等。
// fileURI 为通过 Files API 上传后获得的 URI。
func ImageFilePart(mimeType, fileURI string) Part {
	return Part{FileData: &FileData{MimeType: mimeType, FileURI: fileURI}}
}

// AudioPart 创建一个内联音频 Part（base64 编码）。
func AudioPart(mimeType, base64Data string) Part {
	return Part{InlineData: &Blob{MimeType: mimeType, Data: base64Data}}
}

// AudioFilePart 创建一个通过 File URI 引用的音频 Part。
func AudioFilePart(mimeType, fileURI string) Part {
	return Part{FileData: &FileData{MimeType: mimeType, FileURI: fileURI}}
}

// VideoPart 创建一个内联视频 Part（base64 编码）。
func VideoPart(mimeType, base64Data string) Part {
	return Part{InlineData: &Blob{MimeType: mimeType, Data: base64Data}}
}

// VideoFilePart 创建一个通过 File URI 引用的视频 Part。
func VideoFilePart(mimeType, fileURI string) Part {
	return Part{FileData: &FileData{MimeType: mimeType, FileURI: fileURI}}
}

// VideoPartWithMetadata 创建一个带视频元数据的内联视频 Part。
func VideoPartWithMetadata(mimeType, base64Data string, fps float64) Part {
	return Part{
		InlineData:    &Blob{MimeType: mimeType, Data: base64Data},
		VideoMetadata: &VideoMetadata{FPS: fps},
	}
}

// VideoFilePartWithMetadata 创建一个带视频元数据的 File URI 视频 Part。
func VideoFilePartWithMetadata(mimeType, fileURI string, fps float64) Part {
	return Part{
		FileData:      &FileData{MimeType: mimeType, FileURI: fileURI},
		VideoMetadata: &VideoMetadata{FPS: fps},
	}
}

// YouTubePart 创建一个 YouTube 视频 Part。
//
// 用于图片生成模型（如 gemini-3.1-flash-image）或内容理解模型引用 YouTube 视频。
func YouTubePart(youtubeURL string) Part {
	return Part{FileData: &FileData{FileURI: youtubeURL}}
}

// YouTubePartWithMetadata 创建一个带视频元数据的 YouTube 视频 Part。
func YouTubePartWithMetadata(youtubeURL string, fps float64) Part {
	return Part{
		FileData:      &FileData{FileURI: youtubeURL},
		VideoMetadata: &VideoMetadata{FPS: fps},
	}
}

// ============================================================================
// Blob & FileData
// ============================================================================

// Blob 表示 base64 编码的二进制数据。
type Blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64 编码
}

// FileData 表示通过 URI 引用的文件。
type FileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

// VideoMetadata 视频元数据。
type VideoMetadata struct {
	// FPS 采样率（每秒采样的帧数），0 = 自动。
	FPS float64 `json:"fps,omitempty"`
}

// ResponseFormat 响应格式控制（图片生成专用）。
type ResponseFormat struct {
	Image *ImageResponseFormat `json:"image,omitempty"`
}

// ImageResponseFormat 图片响应格式配置。
type ImageResponseFormat struct {
	AspectRatio string `json:"aspectRatio,omitempty"` // 宽高比，如 "16:9"
	ImageSize   string `json:"imageSize,omitempty"`   // 分辨率，如 "1K"、"2K"、"4K"、"512"
}

// ============================================================================
// Function Calling
// ============================================================================

// FunctionCall 表示模型发起的函数调用。
type FunctionCall struct {
	Name string         `json:"name"`
	ID   string         `json:"id,omitempty"` // Gemini 3 每次调用都返回唯一 ID
	Args map[string]any `json:"args,omitempty"`
}

// FunctionResponse 表示函数执行结果。
type FunctionResponse struct {
	Name     string         `json:"name"`
	ID       string         `json:"id,omitempty"`
	Response map[string]any `json:"response"`
}

// FunctionDeclaration 声明一个可调用的函数。
type FunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

// Tool 定义一组工具。
type Tool struct {
	FunctionDeclarations  []FunctionDeclaration `json:"functionDeclarations,omitempty"`
	GoogleSearch          *struct{}             `json:"googleSearch,omitempty"`
	URLContext            *struct{}             `json:"urlContext,omitempty"`
	CodeExecution         *struct{}             `json:"codeExecution,omitempty"`
	GoogleSearchRetrieval *struct{}             `json:"googleSearchRetrieval,omitempty"`
}

// ToolConfig 工具配置。
type ToolConfig struct {
	FunctionCallingConfig *FunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// FunctionCallingConfig 函数调用配置。
type FunctionCallingConfig struct {
	Mode                 FunctionCallingMode `json:"mode,omitempty"`
	AllowedFunctionNames []string            `json:"allowedFunctionNames,omitempty"`
}

// ============================================================================
// Executable Code
// ============================================================================

// ExecutableCode 表示模型生成的可执行代码。
type ExecutableCode struct {
	Language string `json:"language"` // e.g. "PYTHON"
	Code     string `json:"code"`
}

// CodeExecutionResult 表示代码执行结果。
type CodeExecutionResult struct {
	Outcome string `json:"outcome"` // "OUTCOME_OK", "OUTCOME_FAILED", "OUTCOME_DEADLINE_EXCEEDED"
	Output  string `json:"output"`
}

// ============================================================================
// Generation Config
// ============================================================================

// ThinkingConfig 思考配置。
//
// Gemini 3 使用 ThinkingLevel，Gemini 2.5 使用 ThinkingBudget。
type ThinkingConfig struct {
	// IncludeThoughts 是否返回思考摘要。
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
	// ThinkingBudget 思考 token 预算（Gemini 2.5）。0=禁用，-1=动态。
	ThinkingBudget *int `json:"thinkingBudget,omitempty"`
	// ThinkingLevel 思考级别（Gemini 3）。
	ThinkingLevel ThinkingLevel `json:"thinkingLevel,omitempty"`
}

// GenerationConfig 生成配置。
type GenerationConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	TopK             *int            `json:"topK,omitempty"`
	MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	ThinkingConfig   *ThinkingConfig `json:"thinkingConfig,omitempty"`
	PresencePenalty  *float64        `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequencyPenalty,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	ResponseLogprobs *bool           `json:"responseLogprobs,omitempty"`
	Logprobs         *int            `json:"logprobs,omitempty"`

	// --- 多模态 ---

	// ResponseModalities 期望的响应模态（如 ["TEXT", "IMAGE"]）。
	// 图片生成模型（gemini-2.5-flash-image 等）需要设置此项。
	ResponseModalities []ResponseModality `json:"responseModalities,omitempty"`

	// ResponseFormat 图片生成格式控制（宽高比、分辨率）。
	ResponseFormat *ResponseFormat `json:"responseFormat,omitempty"`

	// MediaResolution 媒体分辨率（Gemini 3+），控制图片/视频帧的 token 分配。
	MediaResolution MediaResolution `json:"mediaResolution,omitempty"`
}

// SafetySetting 安全设置。
type SafetySetting struct {
	Category  HarmCategory       `json:"category"`
	Threshold HarmBlockThreshold `json:"threshold"`
}

// ============================================================================
// Request & Response
// ============================================================================

// GenerateContentRequest generateContent / streamGenerateContent 请求体。
type GenerateContentRequest struct {
	Contents          []Content         `json:"contents"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolConfig        *ToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	SafetySettings    []SafetySetting   `json:"safetySettings,omitempty"`
}

// Candidate 表示一个候选响应。
type Candidate struct {
	Content           Content            `json:"content"`
	FinishReason      FinishReason       `json:"finishReason,omitempty"`
	Index             int                `json:"index,omitempty"`
	SafetyRatings     []SafetyRating     `json:"safetyRatings,omitempty"`
	CitationMetadata  *CitationMetadata  `json:"citationMetadata,omitempty"`
	GroundingMetadata *GroundingMetadata `json:"groundingMetadata,omitempty"`
	FinishMessage     string             `json:"finishMessage,omitempty"`
}

// SafetyRating 安全评级。
type SafetyRating struct {
	Category    HarmCategory    `json:"category"`
	Probability HarmProbability `json:"probability"`
	Blocked     bool            `json:"blocked,omitempty"`
}

// CitationMetadata 引用元数据。
type CitationMetadata struct {
	Citations []Citation `json:"citations,omitempty"`
}

// Citation 引用。
type Citation struct {
	StartIndex      int    `json:"startIndex,omitempty"`
	EndIndex        int    `json:"endIndex,omitempty"`
	URI             string `json:"uri,omitempty"`
	Title           string `json:"title,omitempty"`
	License         string `json:"license,omitempty"`
	PublicationDate string `json:"publicationDate,omitempty"`
}

// GroundingMetadata Grounding 元数据。
type GroundingMetadata struct {
	SearchEntryPoint  *SearchEntryPoint  `json:"searchEntryPoint,omitempty"`
	GroundingChunks   []GroundingChunk   `json:"groundingChunks,omitempty"`
	GroundingSupports []GroundingSupport `json:"groundingSupports,omitempty"`
	WebSearchQueries  []string           `json:"webSearchQueries,omitempty"`
}

// SearchEntryPoint 搜索入口点。
type SearchEntryPoint struct {
	RenderedContent string `json:"renderedContent,omitempty"`
	SDBURL          string `json:"sdbUrl,omitempty"`
}

// GroundingChunk Grounding 数据块。
type GroundingChunk struct {
	Web *struct {
		URI   string `json:"uri,omitempty"`
		Title string `json:"title,omitempty"`
	} `json:"web,omitempty"`
}

// GroundingSupport Grounding 支持。
type GroundingSupport struct {
	GroundingChunkIndices []int   `json:"groundingChunkIndices,omitempty"`
	ConfidenceScore       float64 `json:"confidenceScore,omitempty"`
	Segment               *struct {
		StartIndex int    `json:"startIndex,omitempty"`
		EndIndex   int    `json:"endIndex,omitempty"`
		PartIndex  int    `json:"partIndex,omitempty"`
		Text       string `json:"text,omitempty"`
	} `json:"segment,omitempty"`
}

// PromptFeedback 提示反馈。
type PromptFeedback struct {
	BlockReason   string         `json:"blockReason,omitempty"`
	SafetyRatings []SafetyRating `json:"safetyRatings,omitempty"`
}

// GenerateContentResponse generateContent 响应体。
type GenerateContentResponse struct {
	Candidates     []Candidate     `json:"candidates,omitempty"`
	UsageMetadata  *UsageMetadata  `json:"usageMetadata,omitempty"`
	ModelVersion   string          `json:"modelVersion,omitempty"`
	PromptFeedback *PromptFeedback `json:"promptFeedback,omitempty"`
}

// UsageMetadata token 用量统计。
type UsageMetadata struct {
	PromptTokenCount     int                `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int                `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int                `json:"totalTokenCount,omitempty"`
	ThoughtsTokenCount   int                `json:"thoughtsTokenCount,omitempty"`
	CacheTokensDetails   []CacheTokenDetail `json:"cacheTokensDetails,omitempty"`
}

// CacheTokenDetail 缓存 token 明细。
type CacheTokenDetail struct {
	Role       string `json:"role,omitempty"`
	TokenCount int    `json:"tokenCount,omitempty"`
}

// ============================================================================
// Models
// ============================================================================

// Model 表示一个可用模型。
type Model struct {
	Name                       string   `json:"name"`
	Version                    string   `json:"version,omitempty"`
	DisplayName                string   `json:"displayName,omitempty"`
	Description                string   `json:"description,omitempty"`
	InputTokenLimit            int      `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           int      `json:"outputTokenLimit,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
}

// ListModelsResponse 列表模型响应体。
type ListModelsResponse struct {
	Models        []Model `json:"models"`
	NextPageToken string  `json:"nextPageToken,omitempty"`
}

// ============================================================================
// Count Tokens
// ============================================================================

// CountTokensRequest countTokens 请求体。
type CountTokensRequest struct {
	Contents          []Content         `json:"contents"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
}

// CountTokensResponse countTokens 响应体。
type CountTokensResponse struct {
	TotalTokens             int `json:"totalTokens"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

// ============================================================================
// Error
// ============================================================================

// APIError Google API 错误。
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
}

func (e *APIError) Error() string {
	return e.Message
}

// ErrorResponse Google API 错误响应体。
type ErrorResponse struct {
	Error *APIError `json:"error"`
}
