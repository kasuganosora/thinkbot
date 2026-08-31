package api

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/subagent"
	"github.com/kasuganosora/thinkbot/workflow"
)

// ============================================================================
// WorkflowService — 工作流管理服务（懒初始化）
//
// 工作流引擎需要 LLM Provider + SubAgentManager，这些依赖在 API 启动时
// 可能尚未就绪（需要先配置 Bot LLM）。因此采用懒初始化策略：
// 首次调用 API 时从 BotService 获取 LLM Provider 并创建 workflow.Manager。
// ============================================================================

// WorkflowService 管理工作流引擎的生命周期。
type WorkflowService struct {
	db     *gorm.DB
	store  *config.Store
	tp     trace.TracerProvider
	bus    outbound.EventBus
	logger *zap.SugaredLogger
	botSvc *BotService

	mu    sync.Mutex
	mgr   *workflow.Manager
	saMgr *subagent.SubAgentManager
}

// NewWorkflowService 创建工作流服务。
func NewWorkflowService(db *gorm.DB, store *config.Store, tp trace.TracerProvider, bus outbound.EventBus, logger *zap.SugaredLogger, botSvc *BotService) *WorkflowService {
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	return &WorkflowService{
		db:     db,
		store:  store,
		tp:     tp,
		bus:    bus,
		logger: logger.With("component", "workflow_service"),
		botSvc: botSvc,
	}
}

// Manager 返回工作流管理器（懒初始化）。
//
// **优先复用 BotService 已装配工作区工具的引擎**，只有在没有任何 bot 启动时
// 才退化为自建实例。
//
// 为什么这个优先级至关重要（2026-08-06 线上事故）：本服务负责 Recover /
// Sweeper / UI 重试。自建实例的 WireConfig 没有 ToolMgr，工作流内部 SubAgent
// 因此碰不到工作区 —— 进程重启后 Recover 接管工作流，节点产出从 5000~10000 字的
// 真实审查报告退化成 48~117 字的「我将先探索项目结构…」纯计划，
// 且照样通过 review 被判 completed，等于静默把工作成果丢掉。
func (ws *WorkflowService) Manager() (*workflow.Manager, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.mgr != nil {
		return ws.mgr, nil
	}

	// 优先复用 bot 侧已装配工具的引擎。
	// **刻意不缓存到 ws.mgr**：bot 可能重启或切换模型，每次取当前最新的那个，
	// 避免长期持有一个已被 StopBot 关闭的引擎。
	if shared := ws.botSvc.WorkflowEngine(""); shared != nil {
		return shared, nil
	}

	// 从 BotService 获取 LLM Provider（含当前主模型定义，用于推导分析器 max_tokens）
	provider, model, modelDef, err := ws.botSvc.CreateLLMProvider()
	if err != nil {
		return nil, err
	}

	// 退化路径：没有任何 bot 启动。此时引擎拿不到工作区工具，
	// 代码/文件类节点只能产出计划——workflow.Setup 内部已就此打 WARN。
	mgr, saMgr := workflow.Setup(workflow.WireConfig{
		Provider:       provider,
		Model:          model,
		DB:             ws.db,
		Logger:         ws.logger,
		TracerProvider: ws.tp,
		Store:          ws.store,
		ModelDef:       modelDef,
		EventBus:       ws.bus,
	})

	ws.mgr = mgr
	ws.saMgr = saMgr
	ws.logger.Warnw("workflow engine initialized without workspace tools",
		"model", model,
		"reason", "no started bot exposed an equipped engine",
		"impact", "code/file nodes will only produce plans, not actual results")

	return mgr, nil
}

// Close 关闭工作流引擎。
func (ws *WorkflowService) Close() {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.saMgr != nil {
		ws.saMgr.CloseAll()
	}
}

// Recover 恢复中断的工作流。
func (ws *WorkflowService) Recover(ctx context.Context) (*workflow.RecoveryResult, error) {
	mgr, err := ws.Manager()
	if err != nil {
		return nil, err
	}
	return mgr.Recover(ctx)
}

// StartSweeper 启动卡死工作流看门狗（进程级）。引擎懒初始化，故每次 tick 时取管理器；
// 若引擎尚未就绪（未配置 LLM）则跳过本轮。应在服务启动时调用一次。
func (ws *WorkflowService) StartSweeper(ctx context.Context) {
	ws.logger.Infow("workflow stuck-watchdog starting", "interval", "2m")
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mgr, err := ws.Manager()
				if err != nil {
					// 引擎尚未初始化（未配置 LLM），下一轮再试。
					continue
				}
				mgr.SweepStale(ctx)
			}
		}
	}()
}

// StartQuotaWatch 启动配额续跑看门狗（进程级）。委派给底层 workflow.Manager，
// 引擎未就绪时跳过本轮。应在服务启动时调用一次。
func (ws *WorkflowService) StartQuotaWatch(ctx context.Context) {
	ws.logger.Infow("workflow quota-watchdog starting", "interval", "30s")
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mgr, err := ws.Manager()
				if err != nil {
					// 引擎尚未初始化（未配置 LLM），下一轮再试。
					continue
				}
				mgr.ResumeQuotaInterrupted(ctx)
			}
		}
	}()
}
