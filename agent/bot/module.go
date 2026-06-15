package bot

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"
)

// ============================================================================
// fx Module — Bot 子系统依赖注入
// ============================================================================

// Module 是 Bot 子系统的 fx 模块。
// 它提供 BotManager 并注册 Lifecycle hooks 来管理所有 Bot 的启停。
//
// 使用方式：
//
//	app := fx.New(
//	    bot.Module,
//	    pipeline.Module,    // 共享 OTel + Pipeline 基础设施
//	    outbound.Module,
//	    fx.Provide(zap.NewDevelopment),
//	    fx.Provide(func(l *zap.Logger) *zap.SugaredLogger { return l.Sugar() }),
//	    fx.Invoke(func(mgr *BotManager) {
//	        // 注册你的 Bot
//	        mgr.Register(myBot)
//	    }),
//	)
var Module = fx.Module("bot",
	// 默认提供 NoOp TracerProvider（如果上层未提供）
	fx.Supply(
		fx.Annotate(noop_trace.NewTracerProvider(), fx.As(new(trace.TracerProvider))),
	),

	// 提供 BotManager
	fx.Provide(NewBotManager),

	// 注册生命周期：OnStart 启动所有 Bot，OnStop 停止所有 Bot
	fx.Invoke(registerBotManagerLifecycle),
)

// registerBotManagerLifecycle 将 BotManager 的启停绑定到 fx.Lifecycle。
func registerBotManagerLifecycle(lc fx.Lifecycle, mgr *BotManager) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			return mgr.RunAll(ctx)
		},
		OnStop: func(ctx context.Context) error {
			mgr.StopAll()
			return nil
		},
	})
}

// ProvideBot 是 fx 辅助函数，用于注册一个 Bot 构造器。
// 返回的 Bot 会在 fx 容器启动时被自动创建并注册到 BotManager。
//
// 用法：
//
//	fx.New(
//	    bot.Module,
//	    bot.ProvideBot(func(mgr *BotManager, logger *zap.SugaredLogger, tp trace.TracerProvider) error {
//	        b, _ := bot.New(bot.BotParams{
//	            ID:       "my-bot",
//	            Pipeline: myPipeline,
//	            Dispatcher: myDispatcher,
//	            Channels: []bot.Channel{myChannel},
//	            Logger:   logger,
//	            TP:       tp,
//	        })
//	        return mgr.Register(b)
//	    }),
//	)
func ProvideBot(registerFn any) fx.Option {
	return fx.Invoke(registerFn)
}
