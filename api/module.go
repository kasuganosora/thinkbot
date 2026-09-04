package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"

	"go.opentelemetry.io/otel/metric"
	noop_metric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/identity"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/skill"
	"github.com/kasuganosora/thinkbot/stats"
	"github.com/kasuganosora/thinkbot/toolperm"
)

// ============================================================================
// fx Module — API 依赖注入
// ============================================================================

// APIParams 是创建 API 组件所需的依赖。
type APIParams struct {
	fx.In

	DB            *gorm.DB
	Store         *config.Store
	AuthSvc       *auth.AuthService
	BotMgr        *bot.BotManager
	Logger        *zap.SugaredLogger
	TP            trace.TracerProvider `optional:"true"`
	MP            metric.MeterProvider `optional:"true"`
	StatsRecorder llm.UsageRecorder    `optional:"true"`
	// JudgeRecorder LLM 快判结果落库器。未提供时判定结果不落库（改动前行为）。
	JudgeRecorder *stats.JudgeRecorder `optional:"true"`
	Lifecycle     fx.Lifecycle
}

// Module 是 API 的 fx 模块。
var Module = fx.Module("api",
	fx.Provide(
		newEventBus,
		newCookieManager,
		newBotService,
		newChatHistoryService,
		newWorkflowService,
		newSkillManager,
		newToolPermService,
		newAPIServer,
	),
	fx.Invoke(registerAPILifecycle),
)

// newEventBus 创建内存事件总线。
func newEventBus(logger *zap.SugaredLogger) outbound.EventBus {
	return outbound.NewMemoryEventBus(outbound.DefaultMemoryEventBusConfig(), logger)
}

// newCookieManager 创建 CookieManager。
// JWT secret 优先从 config store 读取（含 DB / .env / 环境变量）。
// 如果不存在，生成随机 secret 并持久化到 DB，保证重启后 cookie 仍然有效。
// Secure 标志从 config store 读取。
//
// 因为 config store 的 Migrate/Reload 在 OnStart 中执行，
// 所以此处也在 OnStart 中初始化 secret，确保 DB 表已就绪。
func newCookieManager(lc fx.Lifecycle, store *config.Store) *CookieManager {
	m := &CookieManager{}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			secret := store.GetString("auth.jwt_secret", "")
			if secret == "" {
				b := make([]byte, 32)
				if _, err := rand.Read(b); err != nil {
					return err
				}
				secret = hex.EncodeToString(b)
				if err := store.Set(ctx, "auth.jwt_secret", secret); err != nil {
					return err
				}
			}
			secure := store.GetBool(config.KeyAPICookieSecure, false)
			m.secret = []byte(secret)
			m.secure = secure
			return nil
		},
	})

	return m
}

// newBotService 创建 BotService。
func newBotService(p APIParams, eventBus outbound.EventBus, chatHistory *ChatHistoryService, permSvc *toolperm.Service, bindStage *identity.BindStage) *BotService {
	tp := p.TP
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	mp := p.MP
	if mp == nil {
		mp = noop_metric.NewMeterProvider()
	}
	// JudgeRecorder 可为 nil（fx 未提供）——此时不挂 sink，判定结果不落库。
	var judgeSink engagement.JudgeRecordSink
	if p.JudgeRecorder != nil {
		judgeSink = stats.NewJudgeSink(p.JudgeRecorder)
	}
	return NewBotService(p.DB, p.Store, p.BotMgr, p.Logger, tp, mp, eventBus,
		p.StatsRecorder, judgeSink, chatHistory, permSvc, bindStage)
}

// newToolPermService 创建 bot 工具权限服务。
func newToolPermService(p APIParams, logger *zap.SugaredLogger) *toolperm.Service {
	return toolperm.NewService(p.DB, logger)
}

