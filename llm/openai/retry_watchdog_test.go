package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// 辅助函数
// ============================================================================

// stallRequest 模拟服务器卡住：阻塞直到客户端断连或 maxWait 超时。
func stallRequest(r *http.Request, maxWait time.Duration) {
	select {
	case <-r.Context().Done():
	case <-time.After(maxWait):
	}
}

// mockChatResponse 构造一个标准的 Chat Completion 响应。
func mockChatResponse(text string) string {
	resp := ChatCompletionResponse{
		ID:      "chatcmpl-test-001",
		Object:  "chat.completion",
		Created: 1700000000,
		Model:   "test-model",
		Choices: []ChatChoice{
			{
				Index:        0,
				Message:      ChatUserMessage(text),
				FinishReason: "stop",
			},
		},
		Usage: &ChatUsage{
			PromptTokens:     5,
			CompletionTokens: 10,
			TotalTokens:      15,
		},
	}
	data, _ := json.Marshal(resp)
	return string(data)
}

// mockChatChunk 构造一个流式 chunk。
func mockChatChunk(content, finishReason string) string {
	chunk := ChatCompletionResponse{
		ID:     "chatcmpl-test-stream",
		Object: "chat.completion.chunk",
		Model:  "test-model",
		Choices: []ChatChoice{
			{
				Index:        0,
				Delta:        ChatDelta{Content: content},
				FinishReason: finishReason,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	return string(data)
}

// writeSSE 写入一个 SSE data 行并 flush。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// newTestClient 创建一个指向 mock server 的 chatMode 客户端。
func newTestClient(baseURL string, opts ...Option) *Client {
	allOpts := append([]Option{
		WithAPIKey("test-key"),
		WithBaseURL(baseURL),
		WithChatMode(),
		WithChatPath("/chat/completions"),
	}, opts...)
	return New(allOpts...)
}

// ============================================================================
// 非流式 HTTP 重试测试
// ============================================================================

func TestChat_Retry_OnServerError(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"server overloaded","type":"server_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockChatResponse("retry success")))
	}))
	defer srv.Close()

	client := newTestClient(srv.URL, WithRetry(retry.Config{
		MaxRetries:    3,
		FixedInterval: 10 * time.Millisecond,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.DoChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if !strings.Contains(resp.Choices[0].Message.ContentStr(), "retry success") {
		t.Errorf("unexpected response: %s", resp.Choices[0].Message.ContentStr())
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", atomic.LoadInt32(&attempts))
	}
	t.Logf("Succeeded after %d attempts", atomic.LoadInt32(&attempts))
}

func TestChat_Retry_Exhausted(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"message":"bad gateway","type":"server_error"}}`))
	}))
	defer srv.Close()

	client := newTestClient(srv.URL, WithRetry(retry.Config{
		MaxRetries:    2,
		FixedInterval: 10 * time.Millisecond,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.DoChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}

	// MaxRetries=2 → 3 total attempts
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", atomic.LoadInt32(&attempts))
	}
	t.Logf("Got expected error after %d attempts: %v", atomic.LoadInt32(&attempts), err)
}

func TestChat_Retry_OnRetry429(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockChatResponse("after rate limit")))
	}))
	defer srv.Close()

	var retryCount int32
	client := newTestClient(srv.URL, WithRetry(retry.Config{
		MaxRetries:    2,
		FixedInterval: 10 * time.Millisecond,
		OnRetry: func(attempt int, err error, wait time.Duration) {
			atomic.StoreInt32(&retryCount, 1)
			t.Logf("retry #%d, err=%v, wait=%v", attempt, err, wait)
		},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.DoChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("expected success after 429 retry, got: %v", err)
	}
	if atomic.LoadInt32(&retryCount) != 1 {
		t.Error("expected OnRetry to be called once")
	}
	if !strings.Contains(resp.Choices[0].Message.ContentStr(), "after rate limit") {
		t.Errorf("unexpected response: %s", resp.Choices[0].Message.ContentStr())
	}
}

// ============================================================================
// 流式看门狗测试
// ============================================================================

func TestChat_Stream_WatchdogTimeout_NoData(t *testing.T) {
	// 服务器：连接建立后不发任何数据，永久卡住
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		flusher.Flush()
		stallRequest(r, 10*time.Second)
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	var chunks int
	err := client.DoStreamChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	}, ChatStreamConfig{
		WatchdogTimeout: 200 * time.Millisecond,
	}, func(chunk ChatCompletionResponse) error {
		chunks++
		return nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected watchdog timeout error, got nil")
	}
	if !httputil.IsWatchdogTimeout(err) {
		t.Fatalf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}

	wdErr, ok := err.(*httputil.WatchdogTimeoutError)
	if !ok {
		t.Fatalf("expected *WatchdogTimeoutError, got %T", err)
	}
	if wdErr.ItemsReceived != 0 {
		t.Errorf("expected 0 items received, got %d", wdErr.ItemsReceived)
	}
	if elapsed > 2*time.Second {
		t.Errorf("watchdog should trigger within ~200ms, took %v", elapsed)
	}
	t.Logf("Watchdog timeout after %v, items=%d, bytes=%d",
		elapsed, wdErr.ItemsReceived, wdErr.BytesReceived)
}

func TestChat_Stream_WatchdogTimeout_WithPartialData(t *testing.T) {
	// 服务器：发一个事件后卡住
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		writeSSE(w, flusher, mockChatChunk("partial", ""))
		stallRequest(r, 10*time.Second)
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var chunks int
	var receivedText string
	err := client.DoStreamChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	}, ChatStreamConfig{
		WatchdogTimeout: 200 * time.Millisecond,
	}, func(chunk ChatCompletionResponse) error {
		chunks++
		for _, c := range chunk.Choices {
			receivedText += c.Delta.Content
		}
		return nil
	})

	if err == nil {
		t.Fatal("expected watchdog timeout error")
	}
	if !httputil.IsWatchdogTimeout(err) {
		t.Fatalf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}

	wdErr := err.(*httputil.WatchdogTimeoutError)
	if wdErr.ItemsReceived == 0 {
		t.Error("expected at least 1 item received before timeout")
	}
	if chunks == 0 {
		t.Error("expected to receive at least 1 chunk before timeout")
	}
	if receivedText != "partial" {
		t.Errorf("expected partial text, got %q", receivedText)
	}
	t.Logf("Received %d chunks, text=%q before timeout (%d items, %d bytes)",
		chunks, receivedText, wdErr.ItemsReceived, wdErr.BytesReceived)
}

// ============================================================================
// 流式看门狗 + 重试测试
// ============================================================================

func TestChat_Stream_RetryOnWatchdog_Success(t *testing.T) {
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		if count == 1 {
			// 第一次：连接后不发数据，卡住
			flusher.Flush()
			stallRequest(r, 10*time.Second)
			return
		}

		// 第二次：正常发送事件
		writeSSE(w, flusher, mockChatChunk("hello-from-retry", ""))
		writeSSE(w, flusher, mockChatChunk("", "stop"))
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	var retryCalled int32
	var chunks int
	var fullText string
	var finishReason string

	client := newTestClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := client.DoStreamChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	}, ChatStreamConfig{
		WatchdogTimeout: 150 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries:    1,
			FixedInterval: 50 * time.Millisecond,
			OnRetry: func(attempt int, err error, wait time.Duration) {
				atomic.StoreInt32(&retryCalled, 1)
				t.Logf("retry #%d triggered by: %v, waiting %v", attempt, err, wait)
				if !httputil.IsWatchdogTimeout(err) {
					t.Errorf("expected retry to be triggered by watchdog timeout, got: %T", err)
				}
			},
		},
	}, func(chunk ChatCompletionResponse) error {
		chunks++
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				fullText += c.Delta.Content
			}
			if c.FinishReason != "" {
				finishReason = c.FinishReason
			}
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if atomic.LoadInt32(&connections) != 2 {
		t.Errorf("expected 2 connections, got %d", atomic.LoadInt32(&connections))
	}
	if atomic.LoadInt32(&retryCalled) != 1 {
		t.Error("expected OnRetry callback to be called")
	}
	if fullText != "hello-from-retry" {
		t.Errorf("expected text 'hello-from-retry', got %q", fullText)
	}
	if finishReason != "stop" {
		t.Errorf("expected finish reason 'stop', got %q", finishReason)
	}
	t.Logf("Succeeded after retry: %d connections, %d chunks, text=%q",
		atomic.LoadInt32(&connections), chunks, fullText)
}

func TestChat_Stream_RetryOnWatchdog_Exhausted(t *testing.T) {
	var connections int32

	// 每次连接都卡住，不发数据
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		flusher.Flush()
		stallRequest(r, 10*time.Second)
	}))
	defer srv.Close()

	var retryCount int32
	client := newTestClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := client.DoStreamChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	}, ChatStreamConfig{
		WatchdogTimeout: 100 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries:    2,
			FixedInterval: 30 * time.Millisecond,
			OnRetry: func(attempt int, err error, wait time.Duration) {
				atomic.AddInt32(&retryCount, 1)
			},
		},
	}, func(chunk ChatCompletionResponse) error {
		return nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !httputil.IsWatchdogTimeout(err) {
		t.Fatalf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}
	// 1 initial + 2 retries = 3 connections
	if atomic.LoadInt32(&connections) != 3 {
		t.Errorf("expected 3 connections, got %d", atomic.LoadInt32(&connections))
	}
	if atomic.LoadInt32(&retryCount) != 2 {
		t.Errorf("expected 2 retry callbacks, got %d", atomic.LoadInt32(&retryCount))
	}
	// Total time: ~100ms * 3 + 30ms * 2 = ~360ms, plus overhead
	if elapsed > 3*time.Second {
		t.Errorf("retries should complete quickly, took %v", elapsed)
	}
	t.Logf("Retries exhausted after %d connections in %v", atomic.LoadInt32(&connections), elapsed)
}

func TestChat_Stream_Retry_DefaultPolicy_NoRetryWithData(t *testing.T) {
	// 默认策略：已收到数据时不重试（避免重复）
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		// 发一个事件后卡住
		writeSSE(w, flusher, mockChatChunk("partial-data", ""))
		stallRequest(r, 10*time.Second)
	}))
	defer srv.Close()

	var events []string
	client := newTestClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := client.DoStreamChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	}, ChatStreamConfig{
		WatchdogTimeout: 150 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries: 3,
		},
	}, func(chunk ChatCompletionResponse) error {
		for _, c := range chunk.Choices {
			events = append(events, c.Delta.Content)
		}
		return nil
	})

	// 默认策略：收到数据后不重试
	if err == nil {
		t.Fatal("expected WatchdogTimeoutError (not retried with data)")
	}
	if !httputil.IsWatchdogTimeout(err) {
		t.Fatalf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}
	if atomic.LoadInt32(&connections) != 1 {
		t.Errorf("expected 1 connection (no retry with data), got %d",
			atomic.LoadInt32(&connections))
	}
	if len(events) == 0 || events[0] != "partial-data" {
		t.Errorf("expected partial data, got %v", events)
	}
	t.Logf("Default policy: no retry with data, %d connections", atomic.LoadInt32(&connections))
}

func TestChat_Stream_Retry_CustomPolicy_RetryWithData(t *testing.T) {
	// 自定义策略：即使有数据也重试
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		if count <= 2 {
			// 前两次：发数据后卡住
			writeSSE(w, flusher, mockChatChunk(fmt.Sprintf("attempt-%d", count), ""))
			stallRequest(r, 10*time.Second)
		} else {
			// 第三次：正常完成
			writeSSE(w, flusher, mockChatChunk("final-success", ""))
			writeSSE(w, flusher, mockChatChunk("", "stop"))
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	}))
	defer srv.Close()

	var allText []string
	client := newTestClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := client.DoStreamChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	}, ChatStreamConfig{
		WatchdogTimeout: 150 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries:    3,
			FixedInterval: 30 * time.Millisecond,
			// 自定义策略：所有看门狗超时都重试
			ShouldRetry: func(attempt int, err error) bool {
				return httputil.IsWatchdogTimeout(err)
			},
		},
	}, func(chunk ChatCompletionResponse) error {
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				allText = append(allText, c.Delta.Content)
			}
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected success after custom-policy retries, got: %v", err)
	}
	if atomic.LoadInt32(&connections) != 3 {
		t.Errorf("expected 3 connections, got %d", atomic.LoadInt32(&connections))
	}
	// 前两次的 partial data 也会被收到（OnEvent 回调每次连接独立触发）
	joined := strings.Join(allText, "")
	if !strings.Contains(joined, "final-success") {
		t.Errorf("expected 'final-success' in response, got: %v", allText)
	}
	t.Logf("Custom policy retries: %d connections, received %v",
		atomic.LoadInt32(&connections), allText)
}

// ============================================================================
// 用户取消测试
// ============================================================================

func TestChat_Stream_NoRetryOnUserCancel(t *testing.T) {
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		flusher.Flush()
		stallRequest(r, 10*time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := newTestClient(srv.URL)

	// 100ms 后用户主动取消
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := client.DoStreamChatCompletion(ctx, ChatCompletionRequest{
		Model: "test-model",
		Messages: []ChatMessage{
			ChatUserMessage("hello"),
		},
	}, ChatStreamConfig{
		WatchdogTimeout: 10 * time.Second, // 长看门狗，确保不是看门狗触发的
		RetryConfig: &retry.Config{
			MaxRetries: 5,
		},
	}, func(chunk ChatCompletionResponse) error {
		return nil
	})

	if err == nil {
		t.Fatal("expected error on user cancel")
	}
	if httputil.IsWatchdogTimeout(err) {
		t.Error("should NOT be WatchdogTimeoutError for user cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %T: %v", err, err)
	}
	if atomic.LoadInt32(&connections) != 1 {
		t.Errorf("expected 1 connection (no retry on cancel), got %d",
			atomic.LoadInt32(&connections))
	}
	t.Logf("User cancel: no retry, %d connection", atomic.LoadInt32(&connections))
}

// ============================================================================
// DoStream (Provider 接口) 正常流式测试
// ============================================================================

func TestChat_DoStream_NormalStream(t *testing.T) {
	// 正常 SSE 响应
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		writeSSE(w, flusher, mockChatChunk("hello", ""))
		writeSSE(w, flusher, mockChatChunk(" world", ""))
		writeSSE(w, flusher, mockChatChunk("", "stop"))
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := client.DoStream(ctx, llm.GenerateParams{
		Model: &llm.Model{ID: "test-model"},
		Messages: []llm.Message{
			llm.UserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("DoStream setup failed: %v", err)
	}

	var (
		textDeltas int
		fullText   string
		gotFinish  bool
	)

	for part := range result.Stream {
		switch p := part.(type) {
		case *llm.TextDeltaPart:
			textDeltas++
			fullText += p.Text
		case *llm.FinishStepPart:
			t.Logf("[finish-step] reason=%s raw=%s", p.FinishReason, p.RawFinishReason)
		case *llm.FinishPart:
			gotFinish = true
			t.Logf("[finish] reason=%s", p.FinishReason)
		case *llm.ErrorPart:
			t.Fatalf("unexpected ErrorPart: %v", p.Error)
		}
	}

	if !gotFinish {
		t.Error("expected FinishPart in stream")
	}
	if textDeltas == 0 {
		t.Error("expected at least one text delta")
	}
	if fullText != "hello world" {
		t.Errorf("expected 'hello world', got %q", fullText)
	}
	t.Logf("DoStream normal: %d deltas, text=%q", textDeltas, fullText)
}

// ============================================================================
// 非流式 DoGenerate 重试测试（Provider 接口层）
// ============================================================================

func TestChat_DoGenerate_Retry(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockChatResponse("hello from retry")))
	}))
	defer srv.Close()

	client := newTestClient(srv.URL, WithRetry(retry.Config{
		MaxRetries:    2,
		FixedInterval: 10 * time.Millisecond,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := client.DoGenerate(ctx, llm.GenerateParams{
		Model: &llm.Model{ID: "test-model"},
		Messages: []llm.Message{
			llm.UserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("DoGenerate failed after retry: %v", err)
	}

	if !strings.Contains(result.Text, "hello from retry") {
		t.Errorf("unexpected text: %s", result.Text)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("expected 2 attempts, got %d", atomic.LoadInt32(&attempts))
	}
	t.Logf("DoGenerate succeeded after %d attempts: %s", atomic.LoadInt32(&attempts), result.Text)
}
