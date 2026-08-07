package workflow

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
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

	// onWorkflowCompleted 是工作流进入终态后的回调（由 api 层注入），
	// 用于唤醒 agent 继续后续流程。仅当阻塞等待方（task 工具的 waitForTerminal）
	// 已退出（超时/取消）时才触发，避免与正常阻塞返回路径重复唤醒。
	onWorkflowCompleted func(wf *Workflow)

	// consumeMu 保护 consumed 与 needsContinuation 映射。
	consumeMu sync.Mutex
	// consumed 记录「终态结果已由阻塞等待方交付」或「事件路径已触发续跑」的工作流，
	// 用于两条唤醒路径的去重：谁先标记谁负责唤醒，另一方放弃。
	consumed map[string]bool
	// needsContinuation 标记工作流终态后已注入续跑消息，供前端 status 轮询感知。
	needsContinuation map[string]bool

	// recoverOnce 确保 Recover 只执行一次。
	recoverOnce sync.Once

	// sweeperOnce 确保卡死看门狗只启动一次。
	sweeperOnce sync.Once

	// quotaWatchOnce 确保配额续跑看门狗只启动一次。
	quotaWatchOnce sync.Once

	// 原子计数器 — 可观测性指标
	metrics ManagerMetrics
}

// 卡死看门狗相关常量。
const (
	// sweepInterval 看门狗扫描周期。
	sweepInterval = 2 * time.Minute
	// analyzingStaleMaxAge 分析阶段无进展上限（分析总时长上限默认 10min，20min 为兜底）。
	analyzingStaleMaxAge = 20 * time.Minute
	// runningStaleMaxAge 执行阶段无进展上限（节点可能耗时较长，给足余量）。
	runningStaleMaxAge = 3 * time.Hour
	// interruptedStaleMaxAge 中断恢复态无进展上限。
	interruptedStaleMaxAge = 30 * time.Minute
)

// staleThreshold 返回某状态允许的最大无进展时长；非监控状态返回 0。
func staleThreshold(status WorkflowStatus) time.Duration {
	switch status {
	case WorkflowAnalyzing:
		return analyzingStaleMaxAge
	case WorkflowRunning:
		return runningStaleMaxAge
	case WorkflowInterrupted:
		return interruptedStaleMaxAge
	}
	return 0
}

// ManagerMetrics 是工作流管理器的运行时指标（原子计数器快照）。
type ManagerMetrics struct {
	Submitted     atomic.Int64 // 累计提交
	Completed     atomic.Int64 // 累计成功完成
	Failed        atomic.Int64 // 累计失败
	Terminated    atomic.Int64 // 累计终止
	Running       atomic.Int64 // 当前运行中
	NodeExecuted  atomic.Int64 // 累计节点执行次数
	NodeFailed    atomic.Int64 // 累计节点失败次数
	NodeRetries   atomic.Int64 // 累计节点重试次数（不含首次执行）
	NodeReviews   atomic.Int64 // 累计 Review 迭代次数
	NodeSkipped   atomic.Int64 // 累计节点跳过次数（级联 + 终止）
	PersistErrors atomic.Int64 // 累计持久化失败次数
}

// MetricsSnapshot 是 ManagerMetrics 的只读快照，用于序列化展示。
type MetricsSnapshot struct {
	Submitted     int64 `json:"submitted"`
	Completed     int64 `json:"completed"`
	Failed        int64 `json:"failed"`
	Terminated    int64 `json:"terminated"`
	Running       int64 `json:"running"`
	NodeExecuted  int64 `json:"nodeExecuted"`
	NodeFailed    int64 `json:"nodeFailed"`
	NodeRetries   int64 `json:"nodeRetries"`
	NodeReviews   int64 `json:"nodeReviews"`
	NodeSkipped   int64 `json:"nodeSkipped"`
	PersistErrors int64 `json:"persistErrors"`
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
		logger:   logger.With("stage", "workflow_manager"),
		emitter:  outbound.NewEventEmitter(bus, ""),
		running:  make(map[string]*runningInstance),
		consumed: make(map[string]bool),
		needsContinuation: make(map[string]bool),
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

// MetricsSnapshot 返回包含所有指标的只读快照。
func (m *Manager) MetricsSnapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Submitted:     m.metrics.Submitted.Load(),
		Completed:     m.metrics.Completed.Load(),
		Failed:        m.metrics.Failed.Load(),
		Terminated:    m.metrics.Terminated.Load(),
		Running:       m.metrics.Running.Load(),
		NodeExecuted:  m.metrics.NodeExecuted.Load(),
		NodeFailed:    m.metrics.NodeFailed.Load(),
		NodeRetries:   m.metrics.NodeRetries.Load(),
		NodeReviews:   m.metrics.NodeReviews.Load(),
		NodeSkipped:   m.metrics.NodeSkipped.Load(),
		PersistErrors: m.metrics.PersistErrors.Load(),
	}
}

// ============================================================================
// Submit — 异步提交
// ============================================================================

