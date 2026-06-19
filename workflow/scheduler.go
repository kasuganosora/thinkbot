package workflow

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	noop_trace "go.opentelemetry.io/otel/trace/noop"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Scheduler — DAG 调度引擎
//
// 核心职责：
//   - 按 DAG 拓扑序调度节点：AND 依赖，所有前置 completed 后才执行
//   - 同层无依赖节点并行执行（semaphore 限流）
//   - 节点执行错误 → 自动重试（MaxRetries 次，指数退避）
//   - Review=true 的节点 → 执行后 Review，不通过则带反馈重执行（MaxIterations 次）
//   - 节点最终失败 → 下游节点级联 Skip
//   - 支持 Terminate 信号中断
// ============================================================================

// Scheduler 执行单个工作流的 DAG 调度。
type Scheduler struct {
	wf       *Workflow
	executor *Executor
	repo     *Repository
	ec       EngineConfig
	maxParallel int
	tracer   trace.Tracer
	logger   *zap.SugaredLogger
	emitter  *outbound.EventEmitter // 可为 nil

	mu         sync.Mutex     // 保护 wf.Nodes 状态读写
	sem        chan struct{}  // 并发限流 semaphore
	terminate  chan struct{}  // 终止信号（close to broadcast）
	terminated bool

	// 手动重试请求
	retryRequests chan string // nodeID

	wg sync.WaitGroup // 等待所有节点 goroutine
}

// SchedulerConfig 是 Scheduler 的配置。
type SchedulerConfig struct {
	MaxParallel int // 最大并行度（默认 3）
}

// NewScheduler 创建调度器。
func NewScheduler(wf *Workflow, executor *Executor, repo *Repository, cfg SchedulerConfig, ec EngineConfig, tp trace.TracerProvider, logger *zap.SugaredLogger, emitter *outbound.EventEmitter) *Scheduler {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	if emitter == nil {
		emitter = &outbound.EventEmitter{}
	}
	maxParallel := cfg.MaxParallel
	if maxParallel <= 0 {
		maxParallel = ec.MaxParallel
	}
	if maxParallel <= 0 {
		maxParallel = 3
	}
	return &Scheduler{
		wf:            wf,
		executor:      executor,
		repo:          repo,
		ec:            ec,
		maxParallel:   maxParallel,
		tracer:        tp.Tracer("github.com/kasuganosora/thinkbot/workflow/scheduler"),
		logger:        logger.With("component", "workflow_scheduler", "workflow_id", wf.ID),
		emitter:       emitter,
		sem:           make(chan struct{}, maxParallel),
		terminate:     make(chan struct{}),
		retryRequests: make(chan string, 16),
	}
}

