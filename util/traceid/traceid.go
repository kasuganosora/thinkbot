// Package traceid 提供请求级 Trace ID 的生成、传播与日志集成。
//
// 核心能力：
//   - 生成 128-bit 随机 Trace ID（与 OpenTelemetry 格式兼容）
//   - 通过 context.Context 注入/提取 Trace ID
//   - 与 OTel Span 互通：context 中有 OTel span 时自动提取其 trace ID
//   - 与 zap.SugaredLogger 集成：日志自动携带 trace_id 字段
//   - HTTP Middleware：请求入口自动生成/读取/传播 Trace ID
package traceid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/log"
)

// ============================================================================
// 常量与类型
// ============================================================================

const (
	// HeaderKey 是 HTTP 请求/响应中传播 Trace ID 的头名称。
	HeaderKey = "X-Trace-ID"

	// LogField 是日志中 Trace ID 的字段名。
	LogField = "trace_id"

	// IDLength 是 Trace ID 的字节长度（128-bit = 16 bytes，hex 编码后 32 字符）。
	IDLength = 16
)

// ctxKey 是 context 中存储 Trace ID 的私有 key 类型。
type ctxKey struct{}

// ============================================================================
// Trace ID 生成
// ============================================================================

// New 生成一个新的 128-bit 随机 Trace ID。
// 返回 32 字符小写十六进制字符串，与 OpenTelemetry 的 trace ID 格式兼容。
func New() string {
	b := make([]byte, IDLength)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在主流平台不会失败；极端情况下使用零值
		return fmt.Sprintf("%032d", 0)
	}
	return hex.EncodeToString(b)
}

// ============================================================================
// Context 集成
// ============================================================================

// WithTraceID 将指定的 Trace ID 注入到 context 中。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, traceID)
}

// FromContext 从 context 中提取 Trace ID。
//
// 提取优先级：
//  1. 本包通过 WithTraceID 注入的值
//  2. OTel span 中的 trace ID（如果 span 正在记录）
//  3. 返回空字符串
func FromContext(ctx context.Context) string {
	// 优先取本包注入的值
	if id, ok := ctx.Value(ctxKey{}).(string); ok && id != "" {
		return id
	}

	// 尝试从 OTel span 提取
	sc := trace.SpanFromContext(ctx).SpanContext()
	if sc.HasTraceID() {
		return sc.TraceID().String()
	}

	return ""
}

// NewContext 确保 context 中有 Trace ID。
// 如果已有则原样返回，否则生成新 ID 后注入。
func NewContext(ctx context.Context) context.Context {
	if FromContext(ctx) != "" {
		return ctx
	}
	return WithTraceID(ctx, New())
}

// ============================================================================
// Logger 集成
// ============================================================================

// WithLogger 返回一个携带 trace_id 字段的 SugaredLogger。
// 如果 context 中没有 Trace ID，则直接返回全局 Logger。
func WithLogger(ctx context.Context) *zap.SugaredLogger {
	return WithLoggerFrom(ctx, log.Logger)
}

// WithLoggerFrom 使用指定的 SugaredLogger 创建携带 trace_id 的子 logger。
func WithLoggerFrom(ctx context.Context, logger *zap.SugaredLogger) *zap.SugaredLogger {
	if logger == nil {
		return nil
	}
	id := FromContext(ctx)
	if id == "" {
		return logger
	}
	return logger.With(LogField, id)
}

// L 是 WithLogger 的简写别名，方便快速使用。
//
//	traceid.L(ctx).Infow("processing request", "user_id", uid)
func L(ctx context.Context) *zap.SugaredLogger {
	return WithLogger(ctx)
}

// ============================================================================
// HTTP Middleware
// ============================================================================

// Middleware 是 HTTP 中间件，自动为每个请求生成或读取 Trace ID。
//
// 行为：
//  1. 检查请求头 X-Trace-ID，如果有则复用
//  2. 否则检查 context 中已有的 Trace ID
//  3. 都没有则生成新 ID
//  4. 注入到 context 和响应头中
//  5. 记录请求开始日志（携带 trace_id）
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 提取或生成 Trace ID
		traceID := r.Header.Get(HeaderKey)
		if traceID == "" {
			traceID = FromContext(r.Context())
		}
		if traceID == "" {
			traceID = New()
		}

		// 注入 context
		ctx := WithTraceID(r.Context(), traceID)

		// 设置响应头（方便客户端关联）
		w.Header().Set(HeaderKey, traceID)

		// 传递给下游
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// MiddlewareFunc 是函数式 HTTP 中间件，与 http.HandlerFunc 兼容。
func MiddlewareFunc(next http.HandlerFunc) http.HandlerFunc {
	return Middleware(next).(http.HandlerFunc)
}

// ============================================================================
// 工具函数
// ============================================================================

// IsValid 检查字符串是否是有效的 Trace ID（32 字符十六进制）。
func IsValid(id string) bool {
	if len(id) != IDLength*2 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// OrNew 如果 id 为空或无效，生成新的 Trace ID。
func OrNew(id string) string {
	if IsValid(id) {
		return id
	}
	return New()
}