// SubmitRequest 是提交工作流的请求参数。
type SubmitRequest struct {
	Requirement string // 用户需求文本
	MaxParallel int    // 最大并行度（可选，默认 3）
	// GoalMode 目标模式：开启后 review 节点在节点级迭代仍不通过时，回退到其 Feedback
	// 目标节点重新执行（注入审查意见），形成「工作→审查→修复→审查」的全局闭环，
	// 直到 review 通过或达到最大迭代轮数。
	GoalMode bool
	// GoalMaxIterations 目标模式最大闭环轮数（0 表示使用引擎默认配置）。
	GoalMaxIterations int
	// BotID / SessionID 工作流来源（哪个 bot、哪个会话提交的）。
	// 可为空（非 web 渠道 / 历史调用），落库后供前端刷新页面时按会话恢复卡片。
	BotID     string
	SessionID string
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
	ctx, span := m.tracer.Start(ctx, "workflow.manager.submit")
	defer span.End()

	if req.Requirement == "" {
		return nil, errs.New("requirement is empty")
	}

	wfID := GenerateWorkflowID()
	m.metrics.Submitted.Add(1)

	// 创建初始工作流（status=analyzing）
	wf := NewWorkflow(wfID, req.Requirement, nil)
	wf.GoalMode = req.GoalMode
	wf.GoalMaxIterations = req.GoalMaxIterations
	// 记录来源，供前端刷新后按会话恢复卡片、以及排查时定位工作空间。
	wf.BotID = req.BotID
	wf.SessionID = req.SessionID

	// 持久化初始状态
	if err := m.repo.Save(wf); err != nil {
		return nil, errs.Wrap(err, "failed to save initial workflow")
	}

	// 发布提交事件
	m.emitWorkflowEvent(ctx, wfID, outbound.EventWorkflowSubmitted, map[string]any{
		"requirement": strutil.Truncate(req.Requirement, 200),
	})

	// 注册运行实例（先注册再启动，避免 goroutine 中 runScheduler 找不到 instance）
	bgCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	inst := &runningInstance{
		wf:     wf,
		cancel: cancel,
		done:   done,
	}
	m.mu.Lock()
	m.running[wfID] = inst
	m.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				m.logger.Errorw("panic in workflow goroutine",
					"workflow_id", wf.ID, "panic", r)
				wf.Status = WorkflowFailed
				wf.Error = fmt.Sprintf("internal error: %v", r)
				_ = m.repo.Save(wf)
				m.metrics.Failed.Add(1)
				m.emitWorkflowEvent(context.Background(), wf.ID, outbound.EventWorkflowFailed, map[string]any{
					"error": wf.Error,
				})
				m.cleanupRunning(wf.ID)
			}
		}()
		m.analyzeAndRun(bgCtx, wf, req.MaxParallel)
	}()

	return &SubmitResult{
		WorkflowID: wfID,
		Status:     string(WorkflowAnalyzing),
		Message:    "工作流已创建，正在分析需求并分解任务...",
	}, nil
}

// emitWorkflowEvent 发布工作流级事件（workflow_id 作为 TraceID，供 SSE 订阅端筛选）。
func (m *Manager) emitWorkflowEvent(ctx context.Context, wfID string, eventType outbound.EventType, data map[string]any) {
	m.emitter.Emit(ctx, eventType, wfID, data)
}

// SetOnWorkflowCompleted 设置工作流终态回调（api 层注入）。
// 回调仅在「阻塞等待方已退出（超时/取消）」时被触发（Manager 已做去重），
// 用于把工作流结果注入原会话、唤醒 agent 继续后续流程。
func (m *Manager) SetOnWorkflowCompleted(fn func(wf *Workflow)) {
	m.mu.Lock()
	m.onWorkflowCompleted = fn
	m.mu.Unlock()
}

// markDelivered 标记某工作流终态已由阻塞等待方（waitForTerminal）交付给 agent。
// 正常路径下调用，使事件路径的 tryDeliverToAgent 跳过，避免重复唤醒。
func (m *Manager) markDelivered(wfID string) {
	m.consumeMu.Lock()
	m.consumed[wfID] = true
	m.consumeMu.Unlock()
}

// tryDeliverToAgent 终态后尝试唤醒 agent 续跑。
// 去重：若阻塞等待方已交付（或事件路径已先触发），立即返回；否则标记并调用回调。
func (m *Manager) tryDeliverToAgent(wf *Workflow) {
	m.consumeMu.Lock()
	if m.consumed[wf.ID] {
		m.consumeMu.Unlock()
		return
	}
	m.consumed[wf.ID] = true
	fn := m.onWorkflowCompleted
	m.consumeMu.Unlock()
	if fn != nil {
		fn(wf)
	}
}

// SetNeedsContinuation 标记工作流终态后已注入续跑消息（供前端 status 轮询感知）。
func (m *Manager) SetNeedsContinuation(wfID string, v bool) {
	m.consumeMu.Lock()
	if v {
		m.needsContinuation[wfID] = true
	} else {
		delete(m.needsContinuation, wfID)
	}
	m.consumeMu.Unlock()
}

// consumeNeedsContinuation 读取并清除续跑标记（前端一次轮询即消费）。
func (m *Manager) consumeNeedsContinuation(wfID string) bool {
	m.consumeMu.Lock()
	defer m.consumeMu.Unlock()
	if m.needsContinuation[wfID] {
		delete(m.needsContinuation, wfID)
		return true
	}
	return false
}

