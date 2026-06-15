package openai

import (
	"encoding/json"
	"testing"

	"github.com/kasuganosora/thinkbot/util/log"
)

func TestMain(m *testing.M) {
	_ = log.Init()
	m.Run()
}

// ============================================================================
// 输入构造辅助函数测试
// ============================================================================

func TestInputSystem(t *testing.T) {
	item := InputSystem("You are helpful.")
	if item.Type != TypeMessage {
		t.Errorf("expected type %q", TypeMessage)
	}
	if item.Role != RoleSystem {
		t.Errorf("expected role %q", RoleSystem)
	}
	var s string
	if err := json.Unmarshal(item.Content, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s != "You are helpful." {
		t.Errorf("unexpected content: %q", s)
	}
}

func TestInputUser(t *testing.T) {
	item := InputUser("Hello")
	if item.Role != RoleUser {
		t.Errorf("expected role %q", RoleUser)
	}
}

func TestInputAssistant(t *testing.T) {
	item := InputAssistant("Hi there")
	if item.Role != RoleAssistant {
		t.Errorf("expected role %q", RoleAssistant)
	}
}

func TestInputDeveloper(t *testing.T) {
	item := InputDeveloper("Be concise")
	if item.Role != RoleDeveloper {
		t.Errorf("expected role %q", RoleDeveloper)
	}
}

func TestInputUserWithImage(t *testing.T) {
	item := InputUserWithImage("What's this?", "https://example.com/img.png")
	if item.Role != RoleUser {
		t.Errorf("expected role %q", RoleUser)
	}
	var parts []ContentPart
	if err := json.Unmarshal(item.Content, &parts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].Type != ContentTypeInputImage {
		t.Errorf("expected first part type %q", ContentTypeInputImage)
	}
	if parts[0].ImageURL != "https://example.com/img.png" {
		t.Error("unexpected image URL")
	}
	if parts[1].Type != ContentTypeInputText {
		t.Errorf("expected second part type %q", ContentTypeInputText)
	}
}

func TestInputFunctionCallOutput(t *testing.T) {
	item := InputFunctionCallOutput("call_123", `{"temp": 20}`)
	if item.Type != TypeFunctionCallOutput {
		t.Errorf("expected type %q", TypeFunctionCallOutput)
	}
	if item.CallID != "call_123" {
		t.Errorf("unexpected call_id: %q", item.CallID)
	}
	if item.Output != `{"temp": 20}` {
		t.Errorf("unexpected output: %q", item.Output)
	}
}

func TestInputString(t *testing.T) {
	raw := InputString("hello")
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s != "hello" {
		t.Errorf("unexpected: %q", s)
	}
}

func TestInputItems(t *testing.T) {
	items := []InputItem{
		InputUser("Hello"),
		InputAssistant("Hi"),
	}
	raw := InputItems(items)
	var arr []InputItem
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 items, got %d", len(arr))
	}
}

// ============================================================================
// CreateResponseRequest 序列化测试
// ============================================================================

func TestCreateResponseRequestJSON(t *testing.T) {
	temp := 0.7
	maxTok := 4096
	req := CreateResponseRequest{
		Model:           ModelGPT4o,
		Input:           InputString("Hello"),
		Temperature:     &temp,
		MaxOutputTokens: &maxTok,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["model"] != ModelGPT4o {
		t.Errorf("unexpected model")
	}
	if m["stream"] == true {
		t.Error("stream should be absent or false")
	}
	if m["temperature"] != 0.7 {
		t.Errorf("unexpected temperature: %v", m["temperature"])
	}
	if m["max_output_tokens"] != float64(4096) {
		t.Errorf("unexpected max_output_tokens")
	}
}

func TestRequestWithReasoning(t *testing.T) {
	req := CreateResponseRequest{
		Model:     ModelO3,
		Input:     InputString("Solve this"),
		Reasoning: &ReasoningConfig{Effort: ReasoningHigh, Summary: "auto"},
	}

	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	r, ok := m["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("expected reasoning")
	}
	if r["effort"] != "high" {
		t.Errorf("unexpected effort: %v", r["effort"])
	}
	if r["summary"] != "auto" {
		t.Errorf("unexpected summary: %v", r["summary"])
	}
}

func TestRequestWithJSONSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	req := CreateResponseRequest{
		Model: ModelGPT4o,
		Input: InputString("Extract name"),
		Text: &TextConfig{
			Format: &TextFormatConfig{
				Type:   "json_schema",
				Name:   "person",
				Schema: schema,
				Strict: boolPtr(true),
			},
		},
	}

	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	txt, ok := m["text"].(map[string]any)
	if !ok {
		t.Fatal("expected text")
	}
	fmt, ok := txt["format"].(map[string]any)
	if !ok {
		t.Fatal("expected format")
	}
	if fmt["type"] != "json_schema" {
		t.Errorf("unexpected type: %v", fmt["type"])
	}
	if fmt["name"] != "person" {
		t.Errorf("unexpected name: %v", fmt["name"])
	}
}

