package pipeline

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// ObservableStage — 自动事件发射的 Stage 包装器
// ============================================================================

// ObservableStage 包装一个 Stage，在进入和退出时自动通过 EventEmitter
// 发射 stage.enter / stage.exit 事件。
//
// 使用方式：
//
//	// Pipeline 构造时，将需要可观测的 Stage 包装：
//	wrapped := pipeline.NewObservableStage(myStage)
//
// 或者在 Pipeline 初始化时批量包装所有 Stage：
//
//	pipeline.WrapWithObservability(stages)
//
// EventEmitter 通过 context 传递（由 Bot 在 processEnvelope 中注入），
// 如果 context 中没有 emitter，ObservableStage 静默退化为直接调用内部 Stage。
type ObservableStage struct {
	inner core.Stage
}

// NewObservableStage 创建可观测 Stage 包装器。
func NewObservableStage(inner core.Stage) *ObservableStage {
	return &ObservableStage{inner: inner}
}

// Name 返回内部 Stage 的名称。
func (s *ObservableStage) Name() string {
	return s.inner.Name()
}

// Process 在调用内部 Stage 前后发射事件。
func (s *ObservableStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	emitter := outbound.EmitterFromContext(ctx)
	traceID := traceid.FromContext(ctx)
	stageName := s.inner.Name()

	// 发射 stage.enter 事件
	emitter.EmitStageEnter(ctx, traceID, stageName)

	start := time.Now()
	result, err := s.inner.Process(ctx, env)
	duration := time.Since(start)

	// 发射 stage.exit 事件（含耗时和错误信息）
	emitter.EmitStageExit(ctx, traceID, stageName, duration, err)

	// 对于 SkipError 也发射一个 stage.skip 事件
	if err != nil && core.IsSkipError(err) {
		emitter.EmitStage(ctx, outbound.EventStageSkip, traceID, stageName, map[string]any{
			"reason": err.Error(),
		})
	}

	return result, err
}

// Inner 返回内部原始 Stage（用于测试或检查）。
func (s *ObservableStage) Inner() core.Stage {
	return s.inner
}

// WrapWithObservability 批量将 StageInfo 中的 Stage 包装为 ObservableStage。
// 已经是 ObservableStage 的不重复包装。
func WrapWithObservability(stages []core.StageInfo) []core.StageInfo {
	wrapped := make([]core.StageInfo, len(stages))
	for i, si := range stages {
		wrapped[i] = si
		if _, ok := si.Stage.(*ObservableStage); !ok {
			wrapped[i].Stage = NewObservableStage(si.Stage)
		}
	}
	return wrapped
}