// analyzeAndRun 后台执行：分析需求 → 构建 DAG → 调度执行。
func (m *Manager) analyzeAndRun(ctx context.Context, wf *Workflow, maxParallel int) {
	ctx, span := m.tracer.Start(ctx, "workflow.manager.analyze_and_run")
	defer span.End()

	m.metrics.Running.Add(1)
	defer m.metrics.Running.Add(-1)

	// Phase 1: 分析需求
	// 分析阶段总时长上限：防止 GLM 退化时分析器反复重试（每次最坏 stuck×3≈9 分钟）
	// 把「分析中」拖成数十分钟的黑洞。超过该时长整轮分析失败并给出明确报错，
	// 前端可立即看到结果而非一直转圈。
	analyzeCtx := ctx
	analyzeCancel := func() {}
	if m.ec.AnalyzerMaxDuration > 0 {
		analyzeCtx, analyzeCancel = context.WithTimeout(ctx, m.ec.AnalyzerMaxDuration)
	}
	defer analyzeCancel()

	// 分析进度回调：每次尝试时更新文案并落库，前端轮询即可看到「第 N 次尝试」等进展，
	// 避免长期停留在静态「分析中…」被误判为卡死。
	onProgress := func(attempt int, phase string) {
		m.mu.Lock()
		wf.AnalyzeMessage = phase
		m.mu.Unlock()
		_ = m.repo.Save(wf)
	}

	nodes, err := m.analyzer.Analyze(analyzeCtx, wf.Requirement, wf.GoalMode, onProgress)

	// 无论分析成功与否，都必须清掉分析阶段的进度文案：它只在分析阶段有意义。
	// 残留会让前端把已结束的工作流一直渲染成「正在调用模型分析需求（第 N/5 次尝试）」，
	// 表现为后端 failed 而 UI 永远转圈的假卡死。
	m.mu.Lock()
	wf.ClearAnalyzeMessage()
	m.mu.Unlock()

	if err != nil {
		// 分析阶段被显式终止（bgCtx 被 Control(terminate) 取消）：标记为 terminated 而非 failed，
		// 避免把"用户/bot 主动终止"误报成"分析失败"。
		if ctx.Err() != nil {
			m.logger.Infow("analysis terminated", "workflow_id", wf.ID, "error", err)
			wf.Status = WorkflowTerminated
			wf.Error = "分析阶段被终止（需求分析未完成）"
			_ = m.repo.Save(wf)
			m.metrics.Terminated.Add(1)
			m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowTerminated, map[string]any{
				"error": wf.Error,
			})
			m.cleanupRunning(wf.ID)
			return
		}
		// 分析总时长超限：给出更明确的报错，引导用户稍后重试。
		if analyzeCtx.Err() != nil && m.ec.AnalyzerMaxDuration > 0 {
			m.logger.Errorw("analysis timed out", "workflow_id", wf.ID,
				"max_duration", m.ec.AnalyzerMaxDuration.String(), "error", err)
			wf.Status = WorkflowFailed
			wf.Error = fmt.Sprintf("需求分析超时（超过 %s），通常由模型服务不稳定导致，请稍后重试", m.ec.AnalyzerMaxDuration)
			_ = m.repo.Save(wf)
			m.metrics.Failed.Add(1)
			m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowFailed, map[string]any{
				"error": wf.Error,
			})
			m.cleanupRunning(wf.ID)
			return
		}
		m.logger.Errorw("analysis failed", "workflow_id", wf.ID, "error", err)
		wf.Status = WorkflowFailed
		wf.Error = fmt.Sprintf("需求分析失败: %s", err.Error())
		_ = m.repo.Save(wf)
		m.metrics.Failed.Add(1)
		m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowFailed, map[string]any{
			"error": wf.Error,
		})
		m.cleanupRunning(wf.ID)
		return
	}

	// 更新工作流节点 + 初始化状态 + 编译验证 + 重建索引
	for _, n := range nodes {
		n.Status = NodePending
	}
	wf.Nodes = nodes
	wf.RebuildIndex()

	// 编译工作流图：校验 DAG + 计算拓扑排序 + 构建邻接索引
	if err := wf.Compile(); err != nil {
		m.logger.Errorw("workflow compilation failed", "workflow_id", wf.ID, "error", err)
		wf.Status = WorkflowFailed
		wf.Error = fmt.Sprintf("DAG 编译失败: %s", err.Error())
		_ = m.repo.Save(wf)
		m.metrics.Failed.Add(1)
		m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowFailed, map[string]any{
			"error": wf.Error,
		})
		m.cleanupRunning(wf.ID)
		return
	}

	if err := m.repo.Save(wf); err != nil {
		m.logger.Errorw("failed to save analyzed workflow", "error", err)
	}

	// 发布分析完成事件
	m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowAnalyzed, map[string]any{
		"node_count": len(nodes),
		"nodes":      nodeSummaries(nodes),
	})

	// Phase 2: 调度执行
	m.runScheduler(ctx, wf, maxParallel)
}