// Run 阻塞执行工作流直到所有节点到达终态，或被 Terminate。
// 返回最终的工作流状态。
func (s *Scheduler) Run(ctx context.Context) WorkflowStatus {
	ctx, span := s.tracer.Start(ctx, "workflow.scheduler.run",
		trace.WithAttributes(
			attribute.String("workflow.id", s.wf.ID),
			attribute.Int("workflow.node_count", len(s.wf.Nodes)),
			attribute.Int("workflow.max_parallel", s.maxParallel),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, s.logger)
	logger.Infow("scheduler started", "nodes", len(s.wf.Nodes), "max_parallel", s.maxParallel)

	// 标记工作流为运行中 + 确保节点初始状态正确
	now := time.Now()
	s.wf.StartedAt = &now
	s.wf.Status = WorkflowRunning

	// 防御性初始化：非终态节点必须为 pending 才能被 ReadyNodes 选中
	for _, n := range s.wf.Nodes {
		if !n.Status.IsTerminal() && n.Status != NodePending {
			n.Status = NodePending
		}
	}
	s.persist()

	// 主调度循环
	tickerInterval := s.ec.ScheduleInterval
	if tickerInterval <= 0 {
		tickerInterval = 200 * time.Millisecond
	}
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()

	for {
		// 检查终止
		if s.isTerminated() {
			s.handleTerminate()
			break
		}

		// 检查是否全部完成
		s.mu.Lock()
		allTerminal := IsAllTerminal(s.wf)
		s.mu.Unlock()
		if allTerminal {
			break
		}

		// 处理手动重试请求
		s.drainRetryRequests()

		// 获取就绪节点并启动执行
		s.mu.Lock()
		ready := ReadyNodes(s.wf)
		for _, node := range ready {
			node.Status = NodeReady
		}
		s.mu.Unlock()

		for _, node := range ready {
			s.wg.Go(func() {
				s.runNode(ctx, node)
			})
		}

		// 等待下一轮检查
		select {
		case <-ctx.Done():
			s.handleTerminate()
			goto done
		case <-s.terminate:
			s.handleTerminate()
			goto done
		case <-ticker.C:
			// 继续循环
		}
	}

done:
	s.wg.Wait()

	// 计算最终状态
	finalStatus := s.computeFinalStatus()
	finishedAt := time.Now()
	s.wf.FinishedAt = &finishedAt
	s.wf.Status = finalStatus
	s.persist()

	span.SetAttributes(attribute.String("workflow.final_status", string(finalStatus)))
	logger.Infow("scheduler finished", "status", finalStatus)
	return finalStatus
}

// runNode 执行单个节点的完整生命周期：
// 1. 错误重试循环（MaxRetries，指数退避）
// 2. Review 自循环（如果 review=true）
func (s *Scheduler) runNode(ctx context.Context, node *DAGNode) {
	ctx, span := s.tracer.Start(ctx, "workflow.node.run",
		trace.WithAttributes(
			attribute.String("node.id", node.ID),
			attribute.String("node.name", node.Name),
			attribute.Bool("node.review", node.Review),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, s.logger)

	if s.isTerminated() {
		return
	}

	// 获取 semaphore
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-s.terminate:
		return
	case <-ctx.Done():
		return
	}

	s.mu.Lock()
	node.Status = NodeRunning
	startedAt := time.Now()
	node.StartedAt = &startedAt
	s.mu.Unlock()
	s.persist()

	s.emitNodeEvent(outbound.EventWorkflowNodeStarted, map[string]any{
		"node_id":   node.ID,
		"node_name": node.Name,
		"task":      strutil.Truncate(node.Task, 200),
	})

	// ================================================================
	// Phase 1: 执行 + 错误重试（使用 util/retry，指数退避 + panic recovery）
	// ================================================================
	maxRetries := node.MaxRetries
	if maxRetries <= 0 {
		maxRetries = s.ec.MaxRetries
		if maxRetries <= 0 {
			maxRetries = 2 // 默认重试 2 次
		}
	}

	var result string
	var lastErr error

	retryRes := retry.Do(ctx, "workflow_node_"+node.ID, retry.Config{
		MaxRetries: maxRetries,
		Backoff: &retry.Backoff{
			Strategy: retry.StrategyExponential,
			Initial:  s.ec.RetryInitial,
			Max:      s.ec.RetryMax,
		},
		OnRetry: func(attempt int, err error, wait time.Duration) {
			s.mu.Lock()
			node.RetryCount = attempt
			node.Error = err.Error()
			s.mu.Unlock()
			s.persist()

			s.emitNodeEvent(outbound.EventWorkflowNodeRetrying, map[string]any{
				"node_id":   node.ID,
				"attempt":   attempt,
				"max_retries": maxRetries,
				"error":     err.Error(),
			})

			logger.Warnw("node execution failed, retrying",
				"node_id", node.ID,
				"attempt", attempt,
				"max_retries", maxRetries,
				"wait", wait,
				"error", err)

			span.AddEvent("retry", trace.WithAttributes(
				attribute.Int("attempt", attempt),
				attribute.String("error", err.Error()),
			))
		},
	}, func(ctx context.Context) error {
		if s.isTerminated() {
			return errs.New("workflow terminated")
		}
		execResult, err := s.executor.Execute(ctx, node)
		if err != nil {
			return err
		}
		result = execResult
		return nil
	})

	if retryRes.Err != nil {
		lastErr = retryRes.Err
	}

	if lastErr != nil {
		// 所有重试耗尽
		span.RecordError(lastErr)
		span.SetAttributes(attribute.String("node.final_status", "failed"))
		s.mu.Lock()
		node.Status = NodeFailed
		node.Error = lastErr.Error()
		completedAt := time.Now()
		node.CompletedAt = &completedAt
		s.mu.Unlock()
		s.persist()

		s.emitNodeEvent(outbound.EventWorkflowNodeFailed, map[string]any{
			"node_id":      node.ID,
			"retry_count":  node.RetryCount,
			"error":        lastErr.Error(),
		})

		// 级联跳过下游节点
		s.mu.Lock()
		CascadeSkip(s.wf, node.ID)
		s.mu.Unlock()
		s.persist()
		return
	}

	// 清除执行阶段的错误信息
	s.mu.Lock()
	node.Error = ""
	node.Result = result
	s.mu.Unlock()

	// ================================================================
	// Phase 2: Review 自循环（仅 review=true 的节点）
	// ================================================================
	if node.Review {
		finalResult, err := s.reviewLoop(ctx, node, result)
		if err != nil {
			span.RecordError(err)
			span.SetAttributes(attribute.String("node.final_status", "failed"))
			s.mu.Lock()
			node.Status = NodeFailed
			node.Error = err.Error()
			node.Result = finalResult
			completedAt := time.Now()
			node.CompletedAt = &completedAt
			s.mu.Unlock()
			s.persist()

			// 级联跳过
			s.mu.Lock()
			CascadeSkip(s.wf, node.ID)
			s.mu.Unlock()
			s.persist()
			return
		}
		result = finalResult
	}

	// 成功完成
	span.SetAttributes(attribute.String("node.final_status", "completed"))
	s.mu.Lock()
	node.Status = NodeCompleted
	node.Result = result
	node.Error = ""
	completedAt := time.Now()
	node.CompletedAt = &completedAt
	s.mu.Unlock()
	s.persist()

	s.emitNodeEvent(outbound.EventWorkflowNodeCompleted, map[string]any{
		"node_id":         node.ID,
		"retry_count":     node.RetryCount,
		"iteration_count": node.IterationCount,
		"result_preview":  strutil.Truncate(result, 500),
	})

	logger.Infow("node completed", "node_id", node.ID,
		"retries", node.RetryCount, "iterations", node.IterationCount)
}

// reviewLoop 执行 Review 自循环：
// 反复 Review → 不通过则带反馈重新执行 → 直到通过或超过 MaxIterations。
//
// 流程（maxIter=3 为例）：
//
//	iter 0: review(initialResult) → pass? done : re-execute → result1
//	iter 1: review(result1)       → pass? done : re-execute → result2
//	iter 2: review(result2)       → pass? done : re-execute → result3
//	（循环结束后再 review result3，仍不通过则失败）
//
// 即：共 maxIter 次 review + 最多 maxIter 次 re-execute + 1 次最终 review。
func (s *Scheduler) reviewLoop(ctx context.Context, node *DAGNode, initialResult string) (string, error) {
	maxIter := node.MaxIterations
	if maxIter <= 0 {
		maxIter = s.ec.MaxIterations
		if maxIter <= 0 {
			maxIter = 3 // 默认最多 3 轮迭代
		}
	}

	result := initialResult

	for iter := 0; iter < maxIter; iter++ {
		if s.isTerminated() {
			return result, errs.New("terminated during review")
		}

		// 设置 Review 状态
		s.mu.Lock()
		node.Status = NodeReviewing
		s.mu.Unlock()
		s.persist()

		s.emitNodeEvent(outbound.EventWorkflowNodeReviewing, map[string]any{
			"node_id":   node.ID,
			"iteration": iter + 1,
		})

		// 执行 Review
		reviewResult, err := s.executor.Review(ctx, node, result)
		if err != nil {
			return result, errs.Wrapf(err, "review error at iteration %d", iter+1)
		}

		// 记录 Review 历史
		s.mu.Lock()
		node.ReviewHistory = append(node.ReviewHistory, ReviewRecord{
			Iteration: iter + 1,
			Passed:    reviewResult.Passed,
			Feedback:  reviewResult.Feedback,
		})
		s.mu.Unlock()

		if reviewResult.Passed {
			s.logger.Infow("review passed", "node_id", node.ID, "iteration", iter+1)
			return result, nil
		}

		// Review 未通过，准备重新执行
		s.logger.Infow("review failed, re-executing",
			"node_id", node.ID, "iteration", iter+1, "max_iterations", maxIter)

		s.mu.Lock()
		node.IterationCount = iter + 1
		node.ReviewFeedback = reviewResult.Feedback
		node.Status = NodeRunning
		s.mu.Unlock()
		s.persist()

		// 带反馈重新执行
		newResult, execErr := s.executor.ExecuteWithFeedback(ctx, node, result, reviewResult.Feedback)
		if execErr != nil {
			return result, errs.Wrapf(execErr, "re-execution failed at iteration %d", iter+1)
		}
		result = newResult

		s.mu.Lock()
		node.Result = result
		s.mu.Unlock()
	}

	// 超过最大迭代次数
	return result, errs.Newf("node %q exceeded max review iterations (%d), last feedback: %s",
		node.ID, maxIter, strutil.Truncate(node.ReviewFeedback, 200))
}

// ============================================================================
// 终止与重试控制
// ============================================================================

// Terminate 发送终止信号。
func (s *Scheduler) Terminate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.terminated {
		s.terminated = true
		close(s.terminate)
	}
}

