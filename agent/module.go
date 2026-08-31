package agent

import (
	"context"
	"fmt"
	"os"

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

// Module 是 Agent 的单 Bot 模式 fx 模块（向后兼容）。
// 适用于只有一个 Bot 的简单场景。
//
// 对于多 Bot 场景，请使用 bot.Module + bot.BotManager。
//
// 单 Bot 模式：
//
//	app := fx.New(
//	    agent.Module,
//	    fx.Provide(zap.NewDevelopment),
//	    fx.Provide(func(l *zap.Logger) *zap.SugaredLogger { return l.Sugar() }),
//	    pipeline.ProvideStage(stages.NewLoggerStage, 10),
//	    pipeline.ProvideStage(stages.NewLLMStage, 100),
//	    fx.Invoke(func(ingress *inbound.Ingress) {
//	        ingress.Receive(ctx, msg)
//	    }),
//	)
//
// 多 Bot 模式：
//
//	app := fx.New(
//	    bot.Module,
//	    fx.Invoke(func(mgr *bot.BotManager, logger *zap.SugaredLogger, tp trace.TracerProvider) {
//	        botA, _ := bot.New(bot.BotParams{
//	            ID:         "customer-service",
//	            Config:     bot.BotConfig{SystemPrompt: "你是客服"},
//	            Pipeline:   pipelineA,
//	            Dispatcher: dispatcherA,
//	            Channels:   []bot.Channel{misskeyChA, telegramChA},
//	            Logger:     logger,
//	            TP:         tp,
//	        })
//	        mgr.Register(botA)
//	    }),
//	)
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
		OnStart: func(_ context.Context) error {
			go func() {
				// 使用独立 context，生命周期由 OnStop 的 engine.Stop() 控制，
				// 而非绑定到 fx OnStart 的短生命周期 ctx。
				if err := engine.Run(context.Background()); err != nil {
					fmt.Fprintf(os.Stderr, "agent: engine run failed: %v\n", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			engine.Stop()
			return nil
		},
	})
}