// runScheduler 创建并运行 Scheduler，更新指标。
// 被 analyzeAndRun（新工作流）和 Recover（恢复工作流）共用。
func (m *Manager) runScheduler(ctx context.Context, wf *Workflow, maxParallel int) {
	ctx, span := m.tracer.Start(ctx, "workflow.manager.run_scheduler")
	defer span.End()

	cfg := SchedulerConfig{MaxParallel: maxParallel}
	scheduler := NewScheduler(wf, m.executor, m.repo, cfg, m.ec, m.tp, m.logger, m.emitter, &m.metrics)

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
		m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowCompleted, map[string]any{
			"node_count": len(wf.Nodes),
		})
	case WorkflowFailed:
		m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowFailed, map[string]any{
			"error": wf.Error,
		})
	case WorkflowTerminated:
		m.emitWorkflowEvent(ctx, wf.ID, outbound.EventWorkflowTerminated, nil)
	}

	switch finalStatus {
	case WorkflowCompleted:
		m.metrics.Completed.Add(1)
	case WorkflowFailed:
		m.metrics.Failed.Add(1)
	case WorkflowTerminated:
		m.metrics.Terminated.Add(1)
	}

	// 唤醒 agent 续跑：仅当阻塞等待方已退出（超时/取消/历史或外部触发的工作流）
	// 才会真正触发——正常阻塞返回路径已在 waitForTerminal 内 markDelivered，此处跳过。
	m.tryDeliverToAgent(wf)

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
	// 确保 Recover 只执行一次，避免并发调用导致重复恢复
	var result *RecoveryResult
	var recoverErr error
	m.recoverOnce.Do(func() {
		result, recoverErr = m.recover(ctx)
	})
	return result, recoverErr
}

// ============================================================================
// Stuck-watchdog — 进程内卡死工作流看门狗
// ============================================================================

// StartSweeper 启动卡死看门狗（进程级，应在服务启动时调用一次）。
// 周期扫描非终态工作流，将超过无进展阈值（analyzing/running/interrupted 各异）的
// 工作流强制标记失败，避免「分析中/执行中」因 GLM 退化或 goroutine 卡死而永久挂起。
//
// 与 Recover（仅启动时跑一次、且跳过在跑实例）互补：Recover 处理进程崩溃后的恢复，
// Sweeper 处理进程内存活但已卡死、且 Recover 因「在跑」而被跳过的工作流——这正是
// 之前「修了很多遍仍卡住」的盲区：一个在跑但 wedged 的 goroutine 永远不会被回收。
func (m *Manager) StartSweeper(ctx context.Context) {
	m.sweeperOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(sweepInterval)
			defer ticker.Stop()
			m.logger.Infow("workflow stuck-watchdog started", "interval", sweepInterval.String())
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.SweepStale(context.Background())
				}
			}
		}()
	})
}

// SweepStale 扫描非终态工作流，将超过无进展阈值者强制失败。
// 供 StartSweeper 周期调用，也可手动触发调试。
func (m *Manager) SweepStale(ctx context.Context) {
	wfs, err := m.repo.FindNonTerminal()
	if err != nil {
		m.logger.Warnw("stuck-watchdog: failed to list workflows", "error", err)
		return
	}
	now := time.Now()
	for _, wf := range wfs {
		// 配额暂停窗口内：不按「无进展」判死。工作流在等上游配额恢复，
		// 由配额续跑看门狗（StartQuotaWatch）到点拉起，强行判死会丢掉已完成的产物。
		if wf.QuotaResumeAt != nil && now.Before(*wf.QuotaResumeAt) {
			continue
		}
		maxAge := staleThreshold(wf.Status)
		if maxAge <= 0 {
			continue
		}
		age := now.Sub(wf.UpdatedAt)
		if age <= maxAge {
			continue
		}
		m.forceFailStale(wf, age)
	}
}

// ============================================================================
// Quota-watchdog — 配额熔断后的续跑看门狗
// ============================================================================

// quotaWatchInterval 是配额续跑看门狗的扫描周期。
//
// 取 30s 远小于单轮最长等待（hardQuotaWaitCap=12h，但正常情况下尊重服务端给出的
// 真实重置时刻），保证到点后很快续跑；同时避免过于频繁扫描。
// 恢复时刻由熔断器写入 wf.QuotaResumeAt。
const quotaWatchInterval = 30 * time.Second

// StartQuotaWatch 启动配额续跑看门狗（进程级，应在服务启动时调用一次）。
//
// 职责（双保险）：
//  1. 对 QuotaResumeAt 仍在未来的 interrupted 工作流**投喂心跳**（刷新 UpdatedAt），
//     避免卡死看门狗 SweepStale 在等待期间（尤其 QuotaResumeAt 刚到期的竞态窗口）把
//     工作流误判为卡死（interruptedStaleMaxAge=30min）而丢弃已完成产物；
//  2. 对 QuotaResumeAt 已到期的工作流拉起续跑（重置 pending 节点、清掉配额错误），
//     让配额恢复后的工作流原地续跑、保留已完成产物。
//
// 与 StartSweeper 互补：Sweeper 把「卡死」的工作流判死，本看门狗把「等配额恢复」的
// 工作流续跑——二者通过 wf.QuotaResumeAt 区分（SweepStale 在窗口内显式跳过 + 心跳投喂）。
func (m *Manager) StartQuotaWatch(ctx context.Context) {
	m.quotaWatchOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(quotaWatchInterval)
			defer ticker.Stop()
			m.logger.Infow("workflow quota-watchdog started", "interval", quotaWatchInterval.String())
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.ResumeQuotaInterrupted(context.Background())
				}
			}
		}()
	})
}

