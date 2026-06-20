package errs

import (
	"context"
	"fmt"
	"net/http"
	"runtime"

	"github.com/kasuganosora/thinkbot/util/log"
)

// ============================================================================
// 核心 Error 类型
// ============================================================================

// Error 是项目统一错误类型，包含消息、原因、HTTP 状态码、调用堆栈。
type Error struct {
	message string     // 错误描述
	cause   error      // 原始错误（可为 nil）
	code    int        // HTTP 状态码（0 表示未设置）
	stack   *stack     // 调用堆栈
	context []ctxField // 结构化上下文字段
}

type ctxField struct {
	key   string
	value any
}

// --- error 接口 ---

func (e *Error) Error() string {
	if e.cause != nil {
		return e.message + ": " + e.cause.Error()
	}
	return e.message
}

// Unwrap 支持 errors.Is / errors.As。
func (e *Error) Unwrap() error {
	return e.cause
}

// --- 访问器 ---

// Code 返回 HTTP 状态码（未设置时为 0）。
func (e *Error) Code() int {
	return e.code
}

// StackTrace 返回格式化的堆栈信息。
func (e *Error) StackTrace() string {
	if e.stack == nil {
		return ""
	}
	return e.stack.String()
}

// Context 返回结构化上下文字段的副本（防止调用方修改影响内部状态）。
func (e *Error) Context() []ctxField {
	out := make([]ctxField, len(e.context))
	copy(out, e.context)
	return out
}

// With 添加结构化上下文字段，返回新实例（不改原对象）。
func (e *Error) With(key string, value any) *Error {
	cloned := e.clone()
	cloned.context = append(cloned.context, ctxField{key, value})
	return cloned
}

// WithCode 设置/覆盖 HTTP 状态码，返回新实例。
func (e *Error) WithCode(code int) *Error {
	cloned := e.clone()
	cloned.code = code
	return cloned
}

// clone 深拷贝。
func (e *Error) clone() *Error {
	ctx := make([]ctxField, len(e.context))
	copy(ctx, e.context)
	return &Error{
		message: e.message,
		cause:   e.cause,
		code:    e.code,
		stack:   e.stack,
		context: ctx,
	}
}

// ============================================================================
// 堆栈
// ============================================================================

// frame 单个堆栈帧。
type frame struct {
	function string
	file     string
	line     int
}

// stack 堆栈帧集合。
type stack struct {
	frames []frame
}

// String 格式化堆栈为可读字符串。
func (s *stack) String() string {
	if s == nil || len(s.frames) == 0 {
		return ""
	}
	var b []byte
	for i, f := range s.frames {
		b = fmt.Appendf(b, "#%-2d %s\n    %s:%d\n", i, f.function, f.file, f.line)
	}
	return string(b)
}

// captureStack 捕获当前调用堆栈，跳过 skip 层。
func captureStack(skip int, depth int) *stack {
	if depth <= 0 {
		depth = 32
	}
	pcs := make([]uintptr, depth)
	n := runtime.Callers(skip+2, pcs) // +2 跳过 runtime.Callers 和 captureStack 自身
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])

	var result []frame
	for {
		f, more := frames.Next()
		// 跳过标准库和 runtime
		if f.Function != "" && f.File != "" {
			result = append(result, frame{
				function: f.Function,
				file:     f.File,
				line:     f.Line,
			})
		}
		if !more {
			break
		}
		if len(result) >= depth {
			break
		}
	}

	if len(result) == 0 {
		return nil
	}
	return &stack{frames: result}
}

// ============================================================================
// 构造函数
// ============================================================================

// New 创建一个带堆栈的新错误。
func New(message string) *Error {
	return &Error{
		message: message,
		stack:   captureStack(1, 32),
	}
}

// Newf 创建带格式化消息的新错误。
func Newf(format string, args ...any) *Error {
	return &Error{
		message: fmt.Sprintf(format, args...),
		stack:   captureStack(1, 32),
	}
}

// Wrap 包装一个已有错误并附带消息和堆栈。
// 如果 err 为 nil 返回 nil。
func Wrap(err error, message string) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		message: message,
		cause:   err,
		stack:   captureStack(1, 32),
	}
}

