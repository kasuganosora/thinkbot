package api

import (
	"context"

	"go.opentelemetry.io/otel/metric"
	noop_metric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
)

// ============================================================================
// fx Module — API 依赖注入
// ============================================================================

// APIParams 是创建 API 组件所需的依赖。
type APIParams struct {
	fx.In

	DB        *gorm.DB
	Store     *config.Store
	AuthSvc   *auth.AuthService
	BotMgr    *bot.BotManager
	Logger    *zap.SugaredLogger
	TP        trace.TracerProvider `optional:"true"`
	MP        metric.MeterProvider `optional:"true"`
	Lifecycle fx.Lifecycle
}

// Module 是 API 的 fx 模块。
var Module = fx.Module("api",
	fx.Provide(
		newEventBus,
		newCookieManager,
		newBotService,
		newChatHistoryService,
		newAPIServer,
	),
	fx.Invoke(registerAPILifecycle),
)

// newEventBus 创建内存事件总线。
func newEventBus(logger *zap.SugaredLogger) outbound.EventBus {
	return outbound.NewMemoryEventBus(outbound.DefaultMemoryEventBusConfig(), logger)
}

// newCookieManager 创建 CookieManager。
// JWT secret 和 Secure 标志从 config store 读取。
func newCookieManager(store *config.Store) *CookieManager {
	secret := store.GetString("auth.jwt_secret", "")
	secure := store.GetBool(config.KeyAPICookieSecure, false)
	return NewCookieManager(secret, secure)
}

// newBotService 创建 BotService。
func newBotService(p APIParams, eventBus outbound.EventBus) *BotService {
	tp := p.TP
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	mp := p.MP
	if mp == nil {
		mp = noop_metric.NewMeterProvider()
	}
	return NewBotService(p.DB, p.Store, p.BotMgr, p.Logger, tp, mp, eventBus)
}

// newChatHistoryService 创建聊天历史服务。
func newChatHistoryService(db *gorm.DB, logger *zap.SugaredLogger) *ChatHistoryService {
	return NewChatHistoryService(db, logger)
}

// newAPIServer 创建 Gin API Server。
func newAPIServer(
	authSvc *auth.AuthService,
	botSvc *BotService,
	cookie *CookieManager,
	chatHistory *ChatHistoryService,
	store *config.Store,
	db *gorm.DB,
	logger *zap.SugaredLogger,
) *Server {
	return NewServer(authSvc, botSvc, cookie, chatHistory, store, db, logger)
}

// registerAPILifecycle 绑定 Server 和 BotService 的生命周期。
func registerAPILifecycle(p APIParams, server *Server, botSvc *BotService) {
	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// 启动所有定义中 status=running 的 Bot
			if err := botSvc.StartAll(ctx); err != nil {
				p.Logger.Warnw("api: failed to start bots from DB", "err", err)
			}

			// 在后台启动 HTTP Server
			go func() {
				if err := server.Start(ctx); err != nil {
					p.Logger.Errorw("api: server error", "err", err)
				}
			}()

			p.Logger.Infow("api server initialized", "addr", server.addr)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Logger.Infow("api server shutting down")
			return nil
		},
	})
}
