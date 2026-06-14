package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/util/log"
)

func TestMain(m *testing.M) {
	_ = log.InitWithConfig(log.Config{
		Level:   "debug",
		Outputs: []log.Output{log.Stdout()},
	})
	os.Exit(m.Run())
}

// ============================================================================
// GenerateContent
// ============================================================================

func TestGenerateContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证路径
		if !strings.Contains(r.URL.Path, "gemini-2.5-flash:generateContent") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		// 验证 API Key header
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("expected x-goog-api-key header, got %s", r.Header.Get("x-goog-api-key"))
		}
		// 验证 Content-Type
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}

		// 解析请求体
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if len(req.Contents) != 1 {
			t.Fatalf("expected 1 content, got %d", len(req.Contents))
		}
		if len(req.Contents[0].Parts) != 1 {
			t.Fatalf("expected 1 part, got %d", len(req.Contents[0].Parts))
		}
		if req.Contents[0].Parts[0].Text != "Hello, Gemini!" {
			t.Errorf("unexpected text: %s", req.Contents[0].Parts[0].Text)
		}

		// 返回模拟响应
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "Hello! How can I help you?"}},
				},
				FinishReason: FinishReasonStop,
				Index:        0,
			}},
			UsageMetadata: &UsageMetadata{
				PromptTokenCount:     5,
				CandidatesTokenCount: 10,
				TotalTokenCount:      15,
			},
			ModelVersion: "gemini-2.5-flash",
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("Hello, Gemini!")},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(resp.Candidates))
	}
	if resp.Candidates[0].Content.Parts[0].Text != "Hello! How can I help you?" {
		t.Errorf("unexpected response text: %s", resp.Candidates[0].Content.Parts[0].Text)
	}
	if resp.Candidates[0].FinishReason != FinishReasonStop {
		t.Errorf("expected STOP, got %s", resp.Candidates[0].FinishReason)
	}
	if resp.UsageMetadata.TotalTokenCount != 15 {
		t.Errorf("expected 15 tokens, got %d", resp.UsageMetadata.TotalTokenCount)
	}
}