// RequestRetry 请求手动重试指定节点（将其从终态恢复为 pending）。
// 只有 Failed 或 Skipped 的节点可以被重试。
func (s *Scheduler) RequestRetry(nodeID string) error {
	s.mu.Lock()
	node, ok := s.wf.GetNode(nodeID)
	if !ok {
		s.mu.Unlock()
		return errs.Newf("node %q not found", nodeID)
	}
	if node.Status != NodeFailed && node.Status != NodeSkipped {
		s.mu.Unlock()
		return errs.Newf("node %q is in status %s, only failed/skipped nodes can be retried", nodeID, node.Status)
	}

	// 重置节点状态
	node.Status = NodePending
	node.Error = ""
	node.Result = ""
	node.RetryCount = 0
	node.IterationCount = 0
	node.ReviewFeedback = ""
	node.ReviewHistory = nil
	node.CompletedAt = nil

	// 同时取消下游被级联跳过的节点
	s.unskipDependents(nodeID)
	s.mu.Unlock()
	s.persist()

	s.logger.Infow("node retry requested", "node_id", nodeID)
	return nil
}

// unskipDependents 将因指定节点失败而被跳过的下游节点恢复为 pending。
func (s *Scheduler) unskipDependents(nodeID string) {
	for _, n := range s.wf.Nodes {
		if n.Status != NodeSkipped {
			continue
		}
		for _, dep := range n.Dependencies {
			if dep == nodeID {
				// 检查该节点是否还依赖其他失败/跳过的节点
				allDepsOk := true
				for _, d := range n.Dependencies {
					depNode, ok := s.wf.GetNode(d)
					if !ok || depNode.Status == NodeFailed || depNode.Status == NodeSkipped {
						allDepsOk = false
						break
					}
				}
				if allDepsOk {
					n.Status = NodePending
					n.Error = ""
					s.unskipDependents(n.ID) // 递归恢复
				}
				break
			}
		}
	}
}