func TestRequestWithFunctionTools(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`)
	req := CreateResponseRequest{
		Model: ModelGPT4o,
		Input: InputString("Weather?"),
	}
	WithFunctionTools(FunctionTool{
		Type:       "function",
		Name:       "get_weather",
		Parameters: params,
	})(&req)

	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %v", m["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("unexpected name: %v", tool["name"])
	}
}

func TestRequestWithPreviousResponse(t *testing.T) {
	req := CreateResponseRequest{
		Model:              ModelGPT4o,
		Input:              InputString("Continue"),
		PreviousResponseID: "resp_abc123",
	}
	store := true
	WithStore(store)(&req)

	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["previous_response_id"] != "resp_abc123" {
		t.Errorf("unexpected previous_response_id")
	}
	s, ok := m["store"].(bool)
	if !ok || !s {
		t.Error("expected store true")
	}
}

// ============================================================================
// RequestOption 测试
// ============================================================================

func TestRequestOptions(t *testing.T) {
	req := CreateResponseRequest{
		Model: ModelGPT4o,
		Input: InputString("test"),
	}

	WithInstructions("Be helpful")(&req)
	WithTemperature(0.8)(&req)
	WithTopP(0.9)(&req)
	WithMaxOutputTokens(2048)(&req)
	WithReasoningEffort(ReasoningMedium)(&req)
	WithUser("user-123")(&req)
	WithParallelToolCalls(true)(&req)

	if req.Instructions != "Be helpful" {
		t.Error("instructions not set")
	}
	if *req.Temperature != 0.8 {
		t.Error("temperature not set")
	}
	if *req.TopP != 0.9 {
		t.Error("top_p not set")
	}
	if *req.MaxOutputTokens != 2048 {
		t.Error("max_output_tokens not set")
	}
	if req.Reasoning == nil || req.Reasoning.Effort != ReasoningMedium {
		t.Error("reasoning_effort not set")
	}
	if req.User != "user-123" {
		t.Error("user not set")
	}
	if *req.ParallelToolCalls != true {
		t.Error("parallel_tool_calls not set")
	}
}

func TestWithJSONSchemaOption(t *testing.T) {
	req := CreateResponseRequest{
		Model: ModelGPT4o,
		Input: InputString("test"),
	}
	schema := json.RawMessage(`{"type":"object"}`)
	WithJSONSchema("my_schema", schema, true)(&req)

	if req.Text == nil || req.Text.Format == nil {
		t.Fatal("text.format not set")
	}
	if req.Text.Format.Type != "json_schema" {
		t.Error("unexpected format type")
	}
	if req.Text.Format.Name != "my_schema" {
		t.Error("unexpected name")
	}
	if *req.Text.Format.Strict != true {
		t.Error("unexpected strict")
	}
}

func TestWithWebSearchOption(t *testing.T) {
	req := CreateResponseRequest{
		Model: ModelGPT4o,
		Input: InputString("What's the news?"),
	}
	WithWebSearch("medium")(&req)

	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
	var tool map[string]any
	_ = json.Unmarshal(req.Tools[0], &tool)
	if tool["type"] != "web_search" {
		t.Errorf("unexpected tool type: %v", tool["type"])
	}
}

func TestWithFileSearchOption(t *testing.T) {
	req := CreateResponseRequest{
		Model: ModelGPT4o,
		Input: InputString("Search docs"),
	}
	WithFileSearch([]string{"vs_abc"}, 5)(&req)

	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
	var tool map[string]any
	_ = json.Unmarshal(req.Tools[0], &tool)
	if tool["type"] != "file_search" {
		t.Errorf("unexpected tool type: %v", tool["type"])
	}
}

func TestWithCodeInterpreterOption(t *testing.T) {
	req := CreateResponseRequest{
		Model: ModelGPT4o,
		Input: InputString("Run code"),
	}
	WithCodeInterpreter("file_1", "file_2")(&req)

	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
	var tool map[string]any
	_ = json.Unmarshal(req.Tools[0], &tool)
	if tool["type"] != "code_interpreter" {
		t.Errorf("unexpected tool type: %v", tool["type"])
	}
}

// ============================================================================
// SpeechOption 测试
// ============================================================================

func TestSpeechOptions(t *testing.T) {
	req := SpeechRequest{
		Model: ModelGPT4oMiniTTS,
		Voice: VoiceAlloy,
		Input: "Hello world",
	}

	WithSpeechFormat(AudioFormatWAV)(&req)
	WithSpeechSpeed(1.5)(&req)
	WithSpeechInstructions("Speak slowly")(&req)
	WithSpeechStreamFormat("sse")(&req)

	if req.ResponseFormat != AudioFormatWAV {
		t.Error("format not set")
	}
	if *req.Speed != 1.5 {
		t.Error("speed not set")
	}
	if req.Instructions != "Speak slowly" {
		t.Error("instructions not set")
	}
	if req.StreamFormat != "sse" {
		t.Error("stream_format not set")
	}
}

func TestSpeechRequestJSON(t *testing.T) {
	req := SpeechRequest{
		Model: ModelGPT4oMiniTTS,
		Voice: VoiceNova,
		Input: "Hello",
	}
	data, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["model"] != ModelGPT4oMiniTTS {
		t.Error("unexpected model")
	}
	if m["voice"] != "nova" {
		t.Error("unexpected voice")
	}
	if m["input"] != "Hello" {
		t.Error("unexpected input")
	}
}

// ============================================================================
// TranslationOption 测试
// ============================================================================

func TestTranslationOptions(t *testing.T) {
	params := TranslationRequest{
		Model:    ModelWhisper1,
		File:     []byte("fake"),
		Filename: "audio.mp3",
	}

	WithTranslationPrompt("This is a conversation")(&params)
	WithTranslationFormat("verbose_json")(&params)
	temp := 0.5
	WithTranslationTemperature(temp)(&params)

	if params.Prompt != "This is a conversation" {
		t.Error("prompt not set")
	}
	if params.ResponseFormat != "verbose_json" {
		t.Error("format not set")
	}
	if params.Temperature == nil || *params.Temperature != 0.5 {
		t.Error("temperature not set")
	}
}

// ============================================================================
// Response 解析测试
// ============================================================================

func TestResponseParsing(t *testing.T) {
	data := `{
		"id": "resp_abc123",
		"object": "response",
		"created_at": 1710000000,
		"status": "completed",
		"model": "gpt-4o",
		"output": [
			{
				"type": "message",
				"id": "msg_001",
				"role": "assistant",
				"status": "completed",
				"content": [
					{"type": "output_text", "text": "Hello! How can I help?"}
				]
			}
		],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 8,
			"total_tokens": 18
		}
	}`

	var resp Response
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ID != "resp_abc123" {
		t.Error("unexpected id")
	}
	if resp.Status != StatusCompleted {
		t.Error("unexpected status")
	}
	if resp.Model != ModelGPT4o {
		t.Error("unexpected model")
	}
	if len(resp.Output) != 1 {
		t.Fatalf("expected 1 output, got %d", len(resp.Output))
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 18 {
		t.Error("unexpected usage")
	}
}

func TestResponseOutputText(t *testing.T) {
	resp := &Response{
		Output: []OutputItem{
			{
				Type: TypeMessage,
				Role: RoleAssistant,
				Content: json.RawMessage(`[
					{"type":"output_text","text":"Hello "},
					{"type":"output_text","text":"world!"}
				]`),
			},
		},
	}
	if text := resp.OutputText(); text != "Hello world!" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestResponseFirstMessageText(t *testing.T) {
	resp := &Response{
		Output: []OutputItem{
			{
				Type:    TypeMessage,
				Role:    RoleAssistant,
				Content: json.RawMessage(`[{"type":"output_text","text":"The answer is 42."}]`),
			},
		},
	}
	if text := resp.FirstMessageText(); text != "The answer is 42." {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestResponseFunctionCalls(t *testing.T) {
	resp := &Response{
		Output: []OutputItem{
			{
				Type:      TypeFunctionCall,
				ID:        "fc_001",
				CallID:    "call_abc",
				Name:      "get_weather",
				Arguments: `{"location":"NYC"}`,
				Status:    "completed",
			},
		},
	}
	calls := resp.FunctionCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Error("unexpected name")
	}
	if calls[0].CallID != "call_abc" {
		t.Error("unexpected call_id")
	}
}

// ============================================================================
// 流式事件解析测试
// ============================================================================

func TestStreamEventParsing(t *testing.T) {
	data := `{
		"type": "response.output_text.delta",
		"item_id": "msg_001",
		"output_index": 0,
		"content_index": 0,
		"delta": "Hello"
	}`

	var event StreamEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventResponseOutputTextDelta {
		t.Errorf("unexpected type: %s", event.Type)
	}
	if event.Delta != "Hello" {
		t.Error("unexpected delta")
	}
	if event.OutputIndex != 0 {
		t.Error("unexpected output_index")
	}
}

func TestStreamEventResponseCreated(t *testing.T) {
	data := `{
		"type": "response.created",
		"response": {
			"id": "resp_abc",
			"object": "response",
			"status": "in_progress",
			"model": "gpt-4o"
		}
	}`

	var event StreamEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Response == nil {
		t.Fatal("expected response")
	}
	if event.Response.ID != "resp_abc" {
		t.Error("unexpected id")
	}
}

// ============================================================================
// StreamAccumulator 测试
// ============================================================================

func TestStreamAccumulator(t *testing.T) {
	acc := NewStreamAccumulator()

	events := []StreamEvent{
		{
			Type: EventResponseCreated,
			Response: &Response{
				ID: "resp_001", Model: ModelGPT4o, Status: StatusInProgress,
			},
		},
		{
			Type:  EventResponseOutputTextDelta,
			Delta: "Hello",
		},
		{
			Type:  EventResponseOutputTextDelta,
			Delta: " world",
		},
		{
			Type:  EventResponseOutputTextDelta,
			Delta: "!",
		},
		{
			Type: EventResponseCompleted,
			Response: &Response{
				ID: "resp_001", Status: StatusCompleted,
				Usage: &ResponseUsage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
			},
		},
	}

	for _, event := range events {
		if err := acc.OnEvent(event); err != nil {
			t.Fatalf("OnEvent: %v", err)
		}
	}

	if text := acc.Text(); text != "Hello world!" {
		t.Errorf("unexpected text: %q", text)
	}

	result := acc.Result()
	if result.ID != "resp_001" {
		t.Errorf("unexpected id: %s", result.ID)
	}
	if result.Status != StatusCompleted {
		t.Error("unexpected status")
	}
	if result.Usage == nil || result.Usage.TotalTokens != 8 {
		t.Error("unexpected usage")
	}
}

func TestStreamAccumulatorWithFunctionCall(t *testing.T) {
	acc := NewStreamAccumulator()

	events := []StreamEvent{
		{
			Type:        EventResponseOutputItemAdded,
			OutputIndex: 0,
			Item: &OutputItem{
				Type:      TypeFunctionCall,
				ID:        "fc_001",
				CallID:    "call_abc",
				Name:      "get_weather",
				Arguments: `{"location":"NYC"}`,
			},
		},
	}

	for _, event := range events {
		_ = acc.OnEvent(event)
	}

	calls := acc.FunctionCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 function call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Error("unexpected name")
	}
}

// ============================================================================
// Models 解析测试
// ============================================================================

func TestListModelsResponseParsing(t *testing.T) {
	data := `{
		"object": "list",
		"data": [
			{"id": "gpt-4o", "object": "model", "created": 1686935002, "owned_by": "openai"},
			{"id": "o3", "object": "model", "created": 1710000000, "owned_by": "openai"}
		]
	}`

	var resp ListModelsResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Data))
	}
	if resp.Data[0].ID != "gpt-4o" {
		t.Error("unexpected id")
	}

	m := resp.FindModel("o3")
	if m == nil {
		t.Fatal("expected to find o3")
	}
	if m.OwnedBy != "openai" {
		t.Error("unexpected owned_by")
	}
}

// ============================================================================
// TranslationResponse 解析测试
// ============================================================================

func TestTranslationResponseParsing(t *testing.T) {
	data := `{
		"text": "Hello, my name is Wolfgang.",
		"duration": 3.45,
		"language": "english"
	}`

	var resp TranslationResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Text != "Hello, my name is Wolfgang." {
		t.Error("unexpected text")
	}
	if resp.Duration != 3.45 {
		t.Error("unexpected duration")
	}
}

func TestTranslationResponseVerbose(t *testing.T) {
	data := `{
		"text": "Hello world.",
		"duration": 2.0,
		"language": "english",
		"segments": [
			{"id": 0, "text": "Hello world.", "start": 0.0, "end": 2.0}
		]
	}`

	var resp TranslationResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Segments) != 1 {
		t.Fatalf("expected 1 segment")
	}
	if resp.Segments[0].Text != "Hello world." {
		t.Error("unexpected segment text")
	}
}

// ============================================================================
// Voice 解析测试
// ============================================================================

func TestVoiceParsing(t *testing.T) {
	data := `{
		"id": "voice_abc123",
		"name": "My Voice",
		"object": "audio.voice",
		"created_at": 1710000000
	}`

	var v Voice
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.ID != "voice_abc123" {
		t.Error("unexpected id")
	}
	if v.Name != "My Voice" {
		t.Error("unexpected name")
	}
	if v.Object != "audio.voice" {
		t.Error("unexpected object")
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

func TestNewClientWithOrg(t *testing.T) {
	client := New(
		WithAPIKey("test-key"),
		WithOrganization("org-123"),
		WithProject("proj-456"),
	)
	if client.apiKey != "test-key" {
		t.Error("API key not set")
	}
}

// ============================================================================
// 辅助函数测试
// ============================================================================

func TestGuessAudioMIME(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"audio.mp3", "audio/mpeg"},
		{"audio.wav", "audio/wav"},
		{"audio.ogg", "audio/ogg"},
		{"audio.aac", "audio/aac"},
		{"audio.flac", "audio/flac"},
		{"audio.webm", "audio/webm"},
		{"audio.m4a", "audio/mp4"},
		{"unknown.xyz", ""},
	}
	for _, tt := range tests {
		if got := guessAudioMIME(tt.filename); got != tt.expected {
			t.Errorf("guessAudioMIME(%q) = %q, want %q", tt.filename, got, tt.expected)
		}
	}
}

func TestQuoteJSONString(t *testing.T) {
	s := quoteJSONString("hello\nworld")
	var result string
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result != "hello\nworld" {
		t.Errorf("unexpected: %q", result)
	}
}

// ============================================================================
// 常量验证测试
// ============================================================================

func TestModelConstants(t *testing.T) {
	if ModelGPT4o != "gpt-4o" {
		t.Error("unexpected model")
	}
	if ModelO3 != "o3" {
		t.Error("unexpected model")
	}
	if ModelGPT4oMiniTTS != "gpt-4o-mini-tts" {
		t.Error("unexpected tts model")
	}
}

func TestVoiceConstants(t *testing.T) {
	if VoiceAlloy != "alloy" {
		t.Error("unexpected voice")
	}
	if VoiceNova != "nova" {
		t.Error("unexpected voice")
	}
}

func TestAudioFormatConstants(t *testing.T) {
	if AudioFormatMP3 != "mp3" {
		t.Error("unexpected format")
	}
	if AudioFormatOpus != "opus" {
		t.Error("unexpected format")
	}
}

func TestReasoningConstants(t *testing.T) {
	if ReasoningHigh != "high" {
		t.Error("unexpected reasoning")
	}
	if ReasoningLow != "low" {
		t.Error("unexpected reasoning")
	}
}

func TestStatusConstants(t *testing.T) {
	if StatusCompleted != "completed" {
		t.Error("unexpected status")
	}
	if StatusFailed != "failed" {
		t.Error("unexpected status")
	}
}

func TestEventConstants(t *testing.T) {
	if EventResponseCreated != "response.created" {
		t.Error("unexpected event")
	}
	if EventResponseOutputTextDelta != "response.output_text.delta" {
		t.Error("unexpected event")
	}
	if EventResponseCompleted != "response.completed" {
		t.Error("unexpected event")
	}
}

func TestTypeConstants(t *testing.T) {
	if TypeMessage != "message" {
		t.Error("unexpected type")
	}
	if TypeFunctionCall != "function_call" {
		t.Error("unexpected type")
	}
}

// boolPtr 返回 bool 指针。
func boolPtr(b bool) *bool {
	return &b
}
