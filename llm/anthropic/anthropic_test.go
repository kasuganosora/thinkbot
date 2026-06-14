package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
)

func TestMain(m *testing.M) {
	// 初始化全局 Logger（避免 nil pointer）
	_ = log.InitWithConfig(log.Config{Level: "error"}) // 测试时只输出 error
	os.Exit(m.Run())
}

// ============================================================================
// 辅助：模拟 Anthropic API 服务器
// ============================================================================

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func checkAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if key := r.Header.Get("x-api-key"); key != "test-key" {
		t.Errorf("expected x-api-key=test-key, got %q", key)
	}
	if v := r.Header.Get("anthropic-version"); v != APIVersion {
		t.Errorf("expected anthropic-version=%s, got %q", APIVersion, v)
	}
}

// ============================================================================
// Test CreateMessage
// ============================================================================

func TestCreateMessage(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var req MessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if req.Model != "claude-sonnet-4-20250514" {
			t.Errorf("expected model claude-sonnet-4-20250514, got %s", req.Model)
		}
		if req.MaxTokens != 1024 {
			t.Errorf("expected max_tokens=1024, got %d", req.MaxTokens)
		}
		if req.Stream {
			t.Error("expected stream=false")
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Error("expected 1 user message")
		}

		resp := MessageResponse{
			ID:         "msg_123",
			Type:       "message",
			Role:       "assistant",
			Content:    []ContentBlock{{Type: ContentTypeText, Text: "Hello!"}},
			Model:      req.Model,
			StopReason: StopReasonEndTurn,
			Usage:      Usage{InputTokens: 10, OutputTokens: 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi")},
		},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
	if resp.ID != "msg_123" {
		t.Errorf("expected id=msg_123, got %s", resp.ID)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello!" {
		t.Errorf("unexpected content: %+v", resp.Content)
	}
	if resp.StopReason != StopReasonEndTurn {
		t.Errorf("expected stop_reason=end_turn, got %s", resp.StopReason)
	}
}

// ============================================================================
// Test API Error
// ============================================================================

func TestAPIError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ErrorResponse{
			Type: "error",
			Error: APIError{
				Type:    "invalid_request_error",
				Message: "model not found",
			},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "bad-model",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Type != "invalid_request_error" {
		t.Errorf("expected type=invalid_request_error, got %s", apiErr.Type)
	}
	if apiErr.Message != "model not found" {
		t.Errorf("expected message='model not found', got %s", apiErr.Message)
	}
}

// ============================================================================
// Test CountTokens
// ============================================================================

func TestCountTokens(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CountTokensResponse{InputTokens: 42})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CountTokens(context.Background(), CountTokensRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []Message{
			{Role: "user", Content: TextContent("Hello world")},
		},
	})
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if resp.InputTokens != 42 {
		t.Errorf("expected input_tokens=42, got %d", resp.InputTokens)
	}
}

// ============================================================================
// Test CountTokens (Extended: thinking, tools, images, PDFs)
// ============================================================================

func TestCountTokensWithSystem(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req CountTokensRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "claude-opus-4-8" {
			t.Errorf("expected model=claude-opus-4-8, got %s", req.Model)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(req.Messages))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CountTokensResponse{InputTokens: 14})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CountTokens(context.Background(), CountTokensRequest{
		Model:  "claude-opus-4-8",
		System: SystemText("You are a scientist"),
		Messages: []Message{
			{Role: "user", Content: TextContent("Hello, Claude")},
		},
	})
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if resp.InputTokens != 14 {
		t.Errorf("expected input_tokens=14, got %d", resp.InputTokens)
	}
}

func TestCountTokensWithTools(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CountTokensRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(req.Tools))
		}
		if req.Tools[0].Name != "get_weather" {
			t.Errorf("expected tool name=get_weather, got %s", req.Tools[0].Name)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CountTokensResponse{InputTokens: 403})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CountTokens(context.Background(), CountTokensRequest{
		Model: "claude-opus-4-8",
		Tools: []Tool{
			{
				Name:        "get_weather",
				Description: "Get the current weather in a given location",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{
							"type":        "string",
							"description": "The city and state, e.g. San Francisco, CA",
						},
					},
					"required": []string{"location"},
				},
			},
		},
		Messages: []Message{
			{Role: "user", Content: TextContent("What's the weather like in San Francisco?")},
		},
	})
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if resp.InputTokens != 403 {
		t.Errorf("expected input_tokens=403, got %d", resp.InputTokens)
	}
}

func TestCountTokensWithImage(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CountTokensRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		msg := req.Messages[0]
		if len(msg.Content) != 2 {
			t.Fatalf("expected 2 blocks, got %d", len(msg.Content))
		}
		if msg.Content[0].Type != ContentTypeImage {
			t.Errorf("expected first block=image, got %s", msg.Content[0].Type)
		}
		if msg.Content[0].Source == nil || msg.Content[0].Source.MediaType != ImageJPEG {
			t.Errorf("expected image/jpeg source")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CountTokensResponse{InputTokens: 1551})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CountTokens(context.Background(), CountTokensRequest{
		Model: "claude-opus-4-8",
		Messages: []Message{
			{
				Role: "user",
				Content: ImageWithText(
					Base64ImageBlock(ImageJPEG, "base64data..."),
					"Describe this image",
				),
			},
		},
	})
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if resp.InputTokens != 1551 {
		t.Errorf("expected input_tokens=1551, got %d", resp.InputTokens)
	}
}

