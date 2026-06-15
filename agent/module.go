package agent

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
)

// ============================================================================
// fx Module — 顶层 Agent 依赖注入
// ============================================================================

// Module 是 Agent 的顶层 fx 模块。
// 组装 Pipeline + Inbound + Outbound + Engine，并通过 Lifecycle hooks 管理启停。
//
// 使用示例：
//
//	app := fx.New(
//	    agent.Module,
//	    fx.Provide(zap.NewDevelopment),        // logger
//	    fx.Provide(func(l *zap.Logger) *zap.SugaredLogger { return l.Sugar() }),
//	    pipeline.ProvideStage(stages.NewLoggerStage, 10),
//	    pipeline.ProvideStage(stages.NewLLMStage, 100),
//	)
//	app.Run()
var Module = fx.Module("agent",
	// 引入子模块
	pipeline.Module,
	inbound.Module,
	outbound.Module,

	// 提供默认 EngineConfig
	fx.Provide(func() EngineConfig {
		return DefaultEngineConfig()
	}),

	// 提供 Engine 构造器
	fx.Provide(
		fx.Annotate(
			newEngine,
			fx.ParamTags(`group:"inbound_sources"`),
		),
	),

	// 注册 Engine 生命周期
	fx.Invoke(registerEngineLifecycle),
)

// newEngine 是 fx 可注入的 Engine 构造函数。
func newEngine(
	sources []inbound.Source,
	p *pipeline.Pipeline,
	d outbound.Dispatcher,
	cfg EngineConfig,
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
) *Engine {
	return NewEngine(sources, p, d, cfg, logger, tp)
}

// registerEngineLifecycle 将 Engine 的启停绑定到 fx.Lifecycle。
func registerEngineLifecycle(lc fx.Lifecycle, engine *Engine) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go engine.Run(ctx)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			engine.Stop()
			return nil
		},
	})
}