// ============================================================================
// 内部辅助
// ============================================================================

func (s *Scheduler) isTerminated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminated
}

func (s *Scheduler) handleTerminate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.wf.Nodes {
		if !n.Status.IsTerminal() {
			n.Status = NodeSkipped
			n.Error = "workflow terminated"
			now := time.Now()
			n.CompletedAt = &now
		}
	}
}

func (s *Scheduler) drainRetryRequests() {
	for {
		select {
		case nodeID := <-s.retryRequests:
			_ = s.RequestRetry(nodeID)
		default:
			return
		}
	}
}

func (s *Scheduler) persist() {
	if s.repo != nil {
		if err := s.repo.Save(s.wf); err != nil {
			s.logger.Errorw("failed to persist workflow state", "error", err)
		}
	}
}

// emitNodeEvent 发布节点级事件（workflow_id 作为 TraceID）。
func (s *Scheduler) emitNodeEvent(eventType outbound.EventType, data map[string]any) {
	s.emitter.Emit(context.Background(), eventType, s.wf.ID, data)
}

func (s *Scheduler) computeFinalStatus() WorkflowStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 直接读取 s.terminated 字段，不能调用 s.isTerminated()（会再次 Lock 导致死锁）
	if s.terminated {
		return WorkflowTerminated
	}

	hasFailed := false
	allCompleted := true
	for _, n := range s.wf.Nodes {
		if n.Status == NodeFailed {
			hasFailed = true
		}
		if n.Status != NodeCompleted && n.Status != NodeSkipped {
			allCompleted = false
		}
	}

	if hasFailed {
		return WorkflowFailed
	}
	if allCompleted {
		return WorkflowCompleted
	}
	return WorkflowFailed
}

// SubmitRetry 向调度器提交手动重试请求（线程安全）。
func (s *Scheduler) SubmitRetry(nodeID string) {
	select {
	case s.retryRequests <- nodeID:
	default:
		s.logger.Warnw("retry request channel full, dropping", "node_id", nodeID)
	}
}

// String 返回调度器的可读描述。
func (s *Scheduler) String() string {
	return fmt.Sprintf("Scheduler(wf=%s, parallel=%d)", s.wf.ID, s.maxParallel)
}