func TestCountTokensWithPDF(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CountTokensRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		msg := req.Messages[0]
		if len(msg.Content) != 2 {
			t.Fatalf("expected 2 blocks, got %d", len(msg.Content))
		}
		if msg.Content[0].Type != ContentTypeDocument {
			t.Errorf("expected document type, got %s", msg.Content[0].Type)
		}
		if msg.Content[0].Source == nil || msg.Content[0].Source.MediaType != MimeTypePDF {
			t.Errorf("expected application/pdf source")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CountTokensResponse{InputTokens: 2188})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CountTokens(context.Background(), CountTokensRequest{
		Model: "claude-opus-4-8",
		Messages: []Message{
			{
				Role:    "user",
				Content: DocumentWithText("pdfbase64data...", "Please summarize this document."),
			},
		},
	})
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if resp.InputTokens != 2188 {
		t.Errorf("expected input_tokens=2188, got %d", resp.InputTokens)
	}
}

func TestCountTokensWithThinking(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CountTokensRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Thinking == nil {
			t.Fatal("expected non-nil Thinking")
		}
		if req.Thinking.Type != "enabled" {
			t.Errorf("expected thinking.type=enabled, got %s", req.Thinking.Type)
		}
		if req.Thinking.BudgetTokens != 16000 {
			t.Errorf("expected budget_tokens=16000, got %d", req.Thinking.BudgetTokens)
		}
		// 应该有 3 条消息
		if len(req.Messages) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(req.Messages))
		}
		// 第二条 assistant 消息应包含 thinking + text
		assistantMsg := req.Messages[1]
		if len(assistantMsg.Content) != 2 {
			t.Fatalf("expected 2 blocks in assistant message, got %d", len(assistantMsg.Content))
		}
		if assistantMsg.Content[0].Type != ContentTypeThinking {
			t.Errorf("expected thinking block, got %s", assistantMsg.Content[0].Type)
		}
		if assistantMsg.Content[0].Signature == "" {
			t.Error("expected non-empty signature")
		}
		if assistantMsg.Content[1].Type != ContentTypeText {
			t.Errorf("expected text block, got %s", assistantMsg.Content[1].Type)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CountTokensResponse{InputTokens: 88})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CountTokens(context.Background(), CountTokensRequest{
		Model:    "claude-sonnet-4-6",
		Thinking: ThinkingEnabled(16000),
		Messages: []Message{
			{Role: "user", Content: TextContent("Are there an infinite number of prime numbers such that n mod 4 == 3?")},
			{
				Role: "assistant",
				Content: MessageContent{
					ThinkingBlock(
						"This is a nice number theory question. Let's think about it step by step...",
						"EuYBCkQYAiJAgCs1le6/Pol5Z4/JMomVOouGrWdhYNsH3ukzUECbB6iWrSQtsQuRHJID6lWV...",
					),
					{Type: ContentTypeText, Text: "Yes, there are infinitely many prime numbers p such that p mod 4 = 3..."},
				},
			},
			{Role: "user", Content: TextContent("Can you write a formal proof?")},
		},
	})
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if resp.InputTokens != 88 {
		t.Errorf("expected input_tokens=88, got %d", resp.InputTokens)
	}
}

func TestThinkingBlockSerialization(t *testing.T) {
	block := ThinkingBlock("step by step...", "sig_abc123")
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"thinking"`) {
		t.Errorf("expected type=thinking, got %s", s)
	}
	if !strings.Contains(s, `"thinking":"step by step..."`) {
		t.Errorf("expected thinking field, got %s", s)
	}
	if !strings.Contains(s, `"signature":"sig_abc123"`) {
		t.Errorf("expected signature field, got %s", s)
	}
}

func TestThinkingConfigSerialization(t *testing.T) {
	tc := ThinkingEnabled(10000)
	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"enabled"`) {
		t.Errorf("expected type=enabled, got %s", s)
	}
	if !strings.Contains(s, `"budget_tokens":10000`) {
		t.Errorf("expected budget_tokens=10000, got %s", s)
	}
}

func TestDocumentBlockSerialization(t *testing.T) {
	block := Base64DocumentBlock("pdfbase64data")
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"document"`) {
		t.Errorf("expected type=document, got %s", s)
	}
	if !strings.Contains(s, `"media_type":"application/pdf"`) {
		t.Errorf("expected media_type=application/pdf, got %s", s)
	}
	if !strings.Contains(s, `"data":"pdfbase64data"`) {
		t.Errorf("expected data field, got %s", s)
	}
}

// ============================================================================
// Test ListModels
// ============================================================================

func TestListModels(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if limit := r.URL.Query().Get("limit"); limit != "10" {
			t.Errorf("expected limit=10, got %s", limit)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ListModelsResponse{
			Data: []Model{
				{Type: "model", ID: "claude-sonnet-4-20250514", DisplayName: "Claude Sonnet 4"},
				{Type: "model", ID: "claude-opus-4-20250514", DisplayName: "Claude Opus 4"},
			},
			HasMore: false,
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.ListModels(context.Background(), &ListModelsOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Data))
	}
	if resp.Data[0].ID != "claude-sonnet-4-20250514" {
		t.Errorf("unexpected model id: %s", resp.Data[0].ID)
	}
}

// ============================================================================
// Test StreamMessage
// ============================================================================

func TestStreamMessage(t *testing.T) {
	// 模拟 SSE 流
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)

		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Error("expected stream=true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-20250514","stop_reason":"","usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		}

		for _, data := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n",
				jsonEventType(data), data)
			flusher.Flush()
		}
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	// 使用 StreamAccumulator
	acc := NewStreamAccumulator()
	err := client.StreamMessage(context.Background(), MessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi")},
		},
	}, acc.OnEvent)
	if err != nil {
		t.Fatalf("StreamMessage failed: %v", err)
	}

	result := acc.Result()
	if result.ID != "msg_1" {
		t.Errorf("expected id=msg_1, got %s", result.ID)
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(result.Content))
	}
	if result.Content[0].Text != "Hello world" {
		t.Errorf("expected text='Hello world', got %q", result.Content[0].Text)
	}
	if result.StopReason != StopReasonEndTurn {
		t.Errorf("expected stop_reason=end_turn, got %s", result.StopReason)
	}
	if result.Usage.OutputTokens != 2 {
		t.Errorf("expected output_tokens=2, got %d", result.Usage.OutputTokens)
	}
}

// jsonEventType 从 JSON 数据中提取 type 字段。
func jsonEventType(data string) string {
	var v struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &v)
	return v.Type
}

// ============================================================================
// Test StreamMessage Channel
// ============================================================================

func TestStreamMessageChannel(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`{"type":"ping"}`,
			`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-20250514","stop_reason":"","usage":{"input_tokens":5,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		}

		for _, data := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", jsonEventType(data), data)
			flusher.Flush()
		}
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	ch, errCh := client.StreamMessageChannel(context.Background(), MessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("test")}},
	}, StreamConfig{})

	var events []StreamEvent
	for event := range ch {
		events = append(events, event)
	}
	err := <-errCh
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) < 3 {
		t.Errorf("expected at least 3 events, got %d", len(events))
	}
}

