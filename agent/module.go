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
// 组装 Pipeline + Inbound(Ingress) + Outbound + Engine，
// 并通过 Lifecycle hooks 管理 Engine 启停。
//
// 各 channel 输入端通过 fx 注入 *inbound.Ingress（或 *Engine）获取消息入口，
// 调用 ingress.Receive(ctx, msg) 即可将消息送入 Pipeline。
//
// 使用示例：
//
//	app := fx.New(
//	    agent.Module,
//	    fx.Provide(zap.NewDevelopment),
//	    fx.Provide(func(l *zap.Logger) *zap.SugaredLogger { return l.Sugar() }),
//	    pipeline.ProvideStage(stages.NewLoggerStage, 10),
//	    pipeline.ProvideStage(stages.NewLLMStage, 100),
//	    // channel 输入端通过注入 *inbound.Ingress 来发送消息
//	    fx.Invoke(func(ingress *inbound.Ingress) {
//	        // 启动你的 webhook server / ws 连接 / polling loop
//	        // 在收到消息时调用 ingress.Receive(ctx, msg)
//	    }),
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

	// 提供 Engine
	fx.Provide(newEngine),

	// 注册 Engine 生命周期
	fx.Invoke(registerEngineLifecycle),
)

// newEngine 是 fx 可注入的 Engine 构造函数。
func newEngine(
	ingress *inbound.Ingress,
	p *pipeline.Pipeline,
	d outbound.Dispatcher,
	cfg EngineConfig,
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
) *Engine {
	return NewEngine(ingress, p, d, cfg, logger, tp)
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
