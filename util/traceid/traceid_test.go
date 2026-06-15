package traceid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kasuganosora/thinkbot/util/log"
)

// ============================================================================
// New
// ============================================================================

func TestNew_Format(t *testing.T) {
	id := New()
	if len(id) != IDLength*2 {
		t.Fatalf("expected %d chars, got %d: %q", IDLength*2, len(id), id)
	}
	if !IsValid(id) {
		t.Fatalf("generated ID is not valid hex: %q", id)
	}
}

func TestNew_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID after %d iterations: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

// ============================================================================
// Context 集成
// ============================================================================

func TestWithTraceID_FromContext(t *testing.T) {
	ctx := context.Background()
	id := New()
	ctx = WithTraceID(ctx, id)

	got := FromContext(ctx)
	if got != id {
		t.Fatalf("expected %q, got %q", id, got)
	}
}

func TestFromContext_Empty(t *testing.T) {
	got := FromContext(context.Background())
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestFromContext_OTelSpan(t *testing.T) {
	// 创建一个带有效 trace ID 的 span context
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	got := FromContext(ctx)
	expected := traceID.String()
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestFromContext_LocalOverridesOTel(t *testing.T) {
	// 本包注入的值优先于 OTel span
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	localID := New()
	ctx = WithTraceID(ctx, localID)

	got := FromContext(ctx)
	if got != localID {
		t.Fatalf("expected local ID %q, got %q", localID, got)
	}
}

func TestNewContext_GeneratesIfMissing(t *testing.T) {
	ctx := NewContext(context.Background())
	id := FromContext(ctx)
	if id == "" {
		t.Fatal("expected non-empty trace ID")
	}
	if !IsValid(id) {
		t.Fatalf("generated ID is not valid: %q", id)
	}
}

func TestNewContext_PreservesExisting(t *testing.T) {
	existing := New()
	ctx := WithTraceID(context.Background(), existing)
	ctx = NewContext(ctx) // 不应覆盖

	got := FromContext(ctx)
	if got != existing {
		t.Fatalf("expected %q, got %q", existing, got)
	}
}

// ============================================================================
// Logger 集成
// ============================================================================

func TestWithLogger_AddsTraceID(t *testing.T) {
	// 构建 observer core 捕获日志
	core, logs := observer.New(zapcore.InfoLevel)
	testLogger := zap.New(core).Sugar()

	id := New()
	ctx := WithTraceID(context.Background(), id)

	logger := WithLoggerFrom(ctx, testLogger)
	logger.Infow("test message", "key", "value")

	if logs.Len() != 1 {
		t.Fatalf("expected 1 log entry, got %d", logs.Len())
	}

	entry := logs.All()[0]
	found := false
	for _, f := range entry.ContextMap() {
		if f == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("trace_id %q not found in log context: %v", id, entry.ContextMap())
	}
}

func TestWithLogger_NoTraceID(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	testLogger := zap.New(core).Sugar()

	ctx := context.Background() // 无 trace ID
	logger := WithLoggerFrom(ctx, testLogger)
	logger.Infow("no trace")

	if logs.Len() != 1 {
		t.Fatalf("expected 1 log entry, got %d", logs.Len())
	}

	// 不应有 trace_id 字段
	entry := logs.All()[0]
	for k := range entry.ContextMap() {
		if k == LogField {
			t.Fatal("should not have trace_id field when none in context")
		}
	}
}

func TestWithLogger_NilLogger(t *testing.T) {
	ctx := WithTraceID(context.Background(), New())
	logger := WithLoggerFrom(ctx, nil)
	if logger != nil {
		t.Fatal("expected nil logger")
	}
}

func TestL_Shortcut(t *testing.T) {
	// 确保 L 不 panic（需要全局 Logger 已初始化）
	_ = log.Init()
	ctx := NewContext(context.Background())
	logger := L(ctx)
	if logger == nil {
		t.Fatal("L(ctx) returned nil")
	}
}

// ============================================================================
// HTTP Middleware
// ============================================================================

func TestMiddleware_GeneratesTraceID(t *testing.T) {
	var capturedID string
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 应自动生成 trace ID
	if capturedID == "" {
		t.Fatal("expected trace ID in context")
	}
	if !IsValid(capturedID) {
		t.Fatalf("invalid trace ID: %q", capturedID)
	}

	// 响应头也应有
	respID := rec.Header().Get(HeaderKey)
	if respID != capturedID {
		t.Fatalf("response header %q != context %q", respID, capturedID)
	}
}

func TestMiddleware_ReusesFromHeader(t *testing.T) {
	clientID := New()
	var capturedID string
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set(HeaderKey, clientID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID != clientID {
		t.Fatalf("expected %q, got %q", clientID, capturedID)
	}
}

func TestMiddleware_ReusesFromContext(t *testing.T) {
	existingID := New()
	var capturedID string
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	// 将 trace ID 预注入到 request context
	req = req.WithContext(WithTraceID(req.Context(), existingID))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID != existingID {
		t.Fatalf("expected %q, got %q", existingID, capturedID)
	}
}

func TestMiddlewareFunc(t *testing.T) {
	var called bool
	handler := MiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		id := FromContext(r.Context())
		if id == "" {
			t.Fatal("expected trace ID")
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("handler was not called")
	}
}

// ============================================================================
// IsValid / OrNew
// ============================================================================

func TestIsValid(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{New(), true},
		{"0102030405060708090a0b0c0d0e0f10", true},
		{"0102030405060708090A0B0C0D0E0F10", true}, // 大写也有效
		{"", false},
		{"too-short", false},
		{"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", false}, // 非 hex
		{"0102030405060708090a0b0c0d0e0f1", false},  // 31 字符
		{"0102030405060708090a0b0c0d0e0f100", false}, // 33 字符
	}

	for _, tt := range tests {
		got := IsValid(tt.input)
		if got != tt.valid {
			t.Errorf("IsValid(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

func TestOrNew_ValidID(t *testing.T) {
	id := New()
	got := OrNew(id)
	if got != id {
		t.Fatalf("expected same ID %q, got %q", id, got)
	}
}

func TestOrNew_InvalidID(t *testing.T) {
	got := OrNew("invalid")
	if !IsValid(got) {
		t.Fatalf("expected valid new ID, got %q", got)
	}
}

func TestOrNew_EmptyID(t *testing.T) {
	got := OrNew("")
	if !IsValid(got) {
		t.Fatalf("expected valid new ID, got %q", got)
	}
}

// ============================================================================
// HeaderKey 常量
// ============================================================================

func TestHeaderKey(t *testing.T) {
	// 确保 header key 是标准的 X- 前缀格式
	if !strings.HasPrefix(HeaderKey, "X-") {
		t.Fatalf("HeaderKey should start with X-, got %q", HeaderKey)
	}
}