// ============================================================================
// Test StreamMessage Context Cancel
// ============================================================================

func TestStreamMessageContextCancel(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// 发送初始事件，然后长时间等待
		data := `{"type":"ping"}`
		_, _ = fmt.Fprintf(w, "event: ping\ndata: %s\n\n", data)
		flusher.Flush()

		// 阻塞等待，直到连接关闭
		<-r.Context().Done()
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := client.StreamMessage(ctx, MessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("test")}},
	}, func(event StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error due to context cancel")
	}
}

// ============================================================================
// Test MessageContent JSON
// ============================================================================

func TestMessageContentJSON(t *testing.T) {
	// 简单字符串序列化
	msg := Message{Role: "user", Content: TextContent("hello")}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	// content 应为字符串 "hello"
	if !strings.Contains(string(data), `"content":"hello"`) {
		t.Errorf("expected content as string, got %s", data)
	}

	// 反序列化字符串
	var msg2 Message
	_ = json.Unmarshal(data, &msg2)
	if len(msg2.Content) != 1 || msg2.Content[0].Text != "hello" {
		t.Errorf("expected 1 text block, got %+v", msg2.Content)
	}

	// 多内容块序列化为数组
	msg3 := Message{
		Role: "assistant",
		Content: MessageContent{
			{Type: ContentTypeText, Text: "part1"},
			{Type: ContentTypeText, Text: "part2"},
		},
	}
	data3, _ := json.Marshal(msg3)
	if !strings.Contains(string(data3), `[{`) {
		t.Errorf("expected content as array, got %s", data3)
	}
}

// ============================================================================
// Test StreamAccumulator Tool Use
// ============================================================================

func TestStreamAccumulatorToolUse(t *testing.T) {
	acc := NewStreamAccumulator()

	events := []StreamEvent{
		{Type: EventMessageStart, Message: &MessageResponse{
			ID: "msg_3", Model: "claude-sonnet-4-20250514", Role: RoleAssistant,
			Usage: Usage{InputTokens: 100},
		}},
		{Type: EventContentBlockStart, Index: intPtr(0), ContentBlock: &ContentBlock{
			Type: ContentTypeToolUse, ID: "toolu_1", Name: "get_weather",
		}},
		{Type: EventContentBlockDelta, Index: intPtr(0), Delta: &Delta{
			Type: "input_json_delta", PartialJSON: `{"location"`,
		}},
		{Type: EventContentBlockDelta, Index: intPtr(0), Delta: &Delta{
			Type: "input_json_delta", PartialJSON: `:"SF"}`,
		}},
		{Type: EventContentBlockStop, Index: intPtr(0)},
		{Type: EventMessageDelta, Delta: &Delta{StopReason: StopReasonToolUse},
			Usage: &Usage{OutputTokens: 50}},
		{Type: EventMessageStop},
	}

	for _, event := range events {
		if err := acc.OnEvent(event); err != nil {
			t.Fatalf("OnEvent failed: %v", err)
		}
	}

	result := acc.Result()
	if result.StopReason != StopReasonToolUse {
		t.Errorf("expected stop_reason=tool_use, got %s", result.StopReason)
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(result.Content))
	}
	block := result.Content[0]
	if block.Type != ContentTypeToolUse {
		t.Errorf("expected tool_use, got %s", block.Type)
	}
	if block.ID != "toolu_1" || block.Name != "get_weather" {
		t.Errorf("unexpected tool: id=%s name=%s", block.ID, block.Name)
	}
	// 验证合并后的 input JSON
	var input map[string]string
	if err := json.Unmarshal(block.Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["location"] != "SF" {
		t.Errorf("expected location=SF, got %s", input["location"])
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("expected output_tokens=50, got %d", result.Usage.OutputTokens)
	}
}

func intPtr(v int) *int { return &v }

// ============================================================================
// Test Prompt Caching
// ============================================================================

