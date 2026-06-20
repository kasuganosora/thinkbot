package workflow

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// Manager — 工作流管理器（统一入口）
//
// 职责：
//   - Submit: 异步创建工作流（分析 → 调度执行），立即返回 workflow_id
//   - GetStatus: 查询工作流状态
//   - ListNodes: 查询节点列表（flat / tree）
//   - Control: 重试节点 / 终止工作流
// ============================================================================

// runningInstance 跟踪一个正在执行的工作流。
type runningInstance struct {
	wf        *Workflow
	scheduler *Scheduler
	cancel    context.CancelFunc
	done      chan struct{}
}

// Manager 管理所有工作流实例。
type Manager struct {
	repo     *Repository
	analyzer *Analyzer
	executor *Executor
	ec       EngineConfig
	tracer   trace.Tracer
	tp       trace.TracerProvider
	logger   *zap.SugaredLogger
	emitter  *outbound.EventEmitter // 可为 nil（NoOp 模式）

	mu      sync.RWMutex
	running map[string]*runningInstance

	// 原子计数器 — 可观测性指标
	metrics ManagerMetrics
}

// ManagerMetrics 是工作流管理器的运行时指标（原子计数器快照）。
type ManagerMetrics struct {
	Submitted  atomic.Int64 // 累计提交
	Completed  atomic.Int64 // 累计成功完成
	Failed     atomic.Int64 // 累计失败
	Terminated atomic.Int64 // 累计终止
	Running    atomic.Int64 // 当前运行中
}

// NewManager 创建工作流管理器。
func NewManager(repo *Repository, analyzer *Analyzer, executor *Executor, tp trace.TracerProvider, ec EngineConfig, logger *zap.SugaredLogger, bus outbound.EventBus) *Manager {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	return &Manager{
		repo:     repo,
		analyzer: analyzer,
		executor: executor,
		ec:       ec,
		tracer:   tp.Tracer("github.com/kasuganosora/thinkbot/workflow/manager"),
		tp:       tp,
		logger:   logger.With("component", "workflow_manager"),
		emitter:  outbound.NewEventEmitter(bus, ""),
		running:  make(map[string]*runningInstance),
	}
}

// Metrics 返回当前指标快照的只读副本。
func (m *Manager) Metrics() (submitted, completed, failed, terminated, running int64) {
	return m.metrics.Submitted.Load(),
		m.metrics.Completed.Load(),
		m.metrics.Failed.Load(),
		m.metrics.Terminated.Load(),
		m.metrics.Running.Load()
}

// ============================================================================
// Submit — 异步提交
// ============================================================================

// SubmitRequest 是提交工作流的请求参数。
type SubmitRequest struct {
	Requirement string // 用户需求文本
	MaxParallel int    // 最大并行度（可选，默认 3）
}

