package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// 流式 429 限流重试测试
//
// 背景：GLM/智谱在高负载时会对流式请求返回 429（"访问量过大"），
// 该错误发生在连接建立阶段（首字节前），重试整条流是安全的。
// 修复前：doStreamChat 传空 ChatStreamConfig{}，429 直接失败，对话被打断。
// 修复后：client.streamRetryConfig() 把 retryCfg 透传给 SSE 层，
// 且 StreamShouldRetry 能识别 *StreamHTTPError 的 429 状态码。
// ============================================================================

// TestChat_Stream_RetryOn429_Success 验证：流式请求前 2 次返回 429，
// 第 3 次成功 → 客户端自动重试并完整收到内容。
func TestChat_Stream_RetryOn429_Success(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count <= 2 {
			w.Header().Set("Retry-After-MS", "10") // 服务端建议 10ms 后重试
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"1305","message":"当前 API 请求访问量过大"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, mockChatChunk("recovered", ""))
		writeSSE(w, flusher, mockChatChunk("", "stop"))
		writeSSE(w, flusher, "[DONE]")
	}))
	defer srv.Close()

	// 与 llm_factory 相同的构造方式：WithRetry 传入通用 LLM 重试配置，
	// doStreamChat 内部会经 streamRetryConfig() 换成流式感知的分类器。
	cfg := retry.LLMRetryConfig(3)
	cfg.Backoff.Initial = 10 * time.Millisecond // 测试提速
	cfg.Backoff.Max = 50 * time.Millisecond
	client := newTestClient(srv.URL, WithRetry(cfg))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := client.DoStream(ctx, llm.GenerateParams{
		Model:    &llm.Model{ID: "test-model"},
		Messages: []llm.Message{llm.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("DoStream setup failed: %v", err)
	}

	var text string
	var streamErr error
	for part := range result.Stream {
		switch p := part.(type) {
		case *llm.TextDeltaPart:
			text += p.Text
		case *llm.ErrorPart:
			streamErr = p.Error
		}
	}

	if streamErr != nil {
		t.Fatalf("expected success after 429 retries, got stream error: %v", streamErr)
	}
	if text != "recovered" {
		t.Errorf("expected text %q, got %q", "recovered", text)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected exactly 3 attempts (2x429 + 1 success), got %d", got)
	}
}

// TestChat_Stream_429_NoRetryWithoutConfig 验证：未配置 WithRetry 时，
// 429 不重试、直接以错误结束（回归保护：确认重试由配置驱动而非隐式行为）。
func TestChat_Stream_429_NoRetryWithoutConfig(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"1305","message":"当前 API 请求访问量过大"}}`))
	}))
	defer srv.Close()

	client := newTestClient(srv.URL) // 无 WithRetry

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := client.DoStream(ctx, llm.GenerateParams{
		Model:    &llm.Model{ID: "test-model"},
		Messages: []llm.Message{llm.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("DoStream setup failed: %v", err)
	}

	var streamErr error
	for part := range result.Stream {
		if p, ok := part.(*llm.ErrorPart); ok {
			streamErr = p.Error
		}
	}

	if streamErr == nil {
		t.Fatal("expected 429 error to surface without retry config")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt (no retry), got %d", got)
	}
}

// TestChat_Stream_429_NonRetryableStatus 验证：4xx 客户端错误（401）不重试，
// 避免对认证失败等不可恢复错误浪费重试预算。
func TestChat_Stream_429_NonRetryableStatus(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	cfg := retry.LLMRetryConfig(3)
	cfg.Backoff.Initial = 10 * time.Millisecond
	client := newTestClient(srv.URL, WithRetry(cfg))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := client.DoStream(ctx, llm.GenerateParams{
		Model:    &llm.Model{ID: "test-model"},
		Messages: []llm.Message{llm.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("DoStream setup failed: %v", err)
	}

	var streamErr error
	for part := range result.Stream {
		if p, ok := part.(*llm.ErrorPart); ok {
			streamErr = p.Error
		}
	}

	if streamErr == nil {
		t.Fatal("expected 401 error to surface")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt for non-retryable 401, got %d", got)
	}
}