func TestAutomaticCaching(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		// 验证请求级 cache_control
		if req.CacheControl == nil {
			t.Error("expected non-nil CacheControl")
		} else if req.CacheControl.Type != "ephemeral" {
			t.Errorf("expected cache_control.type=ephemeral, got %s", req.CacheControl.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_cache_1",
			Type:       "message",
			Role:       "assistant",
			Content:    []ContentBlock{{Type: ContentTypeText, Text: "cached reply"}},
			Model:      req.Model,
			StopReason: StopReasonEndTurn,
			Usage: Usage{
				InputTokens:         50,
				OutputTokens:        10,
				CacheCreationTokens: 1000,
				CacheReadTokens:     5000,
			},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:        "claude-opus-4-8",
		MaxTokens:    1024,
		CacheControl: EphemeralCacheControl(),
		System:       SystemText("You are a helpful assistant."),
		Messages: []Message{
			{Role: "user", Content: TextContent("Hello")},
		},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
	if resp.Usage.CacheCreationTokens != 1000 {
		t.Errorf("expected cache_creation_tokens=1000, got %d", resp.Usage.CacheCreationTokens)
	}
	if resp.Usage.CacheReadTokens != 5000 {
		t.Errorf("expected cache_read_tokens=5000, got %d", resp.Usage.CacheReadTokens)
	}
}

func TestExplicitCacheBreakpoint(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		// 不应有请求级 cache_control
		if req.CacheControl != nil {
			t.Error("expected nil request-level CacheControl for explicit mode")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_cache_2",
			Type:       "message",
			Role:       "assistant",
			Content:    []ContentBlock{{Type: ContentTypeText, Text: "ok"}},
			Model:      req.Model,
			StopReason: StopReasonEndTurn,
			Usage:      Usage{InputTokens: 100, OutputTokens: 5, CacheReadTokens: 3000},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 1024,
		// 显式断点放在 system 上
		System: SystemTextWithCache("You are a helpful assistant.", EphemeralCacheControl()),
		Messages: []Message{
			{Role: "user", Content: TextContent("Hello")},
		},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
	if resp.Usage.CacheReadTokens != 3000 {
		t.Errorf("expected cache_read_tokens=3000, got %d", resp.Usage.CacheReadTokens)
	}
}

func TestCacheControl1hTTL(t *testing.T) {
	cc := EphemeralCacheControl1h()
	if cc.Type != "ephemeral" {
		t.Errorf("expected type=ephemeral, got %s", cc.Type)
	}
	if cc.TTL != "1h" {
		t.Errorf("expected ttl=1h, got %s", cc.TTL)
	}

	// 序列化检查
	data, err := json.Marshal(cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ttl":"1h"`) {
		t.Errorf("expected ttl in JSON, got %s", data)
	}
}

func TestCacheCreationUsageBreakdown(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg_ttl",
			"type": "message",
			"role": "assistant",
			"content": []map[string]string{
				{"type": "text", "text": "ok"},
			},
			"model":         "claude-opus-4-8",
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                2048,
				"output_tokens":               503,
				"cache_read_input_tokens":     1800,
				"cache_creation_input_tokens": 248,
				"cache_creation": map[string]int{
					"ephemeral_5m_input_tokens": 148,
					"ephemeral_1h_input_tokens": 100,
				},
			},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
	if resp.Usage.CacheCreation == nil {
		t.Fatal("expected non-nil CacheCreation")
	}
	if resp.Usage.CacheCreation.Ephemeral5mTokens != 148 {
		t.Errorf("expected 5m=148, got %d", resp.Usage.CacheCreation.Ephemeral5mTokens)
	}
	if resp.Usage.CacheCreation.Ephemeral1hTokens != 100 {
		t.Errorf("expected 1h=100, got %d", resp.Usage.CacheCreation.Ephemeral1hTokens)
	}
}

func TestToolCacheControl(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if len(req.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(req.Tools))
		}
		if req.Tools[0].CacheControl == nil {
			t.Error("expected tool CacheControl to be set")
		} else if req.Tools[0].CacheControl.Type != "ephemeral" {
			t.Errorf("expected ephemeral, got %s", req.Tools[0].CacheControl.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_tool_cache",
			Type:       "message",
			Role:       "assistant",
			Content:    []ContentBlock{{Type: ContentTypeText, Text: "done"}},
			Model:      req.Model,
			StopReason: StopReasonEndTurn,
			Usage:      Usage{InputTokens: 10, OutputTokens: 1},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 1024,
		Tools: []Tool{
			{
				Name:         "get_weather",
				Description:  "Get weather",
				InputSchema:  map[string]any{"type": "object"},
				CacheControl: EphemeralCacheControl(),
			},
		},
		Messages: []Message{{Role: "user", Content: TextContent("weather?")}},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
}

func TestContentBlockCacheControlSerialization(t *testing.T) {
	// 带 cache_control 的 ContentBlock 不应序列化为简单字符串
	msg := Message{
		Role: "user",
		Content: MessageContent{
			{Type: ContentTypeText, Text: "cached part", CacheControl: EphemeralCacheControl()},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	// 由于有 CacheControl，不应该序列化为简单字符串
	if strings.Contains(string(data), `"content":"cached part"`) {
		t.Errorf("expected array serialization due to CacheControl, got %s", data)
	}
	// 应该包含 cache_control
	if !strings.Contains(string(data), `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("expected cache_control in JSON, got %s", data)
	}
}

func TestPreWarmCache(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.MaxTokens != 1 {
			t.Errorf("expected max_tokens=1 for pre-warm, got %d", req.MaxTokens)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_warmup",
			Type:       "message",
			Role:       "assistant",
			Content:    []ContentBlock{},
			Model:      req.Model,
			StopReason: StopReasonMaxTokens,
			Usage:      Usage{InputTokens: 0, OutputTokens: 0, CacheCreationTokens: 5000},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 1, // pre-warm: 最小 max_tokens
		System:    SystemTextWithCache("Large system prompt...", EphemeralCacheControl()),
		Messages:  []Message{{Role: "user", Content: TextContent("warmup")}},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
	if resp.StopReason != StopReasonMaxTokens {
		t.Errorf("expected stop_reason=max_tokens, got %s", resp.StopReason)
	}
	if resp.Usage.CacheCreationTokens != 5000 {
		t.Errorf("expected cache_creation_tokens=5000, got %d", resp.Usage.CacheCreationTokens)
	}
}

// ============================================================================
// Test Vision
// ============================================================================

func TestBase64ImageBlock(t *testing.T) {
	block := Base64ImageBlock(ImagePNG, "iVBORw0KGgoAAAANSUhEUg==")
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"image"`) {
		t.Errorf("expected type=image, got %s", s)
	}
	if !strings.Contains(s, `"type":"base64"`) {
		t.Errorf("expected source.type=base64, got %s", s)
	}
	if !strings.Contains(s, `"media_type":"image/png"`) {
		t.Errorf("expected media_type=image/png, got %s", s)
	}
	if !strings.Contains(s, `"data":"iVBORw0KGgoAAAANSUhEUg=="`) {
		t.Errorf("expected data field, got %s", s)
	}
	// 不应包含 url 或 file_id
	if strings.Contains(s, `"url"`) {
		t.Errorf("should not have url field for base64, got %s", s)
	}
	if strings.Contains(s, `"file_id"`) {
		t.Errorf("should not have file_id field for base64, got %s", s)
	}
}

func TestURLImageBlock(t *testing.T) {
	block := URLImageBlock("https://example.com/image.jpg")
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"url"`) {
		t.Errorf("expected source.type=url, got %s", s)
	}
	if !strings.Contains(s, `"url":"https://example.com/image.jpg"`) {
		t.Errorf("expected url field, got %s", s)
	}
	// 不应包含 media_type 或 data
	if strings.Contains(s, `"media_type"`) {
		t.Errorf("should not have media_type for url source, got %s", s)
	}
	if strings.Contains(s, `"data"`) {
		t.Errorf("should not have data field for url source, got %s", s)
	}
}

func TestFileImageBlock(t *testing.T) {
	block := FileImageBlock("file_abc123")
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"file"`) {
		t.Errorf("expected source.type=file, got %s", s)
	}
	if !strings.Contains(s, `"file_id":"file_abc123"`) {
		t.Errorf("expected file_id field, got %s", s)
	}
}

func TestImageWithText(t *testing.T) {
	content := ImageWithText(
		URLImageBlock("https://example.com/img.jpg"),
		"Describe this image.",
	)
	if len(content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(content))
	}
	if content[0].Type != ContentTypeImage {
		t.Errorf("expected first block to be image, got %s", content[0].Type)
	}
	if content[1].Type != ContentTypeText || content[1].Text != "Describe this image." {
		t.Errorf("expected second block to be text, got %+v", content[1])
	}
}

func TestMultiImageContent(t *testing.T) {
	content := MultiImageContent(
		"How are these images different?",
		URLImageBlock("https://example.com/a.jpg"),
		URLImageBlock("https://example.com/b.jpg"),
	)
	if len(content) != 5 {
		t.Fatalf("expected 5 blocks (2 text + 2 image + 1 text), got %d", len(content))
	}
	// "Image 1:" -> image -> "Image 2:" -> image -> question
	if content[0].Text != "Image 1:" {
		t.Errorf("expected 'Image 1:', got %q", content[0].Text)
	}
	if content[1].Type != ContentTypeImage {
		t.Errorf("expected image at index 1, got %s", content[1].Type)
	}
	if content[2].Text != "Image 2:" {
		t.Errorf("expected 'Image 2:', got %q", content[2].Text)
	}
	if content[3].Type != ContentTypeImage {
		t.Errorf("expected image at index 3, got %s", content[3].Type)
	}
	if content[4].Text != "How are these images different?" {
		t.Errorf("expected question, got %q", content[4].Text)
	}
}

func TestVisionMessageRequest(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)

		var req MessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(req.Messages))
		}
		msg := req.Messages[0]
		// 内容应为数组（图片+文本），不是简单字符串
		if len(msg.Content) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
		}
		if msg.Content[0].Type != ContentTypeImage {
			t.Errorf("expected first block=image, got %s", msg.Content[0].Type)
		}
		if msg.Content[0].Source == nil {
			t.Fatal("expected non-nil source")
		}
		if msg.Content[0].Source.Type != "base64" {
			t.Errorf("expected source.type=base64, got %s", msg.Content[0].Source.Type)
		}
		if msg.Content[0].Source.MediaType != ImagePNG {
			t.Errorf("expected media_type=%s, got %s", ImagePNG, msg.Content[0].Source.MediaType)
		}
		if msg.Content[1].Type != ContentTypeText || msg.Content[1].Text != "What is in this image?" {
			t.Errorf("unexpected text block: %+v", msg.Content[1])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_vision_1",
			Type:       "message",
			Role:       "assistant",
			Content:    []ContentBlock{{Type: ContentTypeText, Text: "It's a test image."}},
			Model:      req.Model,
			StopReason: StopReasonEndTurn,
			Usage:      Usage{InputTokens: 64, OutputTokens: 10},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 1024,
		Messages: []Message{
			{
				Role:    "user",
				Content: ImageWithText(Base64ImageBlock(ImagePNG, "iVBORw0KGgo="), "What is in this image?"),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
	if resp.ID != "msg_vision_1" {
		t.Errorf("expected id=msg_vision_1, got %s", resp.ID)
	}
}

func TestVisionFileSourceRequest(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		msg := req.Messages[0]
		if msg.Content[0].Source == nil || msg.Content[0].Source.Type != "file" {
			t.Fatalf("expected file source, got %+v", msg.Content[0].Source)
		}
		if msg.Content[0].Source.FileID != "file_abc123" {
			t.Errorf("expected file_id=file_abc123, got %s", msg.Content[0].Source.FileID)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_vision_file",
			Type:       "message",
			Role:       "assistant",
			Content:    []ContentBlock{{Type: ContentTypeText, Text: "ok"}},
			Model:      req.Model,
			StopReason: StopReasonEndTurn,
			Usage:      Usage{InputTokens: 10, OutputTokens: 1},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 1024,
		Messages: []Message{
			{
				Role:    "user",
				Content: ImageWithText(FileImageBlock("file_abc123"), "Describe this image."),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}
	if resp.ID != "msg_vision_file" {
		t.Errorf("expected id=msg_vision_file, got %s", resp.ID)
	}
}

// ============================================================================
// Test Image Token Calculation & ResizedSize
// ============================================================================

func TestCountImageTokens(t *testing.T) {
	tests := []struct {
		width, height, expected int
	}{
		{200, 200, 64},     // ceil(200/28)=8, 8*8=64
		{1000, 1000, 1296}, // ceil(1000/28)=36, 36*36=1296
		{1092, 1092, 1521}, // ceil(1092/28)=39, 39*39=1521
		{1920, 1080, 2691}, // ceil(1920/28)=69, ceil(1080/28)=39, 69*39=2691
		{28, 28, 1},
		{1, 1, 1},
	}
	for _, tt := range tests {
		got := CountImageTokens(tt.width, tt.height)
		if got != tt.expected {
			t.Errorf("CountImageTokens(%d, %d) = %d, want %d", tt.width, tt.height, got, tt.expected)
		}
	}
}

func TestResizedSizeStandard(t *testing.T) {
	// 小图片不需要缩放
	w, h := ResizedSizeStandard(200, 200)
	if w != 200 || h != 200 {
		t.Errorf("small image should be unchanged, got %dx%d", w, h)
	}

	// 文档中的 A4 示例：1075×1520 -> 924×1307
	w, h = ResizedSizeStandard(1075, 1520)
	if w != 924 || h != 1307 {
		t.Errorf("A4 example: expected 924x1307, got %dx%d", w, h)
	}

	// 在 token 和边限内的大图不缩放（800x800 = 841 tokens, edges 812px）
	w, h = ResizedSizeStandard(800, 800)
	if w != 800 || h != 800 {
		t.Errorf("800x800 should fit, got %dx%d", w, h)
	}

	// 超过边限 -> 按比例缩小
	w, h = ResizedSizeStandard(3000, 2000)
	if w > 1568 || h > 1568 {
		t.Errorf("resized should be within 1568 edge limit, got %dx%d", w, h)
	}
	if CountImageTokens(w, h) > ImageMaxTokensStandard {
		t.Errorf("resized tokens %d exceed %d", CountImageTokens(w, h), ImageMaxTokensStandard)
	}
}

func TestResizedSizeHighRes(t *testing.T) {
	// 标准模型会缩放的图，高分辨率模型可能不缩放
	w, h := ResizedSizeHighRes(2000, 1500)
	// 高分辨率模型：边限 2576，token 限 4784
	// 2000x1500: ceil(2000/28)=72, ceil(1500/28)=54, 72*54=3888 <= 4784, 边都在 2576 以内
	if w != 2000 || h != 1500 {
		t.Errorf("2000x1500 should fit high-res limits, got %dx%d", w, h)
	}

	// 超过高分辨率限制的大图需缩放
	w, h = ResizedSizeHighRes(3840, 2160)
	// 文档说 4K 图在高分辨率模型上缩到 2576x1449
	if w != 2576 || h != 1449 {
		t.Errorf("4K high-res: expected 2576x1449, got %dx%d", w, h)
	}
}

func TestResizedSizeAspectRatio(t *testing.T) {
	// 缩放后应保持宽高比
	origW, origH := 3000, 1000
	w, h := ResizedSizeStandard(origW, origH)
	origRatio := float64(origW) / float64(origH)
	newRatio := float64(w) / float64(h)
	if math.Abs(origRatio-newRatio) > 0.01 {
		t.Errorf("aspect ratio changed: orig=%.4f new=%.4f (%dx%d)", origRatio, newRatio, w, h)
	}
}

func TestToRelativeCoordinates(t *testing.T) {
	// 使用 A4 示例：原图 1075×1520，Claude 看到 924×1307
	// 如果 Claude 返回坐标 (924, 1307)，相对坐标应为 (1.0, 1.0)
	relX, relY := ToRelativeCoordinates(924, 1307, 1075, 1520, ImageMaxEdgeStandard, ImageMaxTokensStandard)
	if relX < 0.99 || relX > 1.01 {
		t.Errorf("expected relX≈1.0, got %.4f", relX)
	}
	if relY < 0.99 || relY > 1.01 {
		t.Errorf("expected relY≈1.0, got %.4f", relY)
	}

	// 中点应约为 (0.5, 0.5)
	relX2, relY2 := ToRelativeCoordinates(462, 654, 1075, 1520, ImageMaxEdgeStandard, ImageMaxTokensStandard)
	if relX2 < 0.49 || relX2 > 0.51 {
		t.Errorf("expected relX≈0.5, got %.4f", relX2)
	}
	if relY2 < 0.49 || relY2 > 0.51 {
		t.Errorf("expected relY≈0.5, got %.4f", relY2)
	}
}

// ============================================================================
// Test Files API
// ============================================================================

func checkBeta(t *testing.T, r *http.Request) {
	t.Helper()
	if beta := r.Header.Get("anthropic-beta"); beta != BetaFilesAPI {
		t.Errorf("expected anthropic-beta=%s, got %q", BetaFilesAPI, beta)
	}
}

func TestUploadFile(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		checkBeta(t, r)
		if r.URL.Path != "/v1/files" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		// 解析 multipart form
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
		}
		defer func() { _ = file.Close() }()
		if header.Filename != "test.txt" {
			t.Errorf("expected filename=test.txt, got %s", header.Filename)
		}
		content, _ := io.ReadAll(file)
		if string(content) != "hello file" {
			t.Errorf("expected content='hello file', got %q", content)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(FileMetadata{
			ID:        "file_abc123",
			Filename:  "test.txt",
			MimeType:  "text/plain",
			SizeBytes: int64(len(content)),
			Type:      "file",
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	meta, err := client.UploadFile(context.Background(), UploadFileParams{
		Filename: "test.txt",
		MimeType: "text/plain",
		Reader:   strings.NewReader("hello file"),
	})
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}
	if meta.ID != "file_abc123" {
		t.Errorf("expected id=file_abc123, got %s", meta.ID)
	}
	if meta.Filename != "test.txt" {
		t.Errorf("expected filename=test.txt, got %s", meta.Filename)
	}
	if meta.SizeBytes != 10 {
		t.Errorf("expected size_bytes=10, got %d", meta.SizeBytes)
	}
}

func TestListFiles(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkBeta(t, r)
		if r.URL.Path != "/v1/files" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if limit := r.URL.Query().Get("limit"); limit != "5" {
			t.Errorf("expected limit=5, got %s", limit)
		}
		if afterID := r.URL.Query().Get("after_id"); afterID != "file_xyz" {
			t.Errorf("expected after_id=file_xyz, got %s", afterID)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ListFilesResponse{
			Data: []FileMetadata{
				{ID: "file_1", Filename: "a.pdf", Type: "file"},
				{ID: "file_2", Filename: "b.pdf", Type: "file"},
			},
			FirstID: "file_1",
			LastID:  "file_2",
			HasMore: false,
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := client.ListFiles(context.Background(), &ListFilesOptions{
		Limit:   5,
		AfterID: "file_xyz",
	})
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 files, got %d", len(resp.Data))
	}
	if resp.Data[0].ID != "file_1" {
		t.Errorf("expected first id=file_1, got %s", resp.Data[0].ID)
	}
}

func TestDownloadFile(t *testing.T) {
	fileContent := []byte("binary PDF data\x00\x01")
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkBeta(t, r)
		if r.URL.Path != "/v1/files/file_abc123/content" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(fileContent)
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	data, contentType, err := client.DownloadFile(context.Background(), "file_abc123")
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}
	if contentType != "application/pdf" {
		t.Errorf("expected content-type=application/pdf, got %s", contentType)
	}
	if string(data) != string(fileContent) {
		t.Errorf("content mismatch")
	}
}

func TestGetFileMetadata(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkBeta(t, r)
		if r.URL.Path != "/v1/files/file_abc123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(FileMetadata{
			ID:        "file_abc123",
			Filename:  "doc.pdf",
			MimeType:  "application/pdf",
			SizeBytes: 102400,
			Type:      "file",
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	meta, err := client.GetFileMetadata(context.Background(), "file_abc123")
	if err != nil {
		t.Fatalf("GetFileMetadata failed: %v", err)
	}
	if meta.ID != "file_abc123" {
		t.Errorf("expected id=file_abc123, got %s", meta.ID)
	}
	if meta.SizeBytes != 102400 {
		t.Errorf("expected size_bytes=102400, got %d", meta.SizeBytes)
	}
}

func TestDeleteFile(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		checkBeta(t, r)
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/v1/files/file_abc123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(DeletedFile{
			ID:   "file_abc123",
			Type: "file_deleted",
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	result, err := client.DeleteFile(context.Background(), "file_abc123")
	if err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}
	if result.ID != "file_abc123" {
		t.Errorf("expected id=file_abc123, got %s", result.ID)
	}
	if result.Type != "file_deleted" {
		t.Errorf("expected type=file_deleted, got %s", result.Type)
	}
}

// ============================================================================
// Test Extended Thinking Streaming
// ============================================================================

func TestStreamAccumulatorThinking(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`{"type":"message_start","message":{"id":"msg_think","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":"","usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" step by step"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_abc"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"The answer is 42."}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`,
		}

		for _, data := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", jsonEventType(data), data)
			flusher.Flush()
		}
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	acc := NewStreamAccumulator()
	err := client.StreamMessage(context.Background(), MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 4096,
		Thinking:  ThinkingEnabled(10000),
		Messages:  []Message{{Role: "user", Content: TextContent("What is the meaning of life?")}},
	}, acc.OnEvent)
	if err != nil {
		t.Fatalf("StreamMessage failed: %v", err)
	}

	result := acc.Result()
	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(result.Content))
	}

	// Block 0: thinking
	if result.Content[0].Type != ContentTypeThinking {
		t.Errorf("expected block 0 type=thinking, got %s", result.Content[0].Type)
	}
	if result.Content[0].Thinking != "Let me think step by step" {
		t.Errorf("expected thinking='Let me think step by step', got %q", result.Content[0].Thinking)
	}
	if result.Content[0].Signature != "sig_abc" {
		t.Errorf("expected signature='sig_abc', got %q", result.Content[0].Signature)
	}

	// Block 1: text
	if result.Content[1].Type != ContentTypeText {
		t.Errorf("expected block 1 type=text, got %s", result.Content[1].Type)
	}
	if result.Content[1].Text != "The answer is 42." {
		t.Errorf("expected text='The answer is 42.', got %q", result.Content[1].Text)
	}
}

// ============================================================================
// Test Result() Idempotency (Bug Fix #3)
// ============================================================================

func TestStreamAccumulatorResultIdempotent(t *testing.T) {
	acc := NewStreamAccumulator()

	// Simulate events
	idx0 := 0
	idx1 := 1
	_ = acc.OnEvent(StreamEvent{
		Type:         EventContentBlockStart,
		Index:        &idx0,
		ContentBlock: &ContentBlock{Type: ContentTypeText, Text: ""},
	})
	_ = acc.OnEvent(StreamEvent{
		Type:  EventContentBlockDelta,
		Index: &idx0,
		Delta: &Delta{Type: "text_delta", Text: "Hello world"},
	})
	_ = acc.OnEvent(StreamEvent{
		Type:         EventContentBlockStart,
		Index:        &idx1,
		ContentBlock: &ContentBlock{Type: ContentTypeToolUse, ID: "tool_1", Name: "get_weather"},
	})
	_ = acc.OnEvent(StreamEvent{
		Type:  EventContentBlockDelta,
		Index: &idx1,
		Delta: &Delta{Type: "input_json_delta", PartialJSON: `{"location":"SF"}`},
	})

	// Call Result() multiple times — content should NOT change
	r1 := acc.Result()
	r2 := acc.Result()

	if len(r1.Content) != len(r2.Content) {
		t.Fatalf("content block count changed: r1=%d r2=%d", len(r1.Content), len(r2.Content))
	}
	for i := range r1.Content {
		if r1.Content[i].Text != r2.Content[i].Text {
			t.Errorf("text changed on block %d: r1=%q r2=%q", i, r1.Content[i].Text, r2.Content[i].Text)
		}
		if string(r1.Content[i].Input) != string(r2.Content[i].Input) {
			t.Errorf("input changed on block %d: r1=%s r2=%s", i, r1.Content[i].Input, r2.Content[i].Input)
		}
	}

	// Verify correct content
	if r1.Content[0].Text != "Hello world" {
		t.Errorf("expected text='Hello world', got %q", r1.Content[0].Text)
	}
	if string(r1.Content[1].Input) != `{"location":"SF"}` {
		t.Errorf("expected input JSON, got %s", r1.Content[1].Input)
	}
}

// ============================================================================
// Test Parameter Validation (Bug Fix #6)
// ============================================================================

func TestValidationErrors(t *testing.T) {
	client := New(WithAPIKey("test-key"))

	// Missing model
	_, err := client.CreateMessage(context.Background(), MessageRequest{
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("expected model required error, got %v", err)
	}

	// Missing messages
	_, err = client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "messages must not be empty") {
		t.Errorf("expected messages required error, got %v", err)
	}

	// MaxTokens <= 0
	_, err = client.CreateMessage(context.Background(), MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 0,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil || !strings.Contains(err.Error(), "max_tokens must be > 0") {
		t.Errorf("expected max_tokens error, got %v", err)
	}

	// Validation should also apply to streaming
	err = client.StreamMessage(context.Background(), MessageRequest{
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	}, func(StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("stream: expected model required error, got %v", err)
	}

	// CountTokens validation
	_, err = client.CountTokens(context.Background(), CountTokensRequest{
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("count_tokens: expected model required error, got %v", err)
	}
}

// ============================================================================
// Test Stream API Error Parsing (Bug Fix #4)
// ============================================================================

func TestStreamAPIError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ErrorResponse{
			Type: "error",
			Error: APIError{
				Type:    "invalid_request_error",
				Message: "max_tokens must be <= 128000",
			},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	acc := NewStreamAccumulator()
	err := client.StreamMessage(context.Background(), MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 999999,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	}, acc.OnEvent)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Type != "invalid_request_error" {
		t.Errorf("expected type=invalid_request_error, got %s", apiErr.Type)
	}
	if apiErr.Message != "max_tokens must be <= 128000" {
		t.Errorf("unexpected message: %s", apiErr.Message)
	}
}

// ============================================================================
// Test UploadFile with Custom MIME Type (Bug Fix #2)
// ============================================================================

func TestUploadFileWithMimeType(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer func() { _ = file.Close() }()

		// Verify the custom MIME type is set
		if header.Header.Get("Content-Type") != "application/pdf" {
			t.Errorf("expected Content-Type=application/pdf, got %s", header.Header.Get("Content-Type"))
		}
		if header.Filename != "report.pdf" {
			t.Errorf("expected filename=report.pdf, got %s", header.Filename)
		}

		content, _ := io.ReadAll(file)
		if string(content) != "%PDF-1.4 fake" {
			t.Errorf("unexpected content: %q", content)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(FileMetadata{
			ID:       "file_pdf_123",
			Filename: "report.pdf",
			MimeType: "application/pdf",
			Type:     "file",
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	meta, err := client.UploadFile(context.Background(), UploadFileParams{
		Filename: "report.pdf",
		MimeType: "application/pdf",
		Reader:   strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}
	if meta.ID != "file_pdf_123" {
		t.Errorf("expected id=file_pdf_123, got %s", meta.ID)
	}
}