// SubmitResult 是提交工作流的立即返回结果。
type SubmitResult struct {
	WorkflowID string `json:"workflowId"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

// Submit 创建工作流并异步启动分析+执行。
// 立即返回 workflow_id，不等待执行完成。
func (m *Manager) Submit(ctx context.Context, req SubmitRequest) (*SubmitResult, error) {
	if req.Requirement == "" {
		return nil, errs.New("requirement is empty")
	}

	wfID := GenerateWorkflowID()
	m.metrics.Submitted.Add(1)

	// 创建初始工作流（status=analyzing）
	wf := NewWorkflow(wfID, req.Requirement, nil)

	// 持久化初始状态
	if err := m.repo.Save(wf); err != nil {
		return nil, errs.Wrap(err, "failed to save initial workflow")
	}

	// 发布提交事件
	m.emitWorkflowEvent(wfID, outbound.EventWorkflowSubmitted, map[string]any{
		"requirement": strutil.Truncate(req.Requirement, 200),
	})

	// 启动后台 goroutine
	bgCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go m.analyzeAndRun(bgCtx, wf, req.MaxParallel, done)

	// 注册运行实例
	inst := &runningInstance{
		wf:     wf,
		cancel: cancel,
		done:   done,
	}
	m.mu.Lock()
	m.running[wfID] = inst
	m.mu.Unlock()

	return &SubmitResult{
		WorkflowID: wfID,
		Status:     string(WorkflowAnalyzing),
		Message:    "工作流已创建，正在分析需求并分解任务...",
	}, nil
}

// emitWorkflowEvent 发布工作流级事件（workflow_id 作为 TraceID，供 SSE 订阅端筛选）。
func (m *Manager) emitWorkflowEvent(wfID string, eventType outbound.EventType, data map[string]any) {
	m.emitter.Emit(context.Background(), eventType, wfID, data)
}

// analyzeAndRun 后台执行：分析需求 → 构建 DAG → 调度执行。
func (m *Manager) analyzeAndRun(ctx context.Context, wf *Workflow, maxParallel int, done chan struct{}) {
	defer close(done)

	m.metrics.Running.Add(1)
	defer m.metrics.Running.Add(-1)

	// Phase 1: 分析需求
	nodes, err := m.analyzer.Analyze(ctx, wf.Requirement)
	if err != nil {
		m.logger.Errorw("analysis failed", "workflow_id", wf.ID, "error", err)
		wf.Status = WorkflowFailed
		wf.Error = fmt.Sprintf("需求分析失败: %s", err.Error())
		_ = m.repo.Save(wf)
		m.metrics.Failed.Add(1)
		m.emitWorkflowEvent(wf.ID, outbound.EventWorkflowFailed, map[string]any{
			"error": wf.Error,
		})
		m.cleanupRunning(wf.ID)
		return
	}

	// 更新工作流节点 + 初始化状态 + 重建索引
	for _, n := range nodes {
		n.Status = NodePending
	}
	wf.Nodes = nodes
	wf.RebuildIndex()
	if err := m.repo.Save(wf); err != nil {
		m.logger.Errorw("failed to save analyzed workflow", "error", err)
	}

	// 发布分析完成事件
	m.emitWorkflowEvent(wf.ID, outbound.EventWorkflowAnalyzed, map[string]any{
		"node_count": len(nodes),
		"nodes":      nodeSummaries(nodes),
	})

	// Phase 2: 调度执行
	m.runScheduler(ctx, wf, maxParallel)
}

// runScheduler 创建并运行 Scheduler，更新指标。
// 被 analyzeAndRun（新工作流）和 Recover（恢复工作流）共用。
func (m *Manager) runScheduler(ctx context.Context, wf *Workflow, maxParallel int) {
	cfg := SchedulerConfig{MaxParallel: maxParallel}
	scheduler := NewScheduler(wf, m.executor, m.repo, cfg, m.ec, m.tp, m.logger, m.emitter)

	m.mu.Lock()
	if inst, ok := m.running[wf.ID]; ok {
		inst.scheduler = scheduler
	}
	m.mu.Unlock()

	finalStatus := scheduler.Run(ctx)
	m.logger.Infow("workflow finished", "workflow_id", wf.ID, "status", finalStatus)

	// 发布终态事件
	switch finalStatus {
	case WorkflowCompleted:
		m.emitWorkflowEvent(wf.ID, outbound.EventWorkflowCompleted, map[string]any{
			"node_count": len(wf.Nodes),
		})
	case WorkflowFailed:
		m.emitWorkflowEvent(wf.ID, outbound.EventWorkflowFailed, map[string]any{
			"error": wf.Error,
		})
	case WorkflowTerminated:
		m.emitWorkflowEvent(wf.ID, outbound.EventWorkflowTerminated, nil)
	}

	switch finalStatus {
	case WorkflowCompleted:
		m.metrics.Completed.Add(1)
	case WorkflowFailed:
		m.metrics.Failed.Add(1)
	case WorkflowTerminated:
		m.metrics.Terminated.Add(1)
	}

	m.cleanupRunning(wf.ID)
}

// ============================================================================
// Recover — 崩溃恢复
// ============================================================================

// RecoveryResult 记录崩溃恢复的结果。
type RecoveryResult struct {
	Total       int      `json:"total"`       // 发现的非终态工作流总数
	Resumed     int      `json:"resumed"`     // 成功恢复调度的工作流数
	Reanalyzed  int      `json:"reanalyzed"`  // 需要重新分析的工作流数
	Failed      int      `json:"failed"`      // 恢复失败的工作流数
	WorkflowIDs []string `json:"workflowIds"` // 涉及的工作流 ID
}

// Recover 扫描数据库中所有非终态工作流（analyzing/running/interrupted），
// 并根据状态执行恢复策略：
//
//   - analyzing 且无节点：重新提交分析（Phase 1 从头开始）
//   - analyzing 且有节点 / running / interrupted：重置中断节点的中间状态，
//     直接从 Phase 2（调度执行）恢复
//
// 应在服务启动时调用一次。
func (m *Manager) Recover(ctx context.Context) (*RecoveryResult, error) {
	workflows, err := m.repo.FindNonTerminal()
	if err != nil {
		return nil, errs.Wrap(err, "failed to find non-terminal workflows")
	}

	result := &RecoveryResult{Total: len(workflows)}
	m.logger.Infow("starting crash recovery", "non_terminal_count", len(workflows))

	for _, wf := range workflows {
		result.WorkflowIDs = append(result.WorkflowIDs, wf.ID)

		// 跳过正在运行中的工作流（可能是 Recover 被重复调用）
		m.mu.RLock()
		_, isRunning := m.running[wf.ID]
		m.mu.RUnlock()
		if isRunning {
			m.logger.Infow("workflow already running, skipping recovery",
				"workflow_id", wf.ID)
			continue
		}

		// 标记为 interrupted（恢复前的中间态）
		prevStatus := wf.Status
		wf.Status = WorkflowInterrupted
		_ = m.repo.Save(wf)

		if len(wf.Nodes) == 0 {
			// 无节点 = 分析阶段崩溃，重新分析
			m.logger.Infow("recovering: re-analyzing workflow (no nodes)",
				"workflow_id", wf.ID, "prev_status", prevStatus)

			wf.Status = WorkflowAnalyzing
			_ = m.repo.Save(wf)

			bgCtx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})

			go m.analyzeAndRun(bgCtx, wf, 0, done)

			m.mu.Lock()
			m.running[wf.ID] = &runningInstance{
				wf:     wf,
				cancel: cancel,
				done:   done,
			}
			m.mu.Unlock()

			result.Reanalyzed++
			continue
		}

		// 有节点 = 调度阶段中断，重置中间状态后恢复调度
		m.logger.Infow("recovering: resuming workflow scheduling",
			"workflow_id", wf.ID, "prev_status", prevStatus,
			"node_count", len(wf.Nodes))

		wf.EnsureIndex()
		resetCount := 0
		for _, n := range wf.Nodes {
			if !n.Status.IsTerminal() && n.Status != NodePending {
				// running/reviewing/ready → pending（执行被中断）
				n.Status = NodePending
				n.Error = ""
				n.RetryCount = 0
				n.StartedAt = nil
				resetCount++
			}
		}

		m.logger.Infow("reset interrupted nodes to pending",
			"workflow_id", wf.ID, "reset_count", resetCount)

		wf.Status = WorkflowRunning
		if err := m.repo.Save(wf); err != nil {
			m.logger.Errorw("failed to save recovered workflow",
				"workflow_id", wf.ID, "error", err)
			result.Failed++
			continue
		}

		// 恢复调度执行
		bgCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})

		m.mu.Lock()
		m.running[wf.ID] = &runningInstance{
			wf:     wf,
			cancel: cancel,
			done:   done,
		}
		m.mu.Unlock()

		go func(wf *Workflow) {
			defer close(done)
			m.metrics.Running.Add(1)
			defer m.metrics.Running.Add(-1)

			m.runScheduler(bgCtx, wf, 0)
		}(wf)

		result.Resumed++
	}

	m.logger.Infow("crash recovery complete",
		"total", result.Total,
		"resumed", result.Resumed,
		"reanalyzed", result.Reanalyzed,
		"failed", result.Failed)

	return result, nil
}

// ============================================================================
// GetStatus — 查询工作流状态
// ============================================================================

// StatusResult 是工作流状态查询结果。
type StatusResult struct {
	ID          string         `json:"id"`
	Status      WorkflowStatus `json:"status"`
	Requirement string         `json:"requirement"`
	NodeCount   int            `json:"nodeCount"`
	Progress    ProgressInfo   `json:"progress"`
	CreatedAt   string         `json:"createdAt"`
	Error       string         `json:"error,omitempty"`
}

// ProgressInfo 是工作流进度信息。
type ProgressInfo struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Reviewing int `json:"reviewing"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// GetStatus 查询工作流状态。
func (m *Manager) GetStatus(wfID string) (*StatusResult, error) {
	wf, err := m.repo.Get(wfID)
	if err != nil {
		return nil, err
	}

	wf.EnsureIndex()

	progress := ProgressInfo{}
	for _, n := range wf.Nodes {
		switch n.Status {
		case NodePending, NodeReady:
			progress.Pending++
		case NodeRunning:
			progress.Running++
		case NodeReviewing:
			progress.Reviewing++
		case NodeCompleted:
			progress.Completed++
		case NodeFailed:
			progress.Failed++
		case NodeSkipped:
			progress.Skipped++
		}
	}

	createdAt := ""
	if !wf.CreatedAt.IsZero() {
		createdAt = wf.CreatedAt.Format("2006-01-02 15:04:05")
	}

	return &StatusResult{
		ID:          wf.ID,
		Status:      wf.Status,
		Requirement: wf.Requirement,
		NodeCount:   len(wf.Nodes),
		Progress:    progress,
		CreatedAt:   createdAt,
		Error:       wf.Error,
	}, nil
}

// ============================================================================
// ListNodes — 查询节点列表
// ============================================================================

// ListNodesResult 是节点列表查询结果。
type ListNodesResult struct {
	WorkflowID string         `json:"workflowId"`
	Status     WorkflowStatus `json:"status"`
	Format     string         `json:"format"` // "flat" or "tree"
	Flat       []NodeFlat     `json:"flat,omitempty"`
	Tree       []*TreeNode    `json:"tree,omitempty"`
}

// ListNodes 查询工作流节点列表。
// format: "flat"（平铺）或 "tree"（树状）。
func (m *Manager) ListNodes(wfID, format string) (*ListNodesResult, error) {
	wf, err := m.repo.Get(wfID)
	if err != nil {
		return nil, err
	}

	wf.EnsureIndex()

	result := &ListNodesResult{
		WorkflowID: wfID,
		Status:     wf.Status,
		Format:     format,
	}

	switch format {
	case "tree":
		result.Tree = BuildTree(wf)
	default: // flat 或其他值
		result.Format = "flat"
		result.Flat = make([]NodeFlat, 0, len(wf.Nodes))
		for _, n := range wf.Nodes {
			result.Flat = append(result.Flat, n.ToFlat())
		}
	}

	return result, nil
}

// ============================================================================
// Control — 流程控制（重试 / 终止）
// ============================================================================

// ControlAction 是控制操作的类型。
type ControlAction string

const (
	ActionRetry     ControlAction = "retry"
	ActionTerminate ControlAction = "terminate"
)

// ControlRequest 是控制操作请求。
type ControlRequest struct {
	Action ControlAction `json:"action"`           // "retry" or "terminate"
	NodeID string        `json:"nodeId,omitempty"` // retry 时指定节点 ID
}

// ControlResult 是控制操作结果。
type ControlResult struct {
	WorkflowID string `json:"workflowId"`
	Action     string `json:"action"`
	Success    bool   `json:"success"`
	Message    string `json:"message"`
}

// Control 执行流程控制操作。
func (m *Manager) Control(wfID string, req ControlRequest) (*ControlResult, error) {
	m.mu.RLock()
	inst, ok := m.running[wfID]
	m.mu.RUnlock()

	// 验证工作流存在
	wf, err := m.repo.Get(wfID)
	if err != nil {
		return nil, err
	}
	wf.EnsureIndex()

	switch req.Action {
	case ActionTerminate:
		if !ok {
			return nil, errs.New("workflow is not running")
		}
		if inst.scheduler != nil {
			inst.scheduler.Terminate()
		} else {
			// analyzing 阶段 scheduler 尚未创建，取消 context 中断分析
			inst.cancel()
		}
		return &ControlResult{
			WorkflowID: wfID,
			Action:     "terminate",
			Success:    true,
			Message:    "终止信号已发送，正在停止所有未完成的节点...",
		}, nil

	case ActionRetry:
		if req.NodeID == "" {
			return nil, errs.New("nodeId is required for retry action")
		}
		if _, exists := wf.GetNode(req.NodeID); !exists {
			return nil, errs.Newf("node %q not found in workflow %q", req.NodeID, wfID)
		}
		if ok && inst.scheduler != nil {
			inst.scheduler.SubmitRetry(req.NodeID)
		} else {
			return nil, errs.New("workflow is not actively running, cannot retry")
		}
		return &ControlResult{
			WorkflowID: wfID,
			Action:     "retry",
			Success:    true,
			Message:    fmt.Sprintf("节点 %s 的重试请求已提交", req.NodeID),
		}, nil

	default:
		return nil, errs.Newf("unknown action: %s (use 'retry' or 'terminate')", req.Action)
	}
}

// ============================================================================
// 内部辅助
// ============================================================================

func (m *Manager) cleanupRunning(wfID string) {
	m.mu.Lock()
	delete(m.running, wfID)
	m.mu.Unlock()
}

// GetWorkflow 获取工作流领域对象（内部使用）。
func (m *Manager) GetWorkflow(wfID string) (*Workflow, error) {
	return m.repo.Get(wfID)
}

// nodeSummaries 生成节点的摘要信息（用于事件 payload）。
func nodeSummaries(nodes []*DAGNode) []map[string]any {
	result := make([]map[string]any, len(nodes))
	for i, n := range nodes {
		result[i] = map[string]any{
			"id":           n.ID,
			"name":         n.Name,
			"dependencies": n.Dependencies,
			"review":       n.Review,
		}
	}
	return result
}

// WaitDone 阻塞等待指定工作流执行完成（主要用于测试）。
func (m *Manager) WaitDone(wfID string) {
	m.mu.RLock()
	inst, ok := m.running[wfID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	<-inst.done
}