// newChatHistoryService 创建聊天历史服务。
//
// 在 OnStart 中把遗留的 streaming=true 行收敛为 false：这些是上次进程被中断时留下的
// 流式中间态，不可能再继续产出；不清理的话，前端会把其中 status="running" 的工具卡片
// 渲染成永久转圈。服务启动是唯一能可靠判定「这些流已经死了」的时机。
//
// 必须放在 OnStart 而非构造函数：dao.Migrate 同样在启动阶段执行，构造期访问 DB 会因
// 表/列尚未创建而失败（曾出现 "no such column: streaming"，清理静默不生效）。
func newChatHistoryService(lc fx.Lifecycle, db *gorm.DB, logger *zap.SugaredLogger) *ChatHistoryService {
	s := NewChatHistoryService(db, logger)

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// 清理失败不阻断启动：最坏情况只是少数历史卡片仍显示 running。
			if n, err := s.MarkStreamingStale(); err != nil {
				s.logger.Warnw("failed to clear stale streaming flags", "err", err)
			} else if n > 0 {
				s.logger.Infow("cleared stale streaming assistant messages", "count", n)
			}
			return nil
		},
	})

	return s
}

// newWorkflowService 创建工作流服务。
func newWorkflowService(db *gorm.DB, store *config.Store, tp trace.TracerProvider, bus outbound.EventBus, logger *zap.SugaredLogger, botSvc *BotService) *WorkflowService {
	return NewWorkflowService(db, store, tp, bus, logger, botSvc)
}

// newSkillManager 创建技能管理器（全局，从 skills/ 目录加载）。
func newSkillManager(store *config.Store, logger *zap.SugaredLogger) *skill.SkillManager {
	mgr := skill.NewSkillManager(nil, skill.NewConfigStoreAdapter(store), logger)

	// 尝试从文件系统加载技能
	skillsDir := filepath.Join("skills")
	loader := skill.NewLoader(skillsDir, logger)
	if count, err := loader.LoadAndRegister(mgr); err != nil {
		logger.Warnw("api: failed to load skills", "dir", skillsDir, "err", err)
	} else if count > 0 {
		logger.Infow("api: skills loaded", "dir", skillsDir, "count", count)
	}

	return mgr
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
	workflowSvc *WorkflowService,
	skillMgr *skill.SkillManager,
	bindSvc *identity.BindService,
	permSvc *toolperm.Service,
) *Server {
	return NewServer(authSvc, botSvc, cookie, chatHistory, store, db, logger, workflowSvc, skillMgr, bindSvc, permSvc)
}

// registerAPILifecycle 绑定 Server 和 BotService 的生命周期。
func registerAPILifecycle(p APIParams, server *Server, botSvc *BotService, wfSvc *WorkflowService, skillMgr *skill.SkillManager) {
	// 独立的 context，不依赖 FX OnStart 的短生命周期 context
	srvCtx, srvCancel := context.WithCancel(context.Background())

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// 启动所有定义中 status=running 的 Bot
			if err := botSvc.StartAll(ctx); err != nil {
				p.Logger.Warnw("api: failed to start bots from DB", "err", err)
			}

			// 恢复中断的工作流
			if result, err := wfSvc.Recover(ctx); err != nil {
				p.Logger.Warnw("api: workflow recovery failed", "err", err)
			} else if result != nil && result.Total > 0 {
				p.Logger.Infow("api: workflows recovered",
					"total", result.Total, "resumed", result.Resumed, "reanalyzed", result.Reanalyzed)
			}

			// 启动卡死工作流看门狗（进程内卡死回收，与 Recover 互补）
			wfSvc.StartSweeper(srvCtx)

			// 启动配额续跑看门狗（配额熔断的工作流到点自动恢复执行）
			wfSvc.StartQuotaWatch(srvCtx)

			// 在后台启动 HTTP Server（使用独立 context）
			go func() {
				if err := server.Start(srvCtx); err != nil {
					p.Logger.Errorw("api: server error", "err", err)
				}
			}()

			p.Logger.Infow("api server initialized", "addr", server.addr)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Logger.Infow("api server shutting down")
			// 取消 HTTP Server 的 context，触发优雅关闭
			srvCancel()
			// 保存技能启用状态
			skillMgr.SaveEnabledStates(ctx)
			// 关闭工作流引擎
			wfSvc.Close()
			return nil
		},
	})
}
