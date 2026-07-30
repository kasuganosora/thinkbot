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
func (ws *WorkflowService) Manager() (*workflow.Manager, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.mgr != nil {
		return ws.mgr, nil
	}

	// 从 BotService 获取 LLM Provider（含当前主模型定义，用于推导分析器 max_tokens）
	provider, model, modelDef, err := ws.botSvc.CreateLLMProvider()
	if err != nil {
		return nil, err
	}

	// 创建工作流引擎
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
	ws.logger.Infow("workflow engine initialized", "model", model)

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