// ResumeQuotaInterrupted 扫描并续跑所有「配额窗口已到期」的 interrupted 工作流。
// 供 StartQuotaWatch 周期调用，也可手动触发调试。
func (m *Manager) ResumeQuotaInterrupted(ctx context.Context) {
	wfs, err := m.repo.FindNonTerminal()
	if err != nil {
		m.logger.Warnw("quota-watchdog: failed to list workflows", "error", err)
		return
	}
	now := time.Now()
	for _, wf := range wfs {
		if wf.Status != WorkflowInterrupted {
			continue
		}
		if wf.QuotaResumeAt == nil {
			// 异常态：interrupted 却无恢复时刻，交回 SweepStale 常规判死处理。
			continue
		}
		if now.Before(*wf.QuotaResumeAt) {
			// 仍在配额等待窗口内：向卡死看门狗（SweepStale）**投喂心跳**——
			// 仅刷新 UpdatedAt，不改动任何业务状态。否则一旦越过 QuotaResumeAt，
			// SweepStale 的 now.Before 保护失效，而 UpdatedAt 仍是几小时前的旧值，
			// age 远超 interruptedStaleMaxAge=30min，工作流会被误判为卡死丢弃产物。
			m.feedQuotaWatchdog(wf)
			continue
		}
		// 配额窗口已到期：拉起续跑（resumeQuotaInterrupted 内部 Save 同样刷新 UpdatedAt）。
		m.resumeQuotaInterrupted(wf)
	}
}

// feedQuotaWatchdog 在配额等待期间向卡死看门狗（SweepStale）投喂心跳：仅调用
// repo.Save 刷新 UpdatedAt，不改动任何业务状态。SweepStale 据此看到工作流「仍在
// 活跃等待」，不会按 interruptedStaleMaxAge=30min 误判为卡死；而续跑仍由
// ResumeQuotaInterrupted 的到期分支按 QuotaResumeAt 触发。这是「无职守」续跑的
// 双保险（第一道是 SweepStale 的 now.Before(QuotaResumeAt) 跳过）。
func (m *Manager) feedQuotaWatchdog(wf *Workflow) {
	if err := m.repo.Save(wf); err != nil {
		m.logger.Warnw("quota-watchdog: failed to feed heartbeat", "workflow_id", wf.ID, "error", err)
	}
}

// resumeQuotaInterrupted 把单个配额暂停到期的工作流重新拉起调度。
//
// 关键：只清掉配额暂停标记和 pending 节点的配额错误，已 completed 节点的产物
// 原样保留；清空 QuotaResumeAt 让 Sweeper 恢复常规无进展监控。若 DAG 编译失败
// （极少见）则按失败收尾。
func (m *Manager) resumeQuotaInterrupted(wf *Workflow) {
	// 已有在跑实例则不重复拉起（防止与 Recover / 手动重试撞车）。
	m.mu.Lock()
	if _, ok := m.running[wf.ID]; ok {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	wf.EnsureIndex()
	if !wf.Compiled() {
		if err := wf.Compile(); err != nil {
			m.logger.Errorw("quota-watchdog: failed to compile, marking failed",
				"workflow_id", wf.ID, "error", err)
			wf.Status = WorkflowFailed
			wf.Error = fmt.Sprintf("DAG 编译失败: %s", err.Error())
			wf.QuotaResumeAt = nil
			if err := m.repo.Save(wf); err != nil {
				m.logger.Errorw("quota-watchdog: failed to persist failed workflow", "error", err)
			}
			m.metrics.Failed.Add(1)
			return
		}
	}

	// 重置：清掉配额暂停标记、恢复 pending 节点状态。
	wf.Status = WorkflowRunning
	wf.QuotaResumeAt = nil
	resetCount := 0
	for _, n := range wf.Nodes {
		if n.Status == NodePending {
			// 配额暂停期间被设回 pending 的节点：清掉配额错误信息即可续跑。
			if n.Error != "" {
				n.Error = ""
				n.StartedAt = nil
				n.CompletedAt = nil
				resetCount++
			}
		}
	}
	if err := m.repo.Save(wf); err != nil {
		m.logger.Errorw("quota-watchdog: failed to persist resumed workflow", "error", err)
		return
	}

	// 重新拉起调度（与崩溃恢复同一套模式：先注册再启动）。
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
		defer func() {
			if r := recover(); r != nil {
				m.logger.Errorw("panic in quota-resume workflow goroutine",
					"workflow_id", wf.ID, "panic", r)
				wf.Status = WorkflowFailed
				wf.Error = fmt.Sprintf("internal error: %v", r)
				_ = m.repo.Save(wf)
				m.metrics.Failed.Add(1)
				m.emitWorkflowEvent(context.Background(), wf.ID, outbound.EventWorkflowFailed, map[string]any{
					"error": wf.Error,
				})
				m.cleanupRunning(wf.ID)
			}
		}()
		m.metrics.Running.Add(1)
		defer m.metrics.Running.Add(-1)
		m.runScheduler(bgCtx, wf, 0)
	}(wf)

	m.logger.Infow("quota-watchdog: resumed workflow for continued execution",
		"workflow_id", wf.ID, "reset_pending", resetCount)
}