func TestGenerateContentWithSystemInstruction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		if req.SystemInstruction == nil {
			t.Error("expected system instruction")
		}
		if req.SystemInstruction.Parts[0].Text != "You are a helpful assistant" {
			t.Errorf("unexpected system instruction: %s", req.SystemInstruction.Parts[0].Text)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "OK"}},
				},
				FinishReason: FinishReasonStop,
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("Hi")},
		}},
		SystemInstruction: &Content{
			Parts: []Part{TextPart("You are a helpful assistant")},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateContentWithThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		if req.GenerationConfig == nil || req.GenerationConfig.ThinkingConfig == nil {
			t.Error("expected thinking config")
		}
		if !req.GenerationConfig.ThinkingConfig.IncludeThoughts {
			t.Error("expected includeThoughts=true")
		}

		// 返回带思考摘要的响应
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role: RoleModel,
					Parts: []Part{
						{Text: "Let me think about this...", Thought: true},
						{Text: "The answer is 42."},
					},
				},
				FinishReason: FinishReasonStop,
			}},
			UsageMetadata: &UsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 20,
				ThoughtsTokenCount:   50,
				TotalTokenCount:      80,
			},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	budget := 1024
	resp, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("What is the meaning of life?")},
		}},
		GenerationConfig: &GenerationConfig{
			ThinkingConfig: &ThinkingConfig{
				IncludeThoughts: true,
				ThinkingBudget:  &budget,
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if !parts[0].Thought {
		t.Error("expected first part to be a thought")
	}
	if parts[0].Text != "Let me think about this..." {
		t.Errorf("unexpected thought text: %s", parts[0].Text)
	}
	if parts[1].Text != "The answer is 42." {
		t.Errorf("unexpected answer text: %s", parts[1].Text)
	}
	if resp.UsageMetadata.ThoughtsTokenCount != 50 {
		t.Errorf("expected 50 thoughts tokens, got %d", resp.UsageMetadata.ThoughtsTokenCount)
	}
}

func TestGenerateContentWithFunctionCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		// 验证工具声明
		if len(req.Tools) != 1 || len(req.Tools[0].FunctionDeclarations) != 1 {
			t.Error("expected 1 function declaration")
		}
		fd := req.Tools[0].FunctionDeclarations[0]
		if fd.Name != "get_weather" {
			t.Errorf("expected get_weather, got %s", fd.Name)
		}

		// 返回函数调用
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role: RoleModel,
					Parts: []Part{{
						FunctionCall: &FunctionCall{
							Name: "get_weather",
							ID:   "call_123",
							Args: map[string]any{"location": "London"},
						},
					}},
				},
				FinishReason: FinishReasonStop,
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("What's the weather in London?")},
		}},
		Tools: []Tool{{
			FunctionDeclarations: []FunctionDeclaration{{
				Name:        "get_weather",
				Description: "Get weather for a location",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fc := resp.Candidates[0].Content.Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("expected function call")
	}
	if fc.Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", fc.Name)
	}
	if fc.ID != "call_123" {
		t.Errorf("expected call_123, got %s", fc.ID)
	}
	if fc.Args["location"] != "London" {
		t.Errorf("expected London, got %v", fc.Args["location"])
	}
}

func TestGenerateContentWithStructuredOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		if req.GenerationConfig == nil {
			t.Fatal("expected generation config")
		}
		if req.GenerationConfig.ResponseMIMEType != "application/json" {
			t.Errorf("expected application/json, got %s", req.GenerationConfig.ResponseMIMEType)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: `{"sentiment":"positive","summary":"Great!"}`}},
				},
				FinishReason: FinishReasonStop,
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("Classify this feedback: 'Love it!'")},
		}},
		GenerationConfig: &GenerationConfig{
			ResponseMIMEType: "application/json",
			ResponseSchema:   json.RawMessage(`{"type":"object","properties":{"sentiment":{"type":"string","enum":["positive","negative","neutral"]},"summary":{"type":"string"}},"required":["sentiment","summary"]}`),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result struct {
		Sentiment string `json:"sentiment"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(resp.Candidates[0].Content.Parts[0].Text), &result); err != nil {
		t.Fatalf("unmarshal structured output: %v", err)
	}
	if result.Sentiment != "positive" {
		t.Errorf("expected positive, got %s", result.Sentiment)
	}
}

func TestGenerateContentValidationError(t *testing.T) {
	c := New(WithAPIKey("test-key"))

	_, err := c.GenerateContent(context.Background(), "", GenerateContentRequest{
		Contents: []Content{{Parts: []Part{TextPart("hi")}}},
	})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("expected model required error, got %v", err)
	}

	_, err = c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{})
	if err == nil || !strings.Contains(err.Error(), "contents must not be empty") {
		t.Errorf("expected contents empty error, got %v", err)
	}
}

func TestGenerateContentAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(ErrorResponse{
			Error: &APIError{
				Code:    400,
				Message: "Invalid model name",
				Status:  "INVALID_ARGUMENT",
			},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := c.GenerateContent(context.Background(), "bad-model", GenerateContentRequest{
		Contents: []Content{{Parts: []Part{TextPart("hi")}}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Message != "Invalid model name" {
		t.Errorf("expected 'Invalid model name', got %s", apiErr.Message)
	}
}

// ============================================================================
// Streaming
// ============================================================================

func TestStreamGenerateContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证 alt=sse 查询参数
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("expected alt=sse, got %s", r.URL.Query().Get("alt"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		// 模拟 3 个 chunk
		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":", World"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":10}}`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	var texts []string
	err := c.StreamGenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("Say hello")},
		}},
	}, func(resp GenerateContentResponse) error {
		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			texts = append(texts, resp.Candidates[0].Content.Parts[0].Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"Hello", ", World", "!"}
	if len(texts) != len(expected) {
		t.Fatalf("expected %d chunks, got %d", len(expected), len(texts))
	}
	for i, text := range texts {
		if text != expected[i] {
			t.Errorf("chunk %d: expected %q, got %q", i, expected[i], text)
		}
	}
}

