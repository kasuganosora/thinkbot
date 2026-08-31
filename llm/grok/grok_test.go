package grok

import (
	"encoding/json"
	"testing"
)

// ============================================================================
// 消息构造辅助函数测试
// ============================================================================

func TestSystemMessage(t *testing.T) {
	msg := SystemMessage("You are helpful.")
	if msg.Role != RoleSystem {
		t.Errorf("expected role %q, got %q", RoleSystem, msg.Role)
	}
	var s string
	if err := json.Unmarshal(msg.Content, &s); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if s != "You are helpful." {
		t.Errorf("expected 'You are helpful.', got %q", s)
	}
}

func TestUserMessage(t *testing.T) {
	msg := UserMessage("Hello")
	if msg.Role != RoleUser {
		t.Errorf("expected role %q, got %q", RoleUser, msg.Role)
	}
	var s string
	if err := json.Unmarshal(msg.Content, &s); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if s != "Hello" {
		t.Errorf("expected 'Hello', got %q", s)
	}
}

func TestAssistantMessage(t *testing.T) {
	msg := AssistantMessage("Hi there")
	if msg.Role != RoleAssistant {
		t.Errorf("expected role %q, got %q", RoleAssistant, msg.Role)
	}
}

func TestToolMessage(t *testing.T) {
	msg := ToolMessage("call_123", "result data")
	if msg.Role != RoleTool {
		t.Errorf("expected role %q, got %q", RoleTool, msg.Role)
	}
	if msg.ToolCallID != "call_123" {
		t.Errorf("expected tool_call_id 'call_123', got %q", msg.ToolCallID)
	}
}

func TestUserMessageWithImage(t *testing.T) {
	msg := UserMessageWithImage("What's this?", "https://example.com/img.png")
	if msg.Role != RoleUser {
		t.Errorf("expected role %q", RoleUser)
	}

	var parts []ContentPart
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].Type != ContentTypeImageURL {
		t.Errorf("expected first part type %q", ContentTypeImageURL)
	}
	if parts[0].ImageURL == nil || parts[0].ImageURL.URL != "https://example.com/img.png" {
		t.Error("unexpected image URL")
	}
	if parts[1].Type != ContentTypeText {
		t.Errorf("expected second part type %q", ContentTypeText)
	}
	if parts[1].Text != "What's this?" {
		t.Errorf("expected 'What's this?', got %q", parts[1].Text)
	}
}

func TestUserMessageWithBase64Image(t *testing.T) {
	msg := UserMessageWithBase64Image("desc", "image/jpeg", "aGVsbG8=")
	var parts []ContentPart
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parts[0].ImageURL.URL != "data:image/jpeg;base64,aGVsbG8=" {
		t.Errorf("unexpected data URI: %q", parts[0].ImageURL.URL)
	}
}

// ============================================================================
// ChatCompletionRequest 序列化测试
// ============================================================================

func TestChatCompletionRequestJSON(t *testing.T) {
	temp := 0.7
	maxTok := 1024
	req := ChatCompletionRequest{
		Model:       ModelGrok43,
		Messages:    []Message{UserMessage("Hi")},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["model"] != ModelGrok43 {
		t.Errorf("expected model %q", ModelGrok43)
	}
	if m["stream"] == true {
		t.Error("stream should be absent or false")
	}
	if m["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", m["temperature"])
	}
	if m["max_tokens"] != float64(1024) {
		t.Errorf("expected max_tokens 1024")
	}
}

func TestRequestWithReasoningEffort(t *testing.T) {
	temp := 0.5
	req := ChatCompletionRequest{
		Model:           ModelGrok43,
		Messages:        []Message{UserMessage("Solve this")},
		Temperature:     &temp,
		ReasoningEffort: ReasoningHigh,
	}

	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["reasoning_effort"] != string(ReasoningHigh) {
		t.Errorf("expected reasoning_effort %q, got %v", ReasoningHigh, m["reasoning_effort"])
	}
}

func TestRequestWithStructuredOutput(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	req := ChatCompletionRequest{
		Model:          ModelGrok43,
		Messages:       []Message{UserMessage("Extract name")},
		ResponseFormat: JSONSchemaResponseFormat("person", schema, true),
	}

	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	rf, ok := m["response_format"].(map[string]any)
	if !ok {
		t.Fatal("expected response_format")
	}
	if rf["type"] != string(ResponseFormatJSONSchema) {
		t.Errorf("expected type json_schema")
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatal("expected json_schema")
	}
	if js["name"] != "person" {
		t.Errorf("expected name 'person'")
	}
	if js["strict"] != true {
		t.Error("expected strict true")
	}
}