// forceFailStale 强制终止一个长时间无进展的工作流。
func (m *Manager) forceFailStale(wf *Workflow, age time.Duration) {
	prevStatus := wf.Status

	// 若存在在跑实例，取消其 goroutine（被卡死的 LLM 调用会随 ctx 取消而返回）。
	m.mu.Lock()
	if inst, ok := m.running[wf.ID]; ok && inst.cancel != nil {
		inst.cancel()
	}
	m.mu.Unlock()

	wf.Status = WorkflowFailed
	wf.Error = fmt.Sprintf("工作流长时间无进展（约 %.0f 分钟无状态更新），已被卡死看门狗自动终止，疑似分析/执行卡死。可重新提交。", age.Minutes())
	if err := m.repo.Save(wf); err != nil {
		m.logger.Errorw("stuck-watchdog: failed to persist forced-fail", "workflow_id", wf.ID, "error", err)
		return
	}
	m.metrics.Failed.Add(1)
	m.emitWorkflowEvent(context.Background(), wf.ID, outbound.EventWorkflowFailed, map[string]any{
		"error":  wf.Error,
		"reason": "stuck_watchdog",
	})
	m.cleanupRunning(wf.ID)
	m.logger.Infow("stuck-watchdog: force-failed stale workflow",
		"workflow_id", wf.ID, "prev_status", string(prevStatus), "age_min", age.Minutes())
}

