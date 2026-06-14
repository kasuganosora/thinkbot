package http

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/watchdog"
)

// ============================================================================
// 辅助
// ============================================================================

// stallServer 模拟服务器卡住：阻塞直到客户端断连（r.Context 取消）或 maxWait 超时。
// 用它替代 time.Sleep 可以避免 httptest.Server.Close() 因 handler 未退出而阻塞。
func stallServer(r *http.Request, maxWait time.Duration) {
	select {
	case <-r.Context().Done():
	case <-time.After(maxWait):
	}
}

func TestMain(m *testing.M) {
	// 初始化日志（仅 stdout，debug 级别）
	_ = log.InitWithConfig(log.Config{
		Level:   "debug",
		Outputs: []log.Output{log.Stdout()},
	})
	os.Exit(m.Run())
}

// ============================================================================
// 基础请求测试
// ============================================================================

func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	resp, err := c.Get("/test").Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := resp.JSON(&result); err != nil {
		t.Fatalf("json decode error: %v", err)
	}
	if result["status"] != "ok" {
		t.Errorf("expected status=ok, got %s", result["status"])
	}
}

func TestPostJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		w.Write([]byte(`{"received":true}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	body := map[string]any{"name": "test", "value": 42}
	resp, err := c.Post("/submit").SetJSONBody(body).Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := resp.JSON(&result); err != nil {
		t.Fatalf("json decode error: %v", err)
	}
	if result["received"] != true {
		t.Errorf("expected received=true, got %v", result["received"])
	}
}

func TestHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom") != "hello" {
			t.Errorf("missing X-Custom header")
		}
		if r.Header.Get("X-Default") != "def" {
			t.Errorf("missing X-Default header")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(
		WithBaseURL(srv.URL),
		WithHeader("X-Default", "def"),
	)
	resp, err := c.Get("/api").SetHeader("X-Custom", "hello").Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestQueryParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("foo") != "bar" {
			t.Errorf("expected foo=bar, got %s", r.URL.Query().Get("foo"))
		}
		if r.URL.Query().Get("page") != "1" {
			t.Errorf("expected page=1, got %s", r.URL.Query().Get("page"))
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	resp, err := c.Get("/search").
		SetQuery("foo", "bar").
		SetQuery("page", "1").
		Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestBearerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer mytoken123" {
			t.Errorf("expected Bearer mytoken123, got %s", auth)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	resp, err := c.Get("/protected").BearerToken("mytoken123").Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestErrorStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	resp, err := c.Get("/missing").Do()
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stallServer(r, 2*time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c := New(WithBaseURL(srv.URL))
	_, err := c.Get("/slow").SetContext(ctx).Do()
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// ============================================================================
// 重试测试
// ============================================================================

func TestRetry(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(
		WithBaseURL(srv.URL),
		WithRetry(retry.Config{
			MaxRetries:    3,
			FixedInterval: 10 * time.Millisecond,
		}),
	)
	resp, err := c.Get("/flaky").Do()
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", atomic.LoadInt32(&attempts))
	}
}

func TestRetryExhausted(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(
		WithBaseURL(srv.URL),
		WithRetry(retry.Config{
			MaxRetries:    2,
			FixedInterval: 10 * time.Millisecond,
		}),
	)
	_, err := c.Get("/always-fail").Do()
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	// MaxRetries=2 means total 3 attempts (1 initial + 2 retries)
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", atomic.LoadInt32(&attempts))
	}
}

// ============================================================================
// SSE 测试
// ============================================================================

func TestSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server doesn't support flushing")
		}

		events := []string{
			"event: ping\ndata: hello\n\n",
			"data: world\n\n",
			"event: done\ndata: finished\nid: 42\n\n",
		}
		for _, e := range events {
			fmt.Fprint(w, e)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	var events []SSEEvent
	c := New()

	err := c.Get(srv.URL+"/sse").DoSSE(SSEConfig{
		OnEvent: func(e SSEEvent) error {
			events = append(events, e)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// 第一个事件
	if events[0].Event != "ping" {
		t.Errorf("event[0] expected event=ping, got %s", events[0].Event)
	}
	if events[0].Data != "hello" {
		t.Errorf("event[0] expected data=hello, got %s", events[0].Data)
	}

	// 第二个事件（默认 message）
	if events[1].Event != "message" {
		t.Errorf("event[1] expected event=message, got %s", events[1].Event)
	}
	if events[1].Data != "world" {
		t.Errorf("event[1] expected data=world, got %s", events[1].Data)
	}

	// 第三个事件
	if events[2].Event != "done" {
		t.Errorf("event[2] expected event=done, got %s", events[2].Event)
	}
	if events[2].Data != "finished" {
		t.Errorf("event[2] expected data=finished, got %s", events[2].Data)
	}
	if events[2].ID != "42" {
		t.Errorf("event[2] expected id=42, got %s", events[2].ID)
	}
}

func TestSSEChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: item-%d\n\n", i)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New()
	ch, err := c.Get(srv.URL+"/sse-channel").DoSSEStream(SSEConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	for event := range ch {
		expected := fmt.Sprintf("item-%d", count)
		if event.Data != expected {
			t.Errorf("expected %s, got %s", expected, event.Data)
		}
		count++
	}
	if count != 5 {
		t.Errorf("expected 5 events, got %d", count)
	}
}

func TestSSEMultiLineData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		fmt.Fprint(w, "data: line1\ndata: line2\ndata: line3\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	var events []SSEEvent
	c := New()
	err := c.Get(srv.URL+"/sse-multiline").DoSSE(SSEConfig{
		OnEvent: func(e SSEEvent) error {
			events = append(events, e)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	expected := "line1\nline2\nline3"
	if events[0].Data != expected {
		t.Errorf("expected %q, got %q", expected, events[0].Data)
	}
}

func TestSSEWithWatchdogTimeout(t *testing.T) {
	// 服务器：发一个事件后永久卡住（不关闭连接）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		// 先发一个事件
		fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()

		// 然后永久卡住（10秒），不发送任何数据，也不关闭连接
		stallServer(r, 10*time.Second)
	}))
	defer srv.Close()

	var events []SSEEvent
	c := New()

	start := time.Now()
	err := c.Get(srv.URL + "/sse-watchdog").DoSSE(SSEConfig{
		WatchdogTimeout: 200 * time.Millisecond,
		OnEvent: func(e SSEEvent) error {
			events = append(events, e)
			return nil
		},
	})
	elapsed := time.Since(start)

	// 应该至少收到第一个事件
	if len(events) < 1 {
		t.Errorf("expected at least 1 event, got %d", len(events))
	}

	// 关键验证 1：应该返回 WatchdogTimeoutError（而非 nil）
	if err == nil {
		t.Fatal("expected WatchdogTimeoutError, got nil")
	}
	if !IsWatchdogTimeout(err) {
		t.Fatalf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}

	// 关键验证 2：通过 errors.Is 也能识别
	if !errors.Is(err, watchdog.ErrWatchdogTimeout) {
		t.Fatal("expected errors.Is(err, watchdog.ErrWatchdogTimeout)")
	}

	// 关键验证 3：错误包含正确的统计信息
	wdErr, ok := err.(*WatchdogTimeoutError)
	if !ok {
		t.Fatalf("expected *WatchdogTimeoutError, got %T", err)
	}
	if wdErr.ItemsReceived != 1 {
		t.Errorf("expected 1 item received, got %d", wdErr.ItemsReceived)
	}

	// 关键验证 4：看门狗应该在 ~200ms 后断连
	if elapsed > 1*time.Second {
		t.Errorf("watchdog should have disconnected within ~200ms, took %v", elapsed)
	}
	t.Logf("SSE watchdog: disconnected after %v, items=%d, bytes=%d",
		elapsed, wdErr.ItemsReceived, wdErr.BytesReceived)
}

// ============================================================================
// Stream 测试
// ============================================================================

func TestStreamChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "chunk-%d;", i)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	var chunks []string
	c := New()
	err := c.Get(srv.URL+"/stream").DoStream(StreamConfig{
		OnChunk: func(data []byte) error {
			chunks = append(chunks, string(data))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	combined := strings.Join(chunks, "")
	if !strings.Contains(combined, "chunk-4") {
		t.Errorf("expected to receive all chunks, got: %s", combined)
	}
}

func TestStreamLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "line-%d\n", i)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	var lines []string
	c := New()
	err := c.Get(srv.URL+"/stream-lines").DoStream(StreamConfig{
		LineMode: true,
		OnLine: func(line string) error {
			lines = append(lines, line)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
	if lines[0] != "line-0" {
		t.Errorf("expected line-0, got %s", lines[0])
	}
	if lines[4] != "line-4" {
		t.Errorf("expected line-4, got %s", lines[4])
	}
}

func TestStreamChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data-%d", i)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	ch, err := c.Get(srv.URL+"/stream-ch").DoStreamChunks(StreamConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var received []byte
	for data := range ch {
		received = append(received, data...)
	}

	combined := string(received)
	for i := 0; i < 3; i++ {
		expected := fmt.Sprintf("data-%d", i)
		if !strings.Contains(combined, expected) {
			t.Errorf("expected %s in stream, got: %s", expected, combined)
		}
	}
}

func TestStreamWithWatchdogTimeout(t *testing.T) {
	// 服务器：发一些数据后永久卡住（不关闭连接）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		// 发一些数据
		fmt.Fprint(w, "initial-data")
		flusher.Flush()

		// 然后永久卡住（10秒）
		stallServer(r, 10*time.Second)
	}))
	defer srv.Close()

	var received []byte
	c := New()

	start := time.Now()
	err := c.Get(srv.URL + "/stream-wd").DoStream(StreamConfig{
		WatchdogTimeout: 200 * time.Millisecond,
		OnChunk: func(data []byte) error {
			received = append(received, data...)
			return nil
		},
	})
	elapsed := time.Since(start)

	if len(received) == 0 {
		t.Error("expected to receive some data before watchdog timeout")
	}

	// 应该返回 WatchdogTimeoutError
	if err == nil {
		t.Fatal("expected WatchdogTimeoutError, got nil")
	}
	if !IsWatchdogTimeout(err) {
		t.Fatalf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}

	wdErr, ok := err.(*WatchdogTimeoutError)
	if !ok {
		t.Fatalf("expected *WatchdogTimeoutError, got %T", err)
	}
	if wdErr.BytesReceived == 0 {
		t.Error("expected bytes received > 0")
	}

	if elapsed > 1*time.Second {
		t.Errorf("watchdog should have disconnected within ~200ms, took %v", elapsed)
	}
	t.Logf("Stream watchdog: disconnected after %v, items=%d, bytes=%d",
		elapsed, wdErr.ItemsReceived, wdErr.BytesReceived)
}

func TestStreamLineChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		lines := []string{"alpha", "beta", "gamma"}
		for _, l := range lines {
			fmt.Fprintf(w, "%s\n", l)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	ch, err := c.Get(srv.URL+"/stream-line-ch").DoStreamLines(StreamConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var lines []string
	for line := range ch {
		lines = append(lines, line)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "alpha" || lines[1] != "beta" || lines[2] != "gamma" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

// ============================================================================
// 便捷方法测试
// ============================================================================

func TestGetJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"test","age":30}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))

	var result struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	err := c.GetJSON(context.Background(), "/user", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Name != "test" {
		t.Errorf("expected name=test, got %s", result.Name)
	}
	if result.Age != 30 {
		t.Errorf("expected age=30, got %d", result.Age)
	}
}

func TestPostJSONHelper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":99,"created":true}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))

	body := map[string]any{"name": "item"}
	var result struct {
		ID      int  `json:"id"`
		Created bool `json:"created"`
	}
	err := c.PostJSON(context.Background(), "/create", body, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ID != 99 {
		t.Errorf("expected id=99, got %d", result.ID)
	}
	if !result.Created {
		t.Error("expected created=true")
	}
}

// ============================================================================
// SSE 行解析测试
// ============================================================================

func TestSplitSSELine(t *testing.T) {
	tests := []struct {
		line     string
		field    string
		value    string
		ok       bool
	}{
		{"data: hello", "data", "hello", true},
		{"data:hello", "data", "hello", true},   // 无空格
		{"data:  hello", "data", " hello", true}, // 只去一个空格
		{"event: ping", "event", "ping", true},
		{":comment", "", "", false},
		{"nocolon", "nocolon", "", true},
		{"id: 42", "id", "42", true},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			field, value, ok := splitSSELine(tt.line)
			if ok != tt.ok {
				t.Errorf("expected ok=%v, got %v", tt.ok, ok)
			}
			if field != tt.field {
				t.Errorf("expected field=%q, got %q", tt.field, field)
			}
			if value != tt.value {
				t.Errorf("expected value=%q, got %q", tt.value, value)
			}
		})
	}
}

func TestIsRetryableCode(t *testing.T) {
	tests := []struct {
		code     int
		expected bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{0, true}, // 网络错误
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.code), func(t *testing.T) {
			result := isRetryableCode(tt.code)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestResponseIsSuccess(t *testing.T) {
	r := &Response{StatusCode: 200}
	if !r.IsSuccess() {
		t.Error("200 should be success")
	}

	r = &Response{StatusCode: 299}
	if !r.IsSuccess() {
		t.Error("299 should be success")
	}

	r = &Response{StatusCode: 300}
	if r.IsSuccess() {
		t.Error("300 should not be success")
	}

	r = &Response{StatusCode: 404}
	if r.IsSuccess() {
		t.Error("404 should not be success")
	}
}

func TestResponseString(t *testing.T) {
	r := &Response{Body: []byte("hello world")}
	if r.String() != "hello world" {
		t.Errorf("expected 'hello world', got %q", r.String())
	}
}

func TestTruncate(t *testing.T) {
	if truncate("abc", 5) != "abc" {
		t.Error("short string should not be truncated")
	}
	result := truncate("abcdefghij", 5)
	if result != "abcde..." {
		t.Errorf("expected 'abcde...', got %q", result)
	}
}

// ============================================================================
// 递归看门狗集成测试（使用外部 Watchdog）
// ============================================================================

func TestStreamWithExternalWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "tick-%d\n", i)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	wd := watchdog.New(context.Background(), 500*time.Millisecond)
	defer wd.Stop(true)

	var lines []string
	c := New()

	err := c.Get(srv.URL+"/stream-ext-wd").DoStream(StreamConfig{
		Watchdog: wd,
		LineMode: true,
		OnLine: func(line string) error {
			lines = append(lines, line)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
}

// ============================================================================
// 看门狗 vs 用户取消 区分测试
// ============================================================================

func TestSSEWatchdogTimeoutVsUserCancel(t *testing.T) {
	// 场景 1：看门狗超时 → 返回 WatchdogTimeoutError
	t.Run("watchdog_timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			stallServer(r, 10*time.Second) // 永久卡住
		}))
		defer srv.Close()

		c := New()
		err := c.Get(srv.URL).DoSSE(SSEConfig{
			WatchdogTimeout: 150 * time.Millisecond,
		})

		if err == nil {
			t.Fatal("expected error")
		}
		if !IsWatchdogTimeout(err) {
			t.Errorf("expected WatchdogTimeoutError, got %T: %v", err, err)
		}
	})

	// 场景 2：用户主动取消 → 返回 context.Canceled（不是 WatchdogTimeoutError）
	t.Run("user_cancel", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			stallServer(r, 10*time.Second) // 永久卡住
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		c := New()

		// 100ms 后主动取消
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()

		err := c.Get(srv.URL).SetContext(ctx).DoSSE(SSEConfig{
			WatchdogTimeout: 10 * time.Second, // 看门狗 10 秒，远长于用户取消
		})

		if err == nil {
			t.Fatal("expected error")
		}
		// 不应该是看门狗超时
		if IsWatchdogTimeout(err) {
			t.Error("should NOT be WatchdogTimeoutError for user cancel")
		}
		// 应该是 context.Canceled
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %T: %v", err, err)
		}
	})
}

func TestStreamWatchdogTimeoutVsUserCancel(t *testing.T) {
	t.Run("watchdog_timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			stallServer(r, 10*time.Second)
		}))
		defer srv.Close()

		c := New()
		err := c.Get(srv.URL).DoStream(StreamConfig{
			WatchdogTimeout: 150 * time.Millisecond,
		})

		if err == nil {
			t.Fatal("expected error")
		}
		if !IsWatchdogTimeout(err) {
			t.Errorf("expected WatchdogTimeoutError, got %T: %v", err, err)
		}
	})

	t.Run("user_cancel", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			stallServer(r, 10*time.Second)
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		c := New()

		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()

		err := c.Get(srv.URL).SetContext(ctx).DoStream(StreamConfig{
			WatchdogTimeout: 10 * time.Second,
		})

		if err == nil {
			t.Fatal("expected error")
		}
		if IsWatchdogTimeout(err) {
			t.Error("should NOT be WatchdogTimeoutError for user cancel")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %T: %v", err, err)
		}
	})
}

// ============================================================================
// 流式重试测试
// ============================================================================

func TestSSERetryOnWatchdogTimeout(t *testing.T) {
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		if count == 1 {
			// 第一次：连接后立即卡住，不发任何数据
			stallServer(r, 10*time.Second)
			return
		}

		// 第二次：正常发送事件
		fmt.Fprint(w, "data: hello-from-retry\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	var events []SSEEvent
	var retryCalled int32
	c := New()

	err := c.Get(srv.URL + "/sse-retry").DoSSE(SSEConfig{
		WatchdogTimeout: 150 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries:    1,
			FixedInterval: 50 * time.Millisecond,
			OnRetry: func(attempt int, err error, wait time.Duration) {
				atomic.StoreInt32(&retryCalled, 1)
				if attempt != 1 {
					t.Errorf("expected attempt=1, got %d", attempt)
				}
				wdErr, ok := err.(*WatchdogTimeoutError)
				if !ok || wdErr.ItemsReceived != 0 {
					t.Errorf("expected WatchdogTimeoutError with 0 items, got %T: %v", err, err)
				}
			},
		},
		OnEvent: func(e SSEEvent) error {
			events = append(events, e)
			return nil
		},
	})

	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if atomic.LoadInt32(&connections) != 2 {
		t.Errorf("expected 2 connections, got %d", atomic.LoadInt32(&connections))
	}
	if atomic.LoadInt32(&retryCalled) != 1 {
		t.Error("expected OnRetry to be called")
	}
	if len(events) != 1 || events[0].Data != "hello-from-retry" {
		t.Errorf("unexpected events: %v", events)
	}
}

func TestSSERetryNotTriggeredForUserCancel(t *testing.T) {
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		stallServer(r, 10*time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := c.Get(srv.URL).SetContext(ctx).DoSSE(SSEConfig{
		WatchdogTimeout: 10 * time.Second, // 长看门狗
		RetryConfig: &retry.Config{
			MaxRetries: 5,
		},
	})

	// 用户取消 → 不重试，返回 context.Canceled
	if err == nil {
		t.Fatal("expected error")
	}
	if IsWatchdogTimeout(err) {
		t.Error("should NOT be WatchdogTimeoutError")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %T: %v", err, err)
	}
	if atomic.LoadInt32(&connections) != 1 {
		t.Errorf("expected 1 connection (no retry), got %d", atomic.LoadInt32(&connections))
	}
}

func TestSSERetryDefaultPolicyNoRetryWithData(t *testing.T) {
	// 默认策略：如果已收到数据，不重试（避免重复）
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		// 发一个事件后卡住
		fmt.Fprint(w, "data: partial\n\n")
		flusher.Flush()
		stallServer(r, 10*time.Second)
	}))
	defer srv.Close()

	var events []SSEEvent
	c := New()

	err := c.Get(srv.URL).DoSSE(SSEConfig{
		WatchdogTimeout: 150 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries: 3,
		},
		OnEvent: func(e SSEEvent) error {
			events = append(events, e)
			return nil
		},
	})

	// 应该返回 WatchdogTimeoutError，而不是重试
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsWatchdogTimeout(err) {
		t.Errorf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}
	// 只连接了 1 次（默认策略：有数据不重试）
	if atomic.LoadInt32(&connections) != 1 {
		t.Errorf("expected 1 connection (no retry with data), got %d", atomic.LoadInt32(&connections))
	}
}

func TestSSERetryWithCustomPolicy(t *testing.T) {
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		// 每次都发一个事件后卡住
		fmt.Fprintf(w, "data: attempt-%d\n\n", count)
		flusher.Flush()
		stallServer(r, 10*time.Second)
	}))
	defer srv.Close()

	var events []SSEEvent
	c := New()

	err := c.Get(srv.URL).DoSSE(SSEConfig{
		WatchdogTimeout: 150 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries:    2,
			FixedInterval: 50 * time.Millisecond,
			// 自定义策略：即使有数据也重试
			ShouldRetry: func(attempt int, err error) bool {
				return true
			},
		},
		OnEvent: func(e SSEEvent) error {
			events = append(events, e)
			return nil
		},
	})

	// 重试耗尽后返回 WatchdogTimeoutError
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsWatchdogTimeout(err) {
		t.Errorf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}
	// 1 initial + 2 retries = 3 connections
	if atomic.LoadInt32(&connections) != 3 {
		t.Errorf("expected 3 connections, got %d", atomic.LoadInt32(&connections))
	}
}

func TestStreamRetryOnWatchdogTimeout(t *testing.T) {
	var connections int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&connections, 1)
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		if count == 1 {
			// 第一次卡住
			stallServer(r, 10*time.Second)
			return
		}
		// 第二次正常
		fmt.Fprint(w, "retry-success")
		flusher.Flush()
	}))
	defer srv.Close()

	var received []byte
	c := New()

	err := c.Get(srv.URL).DoStream(StreamConfig{
		WatchdogTimeout: 150 * time.Millisecond,
		RetryConfig: &retry.Config{
			MaxRetries:    1,
			FixedInterval: 50 * time.Millisecond,
		},
		OnChunk: func(data []byte) error {
			received = append(received, data...)
			return nil
		},
	})

	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if atomic.LoadInt32(&connections) != 2 {
		t.Errorf("expected 2 connections, got %d", atomic.LoadInt32(&connections))
	}
	if !strings.Contains(string(received), "retry-success") {
		t.Errorf("expected 'retry-success', got %s", string(received))
	}
}

func TestWatchdogTimedOutMethod(t *testing.T) {
	wd := watchdog.New(context.Background(), 100*time.Millisecond)
	defer wd.Stop(true)

	if wd.TimedOut() {
		t.Error("should not be timed out before timeout")
	}

	time.Sleep(200 * time.Millisecond)

	if !wd.TimedOut() {
		t.Error("should be timed out after timeout period")
	}
}

func TestWatchdogTimedOutNotOnUserCancel(t *testing.T) {
	wd := watchdog.New(context.Background(), 10*time.Second)
	defer wd.Stop(true)

	// 主动取消（不是超时）
	wd.Stop(true)

	if wd.TimedOut() {
		t.Error("should NOT be timed out on user cancel")
	}
}

// ============================================================================
// BasicAuth 测试
// ============================================================================

func TestBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("expected Basic auth prefix, got %s", auth)
		}
		// 验证 base64 编码正确
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err != nil {
			t.Fatalf("failed to decode base64: %v", err)
		}
		if string(decoded) != "user:pass" {
			t.Errorf("expected 'user:pass', got %q", string(decoded))
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	resp, err := c.Get("/auth").BasicAuth("user", "pass").Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// ============================================================================
// Dump 测试
// ============================================================================

func TestDump(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response-Header", "resp-val")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// 不影响功能，只验证 dump 不 panic 且返回正确
	c := New(WithBaseURL(srv.URL))
	resp, err := c.Post("/dump").SetJSONBody(map[string]string{"key": "value"}).Dump().Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]bool
	if err := resp.JSON(&result); err != nil {
		t.Fatalf("json decode error: %v", err)
	}
	if !result["ok"] {
		t.Error("expected ok=true")
	}
}

// ============================================================================
// per-request SetRetry 测试
// ============================================================================

func TestSetRetryPerRequest(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Client 无 retry 配置，请求级覆盖
	c := New(WithBaseURL(srv.URL))
	resp, err := c.Get("/flaky").SetRetry(retry.Config{
		MaxRetries:    3,
		FixedInterval: 10 * time.Millisecond,
	}).Do()
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", atomic.LoadInt32(&attempts))
	}
}

// ============================================================================
// MaxBodySize 测试
// ============================================================================

func TestMaxBodySize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 返回 100KB 数据
		data := strings.Repeat("x", 100*1024)
		w.Write([]byte(data))
	}))
	defer srv.Close()

	c := New(
		WithBaseURL(srv.URL),
		WithMaxBodySize(1024), // 1KB limit
	)
	resp, err := c.Get("/big").Do()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Body) != 1024 {
		t.Errorf("expected body to be truncated to 1024 bytes, got %d", len(resp.Body))
	}
}

// ============================================================================
// Channel 错误传递测试
// ============================================================================

func TestStreamChunksWithErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New()
	ch, errCh := c.Get(srv.URL+"/error").DoStreamChunksWithErr(StreamConfig{})

	for data := range ch {
		_ = data
	}

	err := <-errCh
	if err == nil {
		t.Fatal("expected error from error channel")
	}
}

func TestSSEStreamWithErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: item-%d\n\n", i)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New()
	ch, errCh := c.Get(srv.URL+"/sse-err").DoSSEStreamWithErr(SSEConfig{})

	var count int
	for event := range ch {
		_ = event
		count++
	}
	err := <-errCh
	if err != nil {
		t.Errorf("expected nil error on normal end, got %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 events, got %d", count)
	}
}

// ============================================================================
// DefaultStreamShouldRetry with wrapped error 测试
// ============================================================================

func TestDefaultStreamShouldRetryWithWrappedError(t *testing.T) {
	// 构建一个被包装的 WatchdogTimeoutError
	original := &WatchdogTimeoutError{
		URL:           "http://example.com",
		ItemsReceived: 0,
		BytesReceived: 0,
		Elapsed:       100 * time.Millisecond,
		WatchdogName:  "test-wd",
	}
	wrapped := fmt.Errorf("outer error: %w", original)

	// errors.As 应该能穿透包装
	if !DefaultStreamShouldRetry(1, wrapped) {
		t.Error("expected DefaultStreamShouldRetry to return true for wrapped WatchdogTimeoutError with 0 items")
	}

	// 有数据的看门狗超时不应该重试
	withData := &WatchdogTimeoutError{
		URL:           "http://example.com",
		ItemsReceived: 5,
		BytesReceived: 100,
		Elapsed:       100 * time.Millisecond,
		WatchdogName:  "test-wd",
	}
	wrappedWithData := fmt.Errorf("outer: %w", withData)
	if DefaultStreamShouldRetry(1, wrappedWithData) {
		t.Error("expected false for wrapped error with items received > 0")
	}

	// 非看门狗错误不应该重试
	plain := fmt.Errorf("some other error")
	if DefaultStreamShouldRetry(1, plain) {
		t.Error("expected false for non-watchdog error")
	}
}
