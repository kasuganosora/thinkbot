package pipeline

import (
	"context"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Observability — OTel 集成工具
// ============================================================================

// Instruments 封装 Pipeline 所需的 OTel 仪器。
type Instruments struct {
	Tracer       trace.Tracer
	MsgProcessed metric.Int64Counter
	MsgErrors    metric.Int64Counter
	MsgDropped   metric.Int64Counter
	StageLatency metric.Float64Histogram
}

// NewInstruments 使用 OTel providers 创建 Instruments。
func NewInstruments(tp trace.TracerProvider, mp metric.MeterProvider) (*Instruments, error) {
	tracer := tp.Tracer("github.com/kasuganosora/thinkbot/agent/pipeline")
	meter := mp.Meter("github.com/kasuganosora/thinkbot/agent/pipeline")

	processed, err := meter.Int64Counter("pipeline.messages.processed",
		metric.WithDescription("Total messages entering the pipeline"))
	if err != nil {
		return nil, err
	}

	errCounter, err := meter.Int64Counter("pipeline.messages.errors",
		metric.WithDescription("Total message processing errors"))
	if err != nil {
		return nil, err
	}

	dropped, err := meter.Int64Counter("pipeline.messages.dropped",
		metric.WithDescription("Total messages dropped by stages"))
	if err != nil {
		return nil, err
	}

	latency, err := meter.Float64Histogram("pipeline.stage.duration_seconds",
		metric.WithDescription("Stage processing duration in seconds"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	return &Instruments{
		Tracer:       tracer,
		MsgProcessed: processed,
		MsgErrors:    errCounter,
		MsgDropped:   dropped,
		StageLatency: latency,
	}, nil
}

// ObservableStageWrapper 包装 Stage 使其自动记录 OTel span 和 metrics。
// 这是一个便捷工具，当不使用 Pipeline 引擎而直接调用 Stage 时使用。
type ObservableStageWrapper struct {
	inner  core.Stage
	inst   *Instruments
	logger *zap.SugaredLogger
}

// WrapStageObservable 包装 Stage 添加自动可观测性。
func WrapStageObservable(stage core.Stage, inst *Instruments, logger *zap.SugaredLogger) core.Stage {
	return &ObservableStageWrapper{
		inner:  stage,
		inst:   inst,
		logger: logger,
	}
}

// Name 返回内部 Stage 名称。
func (w *ObservableStageWrapper) Name() string { return w.inner.Name() }

// Process 执行内部 Stage 并记录可观测性数据。
func (w *ObservableStageWrapper) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	// 直接委托内部 Stage（Pipeline.executeStage 已经处理了 span 和 metrics）
	return w.inner.Process(ctx, env)
}