func (m *Manager) recover(ctx context.Context) (*RecoveryResult, error) {
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

			// 先注册再启动，确保 runScheduler 能找到 instance
			m.mu.Lock()
			m.running[wf.ID] = &runningInstance{
				wf:     wf,
				cancel: cancel,
				done:   done,
			}
			m.mu.Unlock()

			go func() {
				defer close(done)
				defer func() {
					if r := recover(); r != nil {
						m.logger.Errorw("panic in workflow goroutine",
							"workflow_id", wf.ID, "panic", r)
						wf.Status = WorkflowFailed
						wf.Error = fmt.Sprintf("internal error: %v", r)
						_ = m.repo.Save(wf)
						m.metrics.Failed.Add(1)
						m.emitWorkflowEvent(context.Background(), wf.ID, outbound.EventWorkflowFailed, map[string]any{
							"error": wf.Error,
						})
						m.cleanupRunning(wf.ID)
					}
				}()
				m.analyzeAndRun(bgCtx, wf, 0)
			}()

			result.Reanalyzed++
			continue
		}

		// 有节点 = 调度阶段中断，重置中间状态后恢复调度
		m.logger.Infow("recovering: resuming workflow scheduling",
			"workflow_id", wf.ID, "prev_status", prevStatus,
			"node_count", len(wf.Nodes))

		wf.EnsureIndex()
		// 反序列化的 workflow 没有编译缓存，需要重新编译
		if !wf.Compiled() {
			if err := wf.Compile(); err != nil {
				m.logger.Errorw("failed to compile recovered workflow, marking as failed",
					"workflow_id", wf.ID, "error", err)
				wf.Status = WorkflowFailed
				wf.Error = fmt.Sprintf("DAG 编译失败: %s", err.Error())
				_ = m.repo.Save(wf)
				m.metrics.Failed.Add(1)
				result.Failed++
				continue
			}
		}
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

		// 先注册再启动，确保 runScheduler 能找到 instance
		m.mu.Lock()
		m.running[wf.ID] = &runningInstance{
			wf:     wf,
			cancel: cancel,
			done:   done,
		}
		m.mu.Unlock()

		go func(wf *Workflow) {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					m.logger.Errorw("panic in workflow goroutine",
						"workflow_id", wf.ID, "panic", r)
					wf.Status = WorkflowFailed
					wf.Error = fmt.Sprintf("internal error: %v", r)
					_ = m.repo.Save(wf)
					m.metrics.Failed.Add(1)
					m.emitWorkflowEvent(context.Background(), wf.ID, outbound.EventWorkflowFailed, map[string]any{
						"error": wf.Error,
					})
					m.cleanupRunning(wf.ID)
				}
			}()
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
	ID             string         `json:"id"`
	Status         WorkflowStatus `json:"status"`
	Requirement    string         `json:"requirement"`
	NodeCount      int            `json:"nodeCount"`
	Progress       ProgressInfo   `json:"progress"`
	CreatedAt      string         `json:"createdAt"`
	Error          string         `json:"error,omitempty"`
	AnalyzeMessage string         `json:"analyzeMessage,omitempty"`
	// 目标模式相关（前端展示闭环进度）
	GoalMode          bool `json:"goalMode,omitempty"`
	GoalIteration     int  `json:"goalIteration,omitempty"`
	GoalMaxIterations int  `json:"goalMaxIterations,omitempty"`
	// NeedsContinuation 标记工作流终态后后端已注入续跑消息，前端据此 resume 接收流式回复。
	NeedsContinuation bool `json:"needsContinuation,omitempty"`
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
		ID:                wf.ID,
		Status:            wf.Status,
		Requirement:       wf.Requirement,
		NodeCount:         len(wf.Nodes),
		Progress:          progress,
		CreatedAt:         createdAt,
		Error:             wf.Error,
		AnalyzeMessage:    wf.AnalyzeMessage,
		GoalMode:          wf.GoalMode,
		GoalIteration:     wf.GoalIteration,
		GoalMaxIterations: wf.GoalMaxIterations,
		NeedsContinuation: m.consumeNeedsContinuation(wf.ID),
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
func (m *Manager) Control(ctx context.Context, wfID string, req ControlRequest) (*ControlResult, error) {
	_, span := m.tracer.Start(ctx, "workflow.manager.control",
		trace.WithAttributes(attribute.String("workflow.id", wfID)))
	defer span.End()

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
		// 分析阶段尚未产生任何子任务：bot 在此阶段主动终止几乎总是误判
		// （例如把模型"思考/首 token 延迟"当成卡死）。拒绝终止，让需求分析
		// 继续完成，避免杀死一个本可成功的工作流。进入 running 后仍可正常终止。
		if wf.Status == WorkflowAnalyzing {
			return nil, errs.New("任务仍在分析中，暂不能终止；请等待分析完成（进入 running）后再终止，或修正需求后重新提交")
		}
		// 在锁内读取 scheduler/cancel，避免与 runScheduler 的写入产生 data race
		m.mu.RLock()
		scheduler := inst.scheduler
		cancelFn := inst.cancel
		m.mu.RUnlock()
		if scheduler != nil {
			scheduler.Terminate()
		} else {
			// analyzing 阶段 scheduler 尚未创建，取消 context 中断分析
			cancelFn()
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

		// 工作流仍在跑：交给活跃 scheduler 处理（它掌握当前的并发与依赖状态）。
		if ok {
			m.mu.RLock()
			scheduler := inst.scheduler
			m.mu.RUnlock()
			if scheduler != nil {
				scheduler.SubmitRetry(req.NodeID)
				return &ControlResult{
					WorkflowID: wfID,
					Action:     "retry",
					Success:    true,
					Message:    fmt.Sprintf("节点 %s 的重试请求已提交", req.NodeID),
				}, nil
			}
			// 已注册实例但 scheduler 尚未创建（analyzing 阶段）：此时还没有节点可重试。
			return nil, errs.New("任务仍在分析中，暂无可重试的子任务")
		}

		// 工作流已进入终态（failed/terminated/completed）：调度器早已退出、running
		// 实例被 cleanupRunning 清理。旧逻辑在此直接报
		// "workflow is not actively running, cannot retry"，使 UI 上的「重试」按钮
		// 恰好在最需要它的时刻（节点失败、工作流收尾之后）完全不可用。
		// 正确做法是重新拉起调度，从该节点续跑。
		return m.restartFromNode(wf, req.NodeID)

	default:
		return nil, errs.Newf("unknown action: %s (use 'retry' or 'terminate')", req.Action)
	}
}

// ============================================================================
// 内部辅助
// ============================================================================

// resetForRetry 把目标节点及被级联跳过的下游节点重置为pending，返回复活的下游数量。
//
// 抽成独立函数是为了让「重置了哪些状态」可以被单独验证——一旦调度 goroutine 起来，
// 它会立刻改写这些字段，测试再去断言就变成和后台抢时序。
//
// 已 completed 的节点**刻意不动**：重试一个失败节点不该让前面成功的工作重跑，
// 那会白烧大量模型调用。
func resetForRetry(wf *Workflow, nodeID string) int {
	revived := 0
	for _, n := range wf.Nodes {
		switch {
		case n.ID == nodeID:
			n.Status = NodePending
			n.Error = ""
			n.RetryCount = 0
			n.IterationCount = 0
			n.StartedAt = nil
			n.CompletedAt = nil
		case n.Status == NodeSkipped:
			// 级联跳过的下游必须复活，否则目标节点跑完后它们仍是终态，
			// 调度器会直接收尾，整个重试等于白做。
			n.Status = NodePending
			n.Error = ""
			n.RetryCount = 0
			n.StartedAt = nil
			n.CompletedAt = nil
			revived++
		}
	}
	return revived
}