func TestStreamAccumulator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello, "}]},"index":0}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"World!"}]},"index":0}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":5,"totalTokenCount":10}}`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	acc := NewStreamAccumulator()
	err := c.StreamGenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("Greet me")},
		}},
	}, acc.OnChunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result := acc.Result()
	if len(result.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(result.Candidates))
	}

	parts := result.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "Hello, World!" {
		t.Errorf("expected combined text 'Hello, World!', got parts: %+v", parts)
	}
	if result.Candidates[0].FinishReason != FinishReasonStop {
		t.Errorf("expected STOP, got %s", result.Candidates[0].FinishReason)
	}
	if result.UsageMetadata == nil || result.UsageMetadata.TotalTokenCount != 10 {
		t.Errorf("expected 10 total tokens, got %+v", result.UsageMetadata)
	}
}

func TestStreamGenerateContentWithThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Thinking...","thought":true}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"The answer"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":" is 42."}]},"finishReason":"STOP"}],"usageMetadata":{"thoughtsTokenCount":100,"totalTokenCount":120}}`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	var thoughts, answer string
	err := c.StreamGenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("What is the answer?")},
		}},
	}, func(resp GenerateContentResponse) error {
		if len(resp.Candidates) > 0 {
			for _, part := range resp.Candidates[0].Content.Parts {
				if part.Thought {
					thoughts += part.Text
				} else {
					answer += part.Text
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if thoughts != "Thinking..." {
		t.Errorf("expected thoughts 'Thinking...', got %q", thoughts)
	}
	if answer != "The answer is 42." {
		t.Errorf("expected 'The answer is 42.', got %q", answer)
	}
}

func TestStreamGenerateContentChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"A"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"B"}]},"finishReason":"STOP"}]}`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	ch, errCh := c.StreamGenerateContentChannel(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("hi")},
		}},
	}, StreamConfig{})

	var texts []string
	for resp := range ch {
		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			texts = append(texts, resp.Candidates[0].Content.Parts[0].Text)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(texts) != 2 || texts[0] != "A" || texts[1] != "B" {
		t.Errorf("expected [A, B], got %v", texts)
	}
}

// ============================================================================
// Models
// ============================================================================

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageSize") != "10" {
			t.Errorf("expected pageSize=10, got %s", r.URL.Query().Get("pageSize"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ListModelsResponse{
			Models: []Model{
				{Name: "models/gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash"},
				{Name: "models/gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro"},
			},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.ListModels(context.Background(), &ListModelsOptions{PageSize: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Models))
	}
	if resp.Models[0].Name != "models/gemini-2.5-flash" {
		t.Errorf("unexpected model name: %s", resp.Models[0].Name)
	}
}

func TestGetModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "models/gemini-2.5-flash") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Model{
			Name:                       "models/gemini-2.5-flash",
			DisplayName:                "Gemini 2.5 Flash",
			InputTokenLimit:            1048576,
			OutputTokenLimit:           8192,
			SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	// 测试不带 models/ 前缀
	m, err := c.GetModel(context.Background(), "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "models/gemini-2.5-flash" {
		t.Errorf("unexpected name: %s", m.Name)
	}
	if m.InputTokenLimit != 1048576 {
		t.Errorf("unexpected input token limit: %d", m.InputTokenLimit)
	}
}

// ============================================================================
// CountTokens
// ============================================================================

func TestCountTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "countTokens") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CountTokensResponse{TotalTokens: 42})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.CountTokens(context.Background(), "gemini-2.5-flash", CountTokensRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("Hello, how are you?")},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.TotalTokens != 42 {
		t.Errorf("expected 42 tokens, got %d", resp.TotalTokens)
	}
}

// ============================================================================
// Multi-turn function calling
// ============================================================================

func TestMultiTurnFunctionCalling(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")

		if requestCount == 1 {
			// 第一轮：模型发起函数调用
			_ = json.NewEncoder(w).Encode(GenerateContentResponse{
				Candidates: []Candidate{{
					Content: Content{
						Role: RoleModel,
						Parts: []Part{{
							FunctionCall: &FunctionCall{
								Name: "get_temperature",
								ID:   "call_abc",
								Args: map[string]any{"location": "Tokyo"},
							},
						}},
					},
					FinishReason: FinishReasonStop,
				}},
			})
		} else {
			// 第二轮：模型利用函数结果生成回答
			// 验证函数响应已被传入
			foundResponse := false
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if p.FunctionResponse != nil && p.FunctionResponse.Name == "get_temperature" {
						foundResponse = true
						if p.FunctionResponse.ID != "call_abc" {
							t.Errorf("expected call_abc, got %s", p.FunctionResponse.ID)
						}
					}
				}
			}
			if !foundResponse {
				t.Error("expected function response in second request")
			}

			_ = json.NewEncoder(w).Encode(GenerateContentResponse{
				Candidates: []Candidate{{
					Content: Content{
						Role:  RoleModel,
						Parts: []Part{{Text: "The temperature in Tokyo is 25°C."}},
					},
					FinishReason: FinishReasonStop,
				}},
			})
		}
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	tools := []Tool{{
		FunctionDeclarations: []FunctionDeclaration{{
			Name:        "get_temperature",
			Description: "Get temperature for a location",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`),
		}},
	}}

	// 第一轮
	resp1, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("What's the temperature in Tokyo?")},
		}},
		Tools: tools,
	})
	if err != nil {
		t.Fatalf("first request error: %v", err)
	}

	fc := resp1.Candidates[0].Content.Parts[0].FunctionCall
	if fc == nil || fc.Name != "get_temperature" {
		t.Fatal("expected function call in first response")
	}

	// 第二轮：传入函数响应
	contents := []Content{
		{Role: RoleUser, Parts: []Part{TextPart("What's the temperature in Tokyo?")}},
		resp1.Candidates[0].Content, // 模型的函数调用
		{
			Role: RoleUser,
			Parts: []Part{
				FunctionResponsePartWithID("get_temperature", fc.ID, map[string]any{"temperature": "25°C"}),
			},
		},
	}

	resp2, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: contents,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("second request error: %v", err)
	}

	text := resp2.Candidates[0].Content.Parts[0].Text
	if !strings.Contains(text, "Tokyo") || !strings.Contains(text, "25") {
		t.Errorf("unexpected final answer: %s", text)
	}
}

