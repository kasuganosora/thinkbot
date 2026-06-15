package pipeline

import (
	"go.opentelemetry.io/otel/metric"
	noop_metric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// fx Module — Pipeline 依赖注入
// ============================================================================

// Module 是 Pipeline 的 fx 模块。
// 它从 "pipeline_stages" 分组收集所有 StageInfo，
// 并使用 OTel providers 和 logger 构建 Pipeline。
var Module = fx.Module("pipeline",
	// 提供 Pipeline 构造器
	fx.Provide(
		fx.Annotate(
			newPipeline,
			fx.ParamTags(`group:"pipeline_stages"`),
		),
	),
	// 提供默认 OTel NoOp providers（如果上层没有提供）
	fx.Supply(
		fx.Annotate(noop_trace.NewTracerProvider(), fx.As(new(trace.TracerProvider))),
		fx.Annotate(noop_metric.NewMeterProvider(), fx.As(new(metric.MeterProvider))),
	),
)

// newPipeline 是 fx 可注入的 Pipeline 构造器。
func newPipeline(
	stages []core.StageInfo,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	logger *zap.SugaredLogger,
) (*Pipeline, error) {
	return New(stages, tp, mp, logger)
}

// ProvideStage 将 Stage 构造器注册到 fx 的 "pipeline_stages" 分组。
//
// 用法：
//
//	fx.Options(
//	    pipeline.ProvideStage(stages.NewLoggerStage, 10),  // order=10, 靠前执行
//	    pipeline.ProvideStage(stages.NewLLMStage, 100),    // order=100, 靠后执行
//	)
func ProvideStage(constructor any, order int) fx.Option {
	return fx.Provide(
		fx.Annotate(
			constructor,
			fx.ResultTags(`group:"pipeline_stages"`),
		),
		fx.Decorate(func(s core.Stage) core.StageInfo {
			return core.StageInfo{
				Stage:   s,
				Order:   order,
				Enabled: true,
			}
		}),
	)
}

// ProvideStageInfo 直接将 StageInfo 构造器注册到 fx 分组。
// 适用于需要自定义 Order 和 Enabled 的场景。
func ProvideStageInfo(constructor any) fx.Option {
	return fx.Provide(
		fx.Annotate(
			constructor,
			fx.ResultTags(`group:"pipeline_stages"`),
		),
	)
}