func TestJSONObjectResponseFormat(t *testing.T) {
	rf := JSONObjectResponseFormat()
	if rf.Type != ResponseFormatJSONObject {
		t.Errorf("expected type json_object")
	}
}

func TestRequestWithTools(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`)
	req := ChatCompletionRequest{
		Model:    ModelGrok43,
		Messages: []Message{UserMessage("What's the weather?")},
		Tools: []Tool{
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "get_weather",
					Description: "Get weather for a location",
					Parameters:  params,
				},
			},
		},
	}

	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatal("expected 1 tool")
	}
	tool := tools[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("expected function name 'get_weather'")
	}
}

// ============================================================================
// RequestOption 测试
// ============================================================================

func TestRequestOptions(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    ModelGrok43,
		Messages: []Message{UserMessage("test")},
	}

	WithTemperature(0.8)(&req)
	WithMaxTokens(2048)(&req)
	WithTopP(0.9)(&req)
	WithReasoningEffort(ReasoningMedium)(&req)
	WithSeed(42)(&req)
	WithN(3)(&req)
	WithFrequencyPenalty(0.1)(&req)
	WithPresencePenalty(0.2)(&req)

	if *req.Temperature != 0.8 {
		t.Error("temperature not set")
	}
	if *req.MaxTokens != 2048 {
		t.Error("max_tokens not set")
	}
	if *req.TopP != 0.9 {
		t.Error("top_p not set")
	}
	if req.ReasoningEffort != ReasoningMedium {
		t.Error("reasoning_effort not set")
	}
	if *req.Seed != 42 {
		t.Error("seed not set")
	}
	if *req.N != 3 {
		t.Error("n not set")
	}
	if *req.FrequencyPenalty != 0.1 {
		t.Error("frequency_penalty not set")
	}
	if *req.PresencePenalty != 0.2 {
		t.Error("presence_penalty not set")
	}
}

// ============================================================================
// ImageOption 测试
// ============================================================================

func TestImageOptions(t *testing.T) {
	req := ImageRequest{
		Model:  ModelGrokImageQuality,
		Prompt: "a cat",
	}

	WithImageCount(4)(&req)
	WithImageFormat(ImageFormatBase64)(&req)
	WithAspectRatio("16:9")(&req)
	WithImageResolution("2k")(&req)

	if *req.N != 4 {
		t.Error("n not set")
	}
	if req.ResponseFormat != ImageFormatBase64 {
		t.Error("response_format not set")
	}
	if req.AspectRatio != "16:9" {
		t.Error("aspect_ratio not set")
	}
	if req.Resolution != "2k" {
		t.Error("resolution not set")
	}
}

func TestImageResponseHelpers(t *testing.T) {
	resp := &ImageResponse{
		Data: []ImageData{{URL: "https://example.com/img.png"}},
	}
	if resp.FirstImageURL() != "https://example.com/img.png" {
		t.Error("unexpected URL")
	}
	if resp.FirstImageBase64() != "" {
		t.Error("expected empty base64")
	}

	resp2 := &ImageResponse{
		Data: []ImageData{{B64JSON: "base64data"}},
	}
	if resp2.FirstImageBase64() != "base64data" {
		t.Error("unexpected base64")
	}
	if resp2.FirstImageURL() != "" {
		t.Error("expected empty URL")
	}

	resp3 := &ImageResponse{Data: []ImageData{}}
	if resp3.FirstImageURL() != "" {
		t.Error("expected empty URL for empty data")
	}
}

// ============================================================================
// VideoOption 测试
// ============================================================================

func TestVideoOptions(t *testing.T) {
	req := VideoGenerationRequest{
		Model:  ModelGrokVideo,
		Prompt: "A sunset",
	}

	WithVideoDuration(10)(&req)
	WithVideoAspectRatio("9:16")(&req)
	WithVideoResolution(VideoResolution720p)(&req)
	WithVideoImage("https://example.com/start.png")(&req)

	if *req.Duration != 10 {
		t.Error("duration not set")
	}
	if req.AspectRatio != "9:16" {
		t.Error("aspect_ratio not set")
	}
	if req.Resolution != VideoResolution720p {
		t.Error("resolution not set")
	}
	if req.Image == nil || req.Image.URL != "https://example.com/start.png" {
		t.Error("image not set")
	}
}

func TestVideoStatusResponseParsing(t *testing.T) {
	data := `{
		"status": "done",
		"model": "grok-imagine-video",
		"video": {
			"url": "https://vidgen.x.ai/video.mp4",
			"duration": 8
		}
	}`
	var resp VideoStatusResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != VideoStatusDone {
		t.Errorf("expected status 'done'")
	}
	if resp.Video == nil || resp.Video.URL != "https://vidgen.x.ai/video.mp4" {
		t.Error("unexpected video URL")
	}
	if resp.Video.Duration != 8 {
		t.Error("unexpected duration")
	}
}

func TestVideoErrorParsing(t *testing.T) {
	data := `{
		"status": "failed",
		"error": {
			"code": "invalid_argument",
			"message": "Prompt cannot be empty."
		}
	}`
	var resp VideoStatusResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != VideoStatusFailed {
		t.Error("expected failed")
	}
	if resp.Error == nil || resp.Error.Code != "invalid_argument" {
		t.Error("unexpected error code")
	}
}

// ============================================================================
// TTSOption 测试
// ============================================================================

func TestTTSOptions(t *testing.T) {
	req := TTSRequest{
		Text:     "Hello",
		VoiceID:  VoiceEve,
		Language: "en",
	}

	WithTTSSpeed(1.2)(&req)
	WithTTSOutputFormat("wav", 44100, 0)(&req)
	WithTTSOptimizeStreamingLatency(1)(&req)
	WithTTSTextNormalization(true)(&req)

	if *req.Speed != 1.2 {
		t.Error("speed not set")
	}
	if req.OutputFormat == nil || req.OutputFormat.Codec != "wav" || req.OutputFormat.SampleRate != 44100 {
		t.Error("output_format not set")
	}
	if *req.OptimizeStreamingLatency != 1 {
		t.Error("optimize_streaming_latency not set")
	}
	if !*req.TextNormalization {
		t.Error("text_normalization not set")
	}
}

func TestTTSRequestJSON(t *testing.T) {
	req := TTSRequest{
		Text:     "Hello world",
		VoiceID:  VoiceAra,
		Language: "en",
	}
	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["text"] != "Hello world" {
		t.Error("unexpected text")
	}
	if m["voice_id"] != "ara" {
		t.Error("unexpected voice_id")
	}
	if m["language"] != "en" {
		t.Error("unexpected language")
	}
}

// ============================================================================
// STTOption 测试
// ============================================================================

func TestSTTOptions(t *testing.T) {
	params := STTRequest{}

	WithSTTLanguage("en")(&params)
	WithSTTFormat(true)(&params)
	WithSTTMultichannel(true)(&params)
	WithSTTChannels(2)(&params)
	WithSTTDiarize(true)(&params)
	WithSTTKeyTerms("Understand", "Universe")(&params)
	WithSTTFillerWords(true)(&params)
	WithSTTRawFormat("pcm", 16000)(&params)

	if params.Language != "en" {
		t.Error("language not set")
	}
	if !params.Format {
		t.Error("format not set")
	}
	if !params.Multichannel {
		t.Error("multichannel not set")
	}
	if params.Channels != 2 {
		t.Error("channels not set")
	}
	if !params.Diarize {
		t.Error("diarize not set")
	}
	if len(params.KeyTerms) != 2 {
		t.Error("key_terms not set")
	}
	if !params.FillerWords {
		t.Error("filler_words not set")
	}
	if params.AudioFormat != "pcm" || params.SampleRate != 16000 {
		t.Error("raw format not set")
	}
}

func TestSTTResponseParsing(t *testing.T) {
	data := `{
		"text": "The balance is $167,983.15.",
		"language": "English",
		"duration": 3.45,
		"words": [
			{"text": "The", "start": 0.24, "end": 0.48},
			{"text": "balance", "start": 0.48, "end": 0.96}
		]
	}`
	var resp STTResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Text != "The balance is $167,983.15." {
		t.Error("unexpected text")
	}
	if resp.Language != "English" {
		t.Error("unexpected language")
	}
	if resp.Duration != 3.45 {
		t.Error("unexpected duration")
	}
	if len(resp.Words) != 2 {
		t.Fatalf("expected 2 words, got %d", len(resp.Words))
	}
	if resp.Words[0].Text != "The" {
		t.Error("unexpected first word")
	}
}

// ============================================================================
// APIError 测试
// ============================================================================

func TestAPIError(t *testing.T) {
	e := &APIError{Code: "invalid_request_error", Message: "model not found"}
	if e.Error() != "invalid_request_error: model not found" {
		t.Errorf("unexpected error string: %s", e.Error())
	}

	e2 := &APIError{Message: "rate limited"}
	if e2.Error() != "rate limited" {
		t.Errorf("unexpected error string: %s", e2.Error())
	}
}

func TestErrorResponseParsing(t *testing.T) {
	data := `{
		"error": {
			"type": "invalid_request_error",
			"message": "Invalid model",
			"code": "model_not_found"
		}
	}`
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error.Code != "model_not_found" {
		t.Error("unexpected code")
	}
	if resp.Error.Message != "Invalid model" {
		t.Error("unexpected message")
	}
}

// ============================================================================
// Client 创建测试
// ============================================================================

func TestNewClient(t *testing.T) {
	client := New(WithAPIKey("test-key"))
	if client.apiKey != "test-key" {
		t.Error("API key not set")
	}
	if client.baseURL != DefaultBaseURL {
		t.Error("unexpected base URL")
	}
}

func TestNewClientWithBaseURL(t *testing.T) {
	client := New(
		WithAPIKey("test-key"),
		WithBaseURL("https://custom.example.com/"),
	)
	if client.baseURL != "https://custom.example.com/" {
		t.Errorf("expected baseURL=https://custom.example.com/, got %s", client.baseURL)
	}
}

// ============================================================================
// FileInfo 测试
// ============================================================================

func TestFileInfoParsing(t *testing.T) {
	data := `{
		"id": "file-abc123",
		"filename": "document.pdf",
		"bytes": 1048576,
		"created_at": 1710000000,
		"object": "file",
		"team_id": "team-xyz"
	}`
	var fi FileInfo
	if err := json.Unmarshal([]byte(data), &fi); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fi.ID != "file-abc123" {
		t.Error("unexpected id")
	}
	if fi.Filename != "document.pdf" {
		t.Error("unexpected filename")
	}
	if fi.Bytes != 1048576 {
		t.Error("unexpected bytes")
	}
	if fi.TeamID != "team-xyz" {
		t.Error("unexpected team_id")
	}
}

func TestDeleteFileResponseParsing(t *testing.T) {
	data := `{
		"id": "file-abc123",
		"object": "file",
		"deleted": true
	}`
	var resp DeleteFileResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Deleted {
		t.Error("expected deleted true")
	}
}

// ============================================================================
// StreamAccumulator 测试
// ============================================================================

func TestStreamAccumulator(t *testing.T) {
	acc := NewStreamAccumulator()

	// 模拟流式 chunk
	chunks := []ChatCompletionResponse{
		{
			ID: "chatcmpl-1", Object: "chat.completion", Created: 1710000000, Model: ModelGrok43,
			Choices: []Choice{
				{Index: 0, Delta: Delta{Role: RoleAssistant, Content: "Hello"}},
			},
		},
		{
			ID: "chatcmpl-1", Model: ModelGrok43,
			Choices: []Choice{
				{Index: 0, Delta: Delta{Content: " world"}},
			},
		},
		{
			ID: "chatcmpl-1", Model: ModelGrok43,
			Choices: []Choice{
				{Index: 0, Delta: Delta{Content: "!"}, FinishReason: FinishReasonStop},
			},
			Usage: &Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}

	for _, chunk := range chunks {
		if err := acc.OnChunk(chunk); err != nil {
			t.Fatalf("OnChunk: %v", err)
		}
	}

	result := acc.Result()
	if result.ID != "chatcmpl-1" {
		t.Errorf("unexpected ID: %s", result.ID)
	}
	if result.Model != ModelGrok43 {
		t.Errorf("unexpected model")
	}
	if len(result.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(result.Choices))
	}
	if result.Choices[0].FinishReason != FinishReasonStop {
		t.Error("expected stop")
	}

	var content string
	_ = json.Unmarshal(result.Choices[0].Message.Content, &content)
	if content != "Hello world!" {
		t.Errorf("expected 'Hello world!', got %q", content)
	}

	if result.Usage == nil || result.Usage.TotalTokens != 15 {
		t.Error("unexpected usage")
	}
}

func TestStreamAccumulatorWithReasoning(t *testing.T) {
	acc := NewStreamAccumulator()

	chunks := []ChatCompletionResponse{
		{
			ID: "chatcmpl-2", Model: ModelGrok43,
			Choices: []Choice{
				{Index: 0, Delta: Delta{Role: RoleAssistant, ReasoningContent: "Let me think..."}},
			},
		},
		{
			ID: "chatcmpl-2", Model: ModelGrok43,
			Choices: []Choice{
				{Index: 0, Delta: Delta{Content: "42", ReasoningContent: " The answer is 42."}, FinishReason: FinishReasonStop},
			},
		},
	}

	for _, chunk := range chunks {
		_ = acc.OnChunk(chunk)
	}

	result := acc.Result()
	if result.Choices[0].Message.ReasoningContent != "Let me think... The answer is 42." {
		t.Errorf("unexpected reasoning content: %q", result.Choices[0].Message.ReasoningContent)
	}

	var content string
	_ = json.Unmarshal(result.Choices[0].Message.Content, &content)
	if content != "42" {
		t.Errorf("expected '42', got %q", content)
	}
}

// ============================================================================
// 常量验证
// ============================================================================

func TestReasoningEffortConstants(t *testing.T) {
	tests := []struct {
		val      ReasoningEffort
		expected string
	}{
		{ReasoningNone, "none"},
		{ReasoningLow, "low"},
		{ReasoningMedium, "medium"},
		{ReasoningHigh, "high"},
	}
	for _, tt := range tests {
		if string(tt.val) != tt.expected {
			t.Errorf("expected %q, got %q", tt.expected, string(tt.val))
		}
	}
}

func TestModelConstants(t *testing.T) {
	if ModelGrok43 != "grok-4.3" {
		t.Error("unexpected model constant")
	}
	if ModelGrokImageQuality != "grok-imagine-image-quality" {
		t.Error("unexpected image model constant")
	}
	if ModelGrokVideo != "grok-imagine-video" {
		t.Error("unexpected video model constant")
	}
}

func TestVideoStatusConstants(t *testing.T) {
	if VideoStatusPending != "pending" {
		t.Error("unexpected pending")
	}
	if VideoStatusDone != "done" {
		t.Error("unexpected done")
	}
	if VideoStatusExpired != "expired" {
		t.Error("unexpected expired")
	}
	if VideoStatusFailed != "failed" {
		t.Error("unexpected failed")
	}
}

// ============================================================================
// ListVoicesResponse 测试
// ============================================================================

func TestListVoicesResponseParsing(t *testing.T) {
	data := `{
		"voices": [
			{"voice_id": "eve", "name": "Eve"},
			{"voice_id": "ara", "name": "Ara"},
			{"voice_id": "rex", "name": "Rex"}
		]
	}`
	var resp ListVoicesResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Voices) != 3 {
		t.Fatalf("expected 3 voices, got %d", len(resp.Voices))
	}
	if resp.Voices[0].VoiceID != "eve" {
		t.Error("unexpected voice_id")
	}
}

// ============================================================================
// ListFilesResponse 测试
// ============================================================================

func TestListFilesResponseParsing(t *testing.T) {
	data := `{
		"data": [
			{"id": "file-1", "filename": "a.txt", "bytes": 100, "created_at": 1710000000, "object": "file"},
			{"id": "file-2", "filename": "b.pdf", "bytes": 200, "created_at": 1710000001, "object": "file"}
		],
		"object": "list",
		"pagination_token": "next-page"
	}`
	var resp ListFilesResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 files, got %d", len(resp.Data))
	}
	if resp.PaginationToken != "next-page" {
		t.Error("unexpected pagination_token")
	}
}

// ============================================================================
// intStr 测试
// ============================================================================

func TestIntStr(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{-1, "-1"},
		{42, "42"},
		{8000, "8000"},
		{16000, "16000"},
	}
	for _, tt := range tests {
		if got := intStr(tt.input); got != tt.expected {
			t.Errorf("intStr(%d) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// ============================================================================
// ChatCompletionResponse 解析测试
// ============================================================================

func TestChatCompletionResponseParsing(t *testing.T) {
	data := `{
		"id": "chatcmpl-abc123",
		"object": "chat.completion",
		"created": 1710000000,
		"model": "grok-4.3",
		"choices": [
			{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "The universe is vast."
				},
				"finish_reason": "stop"
			}
		],
		"usage": {
			"prompt_tokens": 15,
			"completion_tokens": 10,
			"total_tokens": 25
		}
	}`

	var resp ChatCompletionResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ID != "chatcmpl-abc123" {
		t.Error("unexpected id")
	}
	if resp.Object != "chat.completion" {
		t.Error("unexpected object")
	}
	if resp.Model != ModelGrok43 {
		t.Error("unexpected model")
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice")
	}
	if resp.Choices[0].FinishReason != FinishReasonStop {
		t.Error("unexpected finish_reason")
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 25 {
		t.Error("unexpected usage")
	}

	var content string
	_ = json.Unmarshal(resp.Choices[0].Message.Content, &content)
	if content != "The universe is vast." {
		t.Errorf("unexpected content: %q", content)
	}
}