// restartFromNode 对已进入终态的工作流重新拉起调度，从指定节点续跑。
//
// 适用场景：节点失败 → 工作流被判failed → 调度器退出、running 实例被清理。
// 此时用户点「重试」，不能只把该节点标回 pending 就完事——没有调度器在跑，
// 没人会去执行它。必须重建调度实例。
//
// 重置范围不止目标节点：节点失败时下游会被**级联 skip**，若只重置目标节点，
// 它跑完后下游仍是 skipped 终态，调度器会直接收尾，整个重试等于白做。
// 因此这里同时把所有 skipped 节点放回 pending，让它们随依赖满足重新参与调度。
func (m *Manager) restartFromNode(wf *Workflow, nodeID string) (*ControlResult, error) {
	if _, exists := wf.GetNode(nodeID); !exists {
		return nil, errs.Newf("node %q not found in workflow %q", nodeID, wf.ID)
	}

	// 防止与已有实例重复启动（例如用户连点两次重试）。
	m.mu.RLock()
	_, alreadyRunning := m.running[wf.ID]
	m.mu.RUnlock()
	if alreadyRunning {
		return nil, errs.New("工作流已在运行中，无需重启调度")
	}

	wf.EnsureIndex()
	// 反序列化得到的 workflow 没有编译缓存，调度前必须重新编译。
	if !wf.Compiled() {
		if err := wf.Compile(); err != nil {
			return nil, errs.Wrapf(err, "DAG 编译失败，无法重试")
		}
	}

	// 重置目标节点及被级联跳过的下游。
	revived := resetForRetry(wf, nodeID)

	wf.Status = WorkflowRunning
	wf.Error = ""
	wf.FinishedAt = nil
	if err := m.repo.Save(wf); err != nil {
		return nil, errs.Wrapf(err, "failed to persist workflow before retry")
	}

	// 重建调度实例（与崩溃恢复同一套模式：先注册再启动，确保 runScheduler 能找到 instance）。
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
		defer func() {
			if r := recover(); r != nil {
				m.logger.Errorw("panic in retry workflow goroutine",
					"workflow_id", wf.ID, "panic", r)
				wf.Status = WorkflowFailed
				wf.Error = fmt.Sprintf("internal error: %v", r)
				_ = m.repo.Save(wf)
				m.metrics.Failed.Add(1)
				m.emitWorkflowEvent(context.Background(), wf.ID, outbound.EventWorkflowFailed, map[string]any{
					"error": wf.Error,
				})
				m.cleanupRunning(wf.ID)
			}
		}()
		m.metrics.Running.Add(1)
		defer m.metrics.Running.Add(-1)

		m.runScheduler(bgCtx, wf, 0)
	}(wf)

	m.logger.Infow("restarted terminal workflow for node retry",
		"workflow_id", wf.ID, "node_id", nodeID, "revived_skipped", revived)

	msg := fmt.Sprintf("已重新启动任务并重试节点 %s", nodeID)
	if revived > 0 {
		msg += fmt.Sprintf("（同时恢复 %d 个此前被跳过的子任务）", revived)
	}
	return &ControlResult{
		WorkflowID: wf.ID,
		Action:     "retry",
		Success:    true,
		Message:    msg,
	}, nil
}

func (m *Manager) cleanupRunning(wfID string) {
	m.mu.Lock()
	delete(m.running, wfID)
	m.mu.Unlock()
	// 终态已交付/已续跑，清理去重标记，避免映射无限增长。
	m.consumeMu.Lock()
	delete(m.consumed, wfID)
	m.consumeMu.Unlock()
}

// GetWorkflow 获取工作流领域对象（内部使用）。
func (m *Manager) GetWorkflow(wfID string) (*Workflow, error) {
	return m.repo.Get(wfID)
}

// ListWorkflows 列出最近的工作流（按创建时间降序）。
func (m *Manager) ListWorkflows(limit int) []*Workflow {
	result, err := m.repo.List(limit)
	if err != nil {
		m.logger.Errorw("failed to list workflows", "error", err)
	}
	return result
}

// sessionWorkflowScanLimit 是按会话查找工作流时扫描的最近工作流条数。
// 工作流以 JSON blob 存储，botId/sessionId 不是独立列，无法下推到 SQL，
// 只能取最近 N 条在内存里过滤。取 200 足够覆盖「当前会话最近一条」的场景，
// 又不至于把整表读进内存。
const sessionWorkflowScanLimit = 200

// LatestWorkflowForSession 返回指定 bot + 会话最近提交的一条工作流（没有则返回 nil）。
//
// 用途：前端刷新页面后 `activeWorkflowId` 会丢失（它只能从实时 SSE 事件里拿到），
// 工作流卡片随之消失，而工作流本身仍在后台运行。前端载入会话后调用本查询把卡片恢复出来。
//
// sessionID 为空时只按 botID 匹配 —— 非 web渠道提交的工作流没有会话概念。
// 历史工作流没有这两个字段，会被直接跳过（不会误配到别的会话）。
func (m *Manager) LatestWorkflowForSession(botID, sessionID string) *Workflow {
	if botID == "" {
		return nil
	}
	all, err := m.repo.List(sessionWorkflowScanLimit)
	if err != nil {
		m.logger.Errorw("failed to list workflows for session lookup", "error", err)
		return nil
	}
	// repo.List 已按 created_at 降序，第一个命中的就是最近的一条。
	for _, wf := range all {
		if wf == nil || wf.BotID != botID {
			continue
		}
		if sessionID != "" && wf.SessionID != sessionID {
			continue
		}
		return wf
	}
	return nil
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
