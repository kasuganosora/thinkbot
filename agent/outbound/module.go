package outbound

import (
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// ============================================================================
// fx Module
// ============================================================================

// Module 是 outbound 子系统的 fx 模块。
var Module = fx.Module("outbound",
	fx.Provide(
		fx.Annotate(
			func(logger *zap.SugaredLogger, tp trace.TracerProvider) Dispatcher {
				return NewLogDispatcher(logger, tp)
			},
		),
	),
	// 如果上层没有提供 TracerProvider，使用 NoOp
	fx.Supply(
		fx.Annotate(noop_trace.NewTracerProvider(), fx.As(new(trace.TracerProvider))),
	),
)