// ============================================================================
// Part constructors
// ============================================================================

func TestPartConstructors(t *testing.T) {
	// TextPart
	p := TextPart("hello")
	if p.Text != "hello" || p.Thought {
		t.Errorf("TextPart unexpected: %+v", p)
	}

	// ThoughtPart
	p2 := ThoughtPart("thinking")
	if p2.Text != "thinking" || !p2.Thought {
		t.Errorf("ThoughtPart unexpected: %+v", p2)
	}

	// InlineDataPart
	p3 := InlineDataPart("image/png", "base64data")
	if p3.InlineData == nil || p3.InlineData.MimeType != "image/png" || p3.InlineData.Data != "base64data" {
		t.Errorf("InlineDataPart unexpected: %+v", p3)
	}

	// FileDataPart
	p4 := FileDataPart("image/png", "https://example.com/img.png")
	if p4.FileData == nil || p4.FileData.FileURI != "https://example.com/img.png" {
		t.Errorf("FileDataPart unexpected: %+v", p4)
	}

	// FunctionCallPart
	p5 := FunctionCallPart("get_weather", map[string]any{"location": "NYC"})
	if p5.FunctionCall == nil || p5.FunctionCall.Name != "get_weather" {
		t.Errorf("FunctionCallPart unexpected: %+v", p5)
	}

	// FunctionResponsePart
	p6 := FunctionResponsePart("get_weather", map[string]any{"temp": 20})
	if p6.FunctionResponse == nil || p6.FunctionResponse.Name != "get_weather" {
		t.Errorf("FunctionResponsePart unexpected: %+v", p6)
	}

	// FunctionCallPartWithID
	p7 := FunctionCallPartWithID("get_weather", "id123", map[string]any{"location": "NYC"})
	if p7.FunctionCall == nil || p7.FunctionCall.ID != "id123" {
		t.Errorf("FunctionCallPartWithID unexpected: %+v", p7)
	}
}
