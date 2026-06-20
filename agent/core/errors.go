package core

import (
	"errors"
	"fmt"
)

// ============================================================================
// Pipeline 专用错误类型
// ============================================================================

// PipelineError 表示在某个 Stage 中发生的错误。
type PipelineError struct {
	// Stage 出错的 Stage 名称。
	Stage string
	// Message 错误描述。
	Message string
	// Cause 原始错误。
	Cause error
}

func (e *PipelineError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("pipeline stage %q: %s: %v", e.Stage, e.Message, e.Cause)
	}
	return fmt.Sprintf("pipeline stage %q: %s", e.Stage, e.Message)
}

func (e *PipelineError) Unwrap() error { return e.Cause }

// ============================================================================
// AbortError — 立即中止 Pipeline
// ============================================================================

// AbortError 表示 Pipeline 应立即停止执行所有后续 Stage。
// Stage 返回此错误时，Pipeline 将停止并将此错误传播给调用者。
type AbortError struct {
	Reason string
	Cause  error
}

func (e *AbortError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("pipeline aborted: %s: %v", e.Reason, e.Cause)
	}
	return fmt.Sprintf("pipeline aborted: %s", e.Reason)
}

func (e *AbortError) Unwrap() error { return e.Cause }

// IsAbortError 判断错误是否是 AbortError（包括被 fmt.Errorf("%w") 包装的）。
func IsAbortError(err error) bool {
	var target *AbortError
	return errors.As(err, &target)
}

// ============================================================================
// SkipError — 跳过后续处理
// ============================================================================

// SkipError 表示当前 Stage 的处理应被跳过，Pipeline 继续执行下一个 Stage。
// 这不是一个真正的错误，而是一种控制流信号。
type SkipError struct {
	Reason string
}

func (e *SkipError) Error() string {
	return fmt.Sprintf("stage skipped: %s", e.Reason)
}

// IsSkipError 判断错误是否是 SkipError（包括被 fmt.Errorf("%w") 包装的）。
func IsSkipError(err error) bool {
	var target *SkipError
	return errors.As(err, &target)
}