// Wrapf 包装错误并附带格式化消息。
func Wrapf(err error, format string, args ...any) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		message: fmt.Sprintf(format, args...),
		cause:   err,
		stack:   captureStack(1, 32),
	}
}

// ============================================================================
// HTTP 错误
// ============================================================================

// HTTPError 包装 HTTP 状态码的便捷构造。
func HTTPError(code int, message string) *Error {
	if message == "" {
		message = http.StatusText(code)
	}
	return &Error{
		message: message,
		code:    code,
		stack:   captureStack(1, 32),
	}
}

// HTTPErrorf 包装 HTTP 状态码的便捷构造（格式化消息）。
func HTTPErrorf(code int, format string, args ...any) *Error {
	return &Error{
		message: fmt.Sprintf(format, args...),
		code:    code,
		stack:   captureStack(1, 32),
	}
}

// 常用 HTTP 错误快捷构造。

func BadRequest(message string) *Error   { return HTTPError(http.StatusBadRequest, message) }
func Unauthorized(message string) *Error { return HTTPError(http.StatusUnauthorized, message) }
func Forbidden(message string) *Error    { return HTTPError(http.StatusForbidden, message) }
func NotFound(message string) *Error     { return HTTPError(http.StatusNotFound, message) }
func Conflict(message string) *Error     { return HTTPError(http.StatusConflict, message) }
func Internal(msg string) *Error         { return HTTPError(http.StatusInternalServerError, msg) }
func ServiceUnavailable(message string) *Error {
	return HTTPError(http.StatusServiceUnavailable, message)
}

// ============================================================================
// 辅助函数
// ============================================================================

// Cause 提取最内层原始错误（兼容标准库和 pkg/errors）。
func Cause(err error) error {
	for err != nil {
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		inner := u.Unwrap()
		if inner == nil {
			break
		}
		err = inner
	}
	return err
}

// GetCode 从错误链中提取第一个非零 HTTP 状态码，未找到返回 0。
func GetCode(err error) int {
	current := err
	for current != nil {
		if e, ok := current.(*Error); ok && e.code != 0 {
			return e.code
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := current.(unwrapper)
		if !ok {
			break
		}
		current = u.Unwrap()
	}
	return 0
}

// GetStackTrace 从错误链中提取堆栈字符串。
func GetStackTrace(err error) string {
	var e *Error
	if As(err, &e) {
		return e.StackTrace()
	}
	return ""
}

// Is / As 代理标准库（保持包级别便捷使用）。

// Is 判断错误链中是否包含 target。
func Is(err, target error) bool { return errorIs(err, target) }

// As 将错误链中第一个匹配的类型赋值到 target。
func As(err error, target any) bool { return errorAs(err, target) }

// ============================================================================
// 日志集成
// ============================================================================

// Log 将错误以适当的日志级别输出。
//   - HTTP 4xx → Warn
//   - HTTP 5xx / 无状态码 → Error
func Log(err error) {
	if err == nil {
		return
	}
	LogWith(context.Background(), err)
}

// LogWith 与 Log 相同，但接受额外 context 字段。
func LogWith(ctx context.Context, err error, fields ...any) {
	if err == nil {
		return
	}

	var e *Error
	extraArgs := []any{"err", err.Error()}
	if As(err, &e) {
		extraArgs = append(extraArgs, "code", e.code)
		if stack := e.StackTrace(); stack != "" {
			extraArgs = append(extraArgs, "stack", stack)
		}
		for _, f := range e.context {
			extraArgs = append(extraArgs, f.key, f.value)
		}
	}
	extraArgs = append(extraArgs, fields...)

	code := GetCode(err)
	switch {
	case code >= 400 && code < 500:
		log.Logger.Warnw("http client error", extraArgs...)
	case code >= 500:
		log.Logger.Errorw("http server error", extraArgs...)
	default:
		log.Logger.Errorw("error", extraArgs...)
	}
}

// LogAndReturn 记录日志后返回原错误，便于链式调用。
func LogAndReturn(err error) error {
	Log(err)
	return err
}
