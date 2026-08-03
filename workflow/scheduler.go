package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

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

// NodeExecutor 抽象节点执行逻辑，使 Scheduler 可独立测试。
// *Executor 是默认实现。
type NodeExecutor interface {
	Execute(ctx context.Context, node *DAGNode) (string, error)
	ExecuteWithFeedback(ctx context.Context, node *DAGNode, prevResult, feedback string) (string, error)
	Review(ctx context.Context, node *DAGNode, product string) (*ReviewResult, error)
}

// errGoalFeedback 是目标模式闭环的哨兵错误：review 节点在节点级迭代仍不通过，
// 但工作流开启了 GoalMode 且仍有闭环额度，Scheduler 已将本节点及其 Feedback 目标节点
// 回退为 pending，由主调度循环重新执行。runNode 捕获该错误后不标记失败、不级联跳过，
// 直接返回，让闭环继续。
var errGoalFeedback = errors.New("goal-mode feedback loop")

// Scheduler 执行单个工作流的 DAG 调度。
type Scheduler struct {
	wf          *Workflow
	executor    NodeExecutor
	repo        *Repository
	ec          EngineConfig
	maxParallel int
	tracer      trace.Tracer
	logger      *zap.SugaredLogger
	emitter     *outbound.EventEmitter // 可为 nil
	metrics     *ManagerMetrics        // 可为 nil（测试时）

	mu         sync.Mutex    // 保护 wf.Nodes 状态读写
	sem        chan struct{} // 并发限流 semaphore
	terminate  chan struct{} // 终止信号（close to broadcast）
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
func NewScheduler(wf *Workflow, executor NodeExecutor, repo *Repository, cfg SchedulerConfig, ec EngineConfig, tp trace.TracerProvider, logger *zap.SugaredLogger, emitter *outbound.EventEmitter, metrics *ManagerMetrics) *Scheduler {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	if emitter == nil {
		emitter = &outbound.EventEmitter{}
	}
	if metrics == nil {
		metrics = &ManagerMetrics{}
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
		metrics:       metrics,
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
	s.mu.Lock()
	s.wf.StartedAt = &now
	s.wf.Status = WorkflowRunning

	// 防御性初始化：非终态节点必须为 pending 才能被 ReadyNodes 选中
	for _, n := range s.wf.Nodes {
		if !n.Status.IsTerminal() && n.Status != NodePending {
			n.Status = NodePending
		}
	}
	s.mu.Unlock()

	// 发布工作流开始执行事件
	s.emitter.Emit(ctx, outbound.EventWorkflowRunning, s.wf.ID, map[string]any{
		"node_count":   len(s.wf.Nodes),
		"max_parallel": s.maxParallel,
	})

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
	s.mu.Lock()
	finishedAt := time.Now()
	s.wf.FinishedAt = &finishedAt
	s.wf.Status = finalStatus
	s.mu.Unlock()
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

	nodeStart := time.Now()
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
	// 再次检查终止状态，防止 handleTerminate 在 isTerminated() 检查
	// 和获取 semaphore 之间的窗口期已将节点设为 NodeSkipped。
	if s.terminated {
		s.mu.Unlock()
		return
	}
	node.Status = NodeRunning
	startedAt := time.Now()
	node.StartedAt = &startedAt
	s.mu.Unlock()
	s.persist()

	s.metrics.NodeExecuted.Add(1)

	s.emitNodeEvent(ctx, outbound.EventWorkflowNodeStarted, map[string]any{
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

	// 目标模式闭环：若本节点被上一轮 review 打回了修复意见（LoopFeedback），
	// 则带反馈重新执行；否则走普通执行。读取后立即清空，避免影响后续重试。
	s.mu.Lock()
	loopFb := node.LoopFeedback
	node.LoopFeedback = ""
	s.mu.Unlock()

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

			s.metrics.NodeRetries.Add(1)

			s.emitNodeEvent(ctx, outbound.EventWorkflowNodeRetrying, map[string]any{
				"node_id":     node.ID,
				"attempt":     attempt,
				"max_retries": maxRetries,
				"error":       err.Error(),
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
		// 注入上游结果上下文（编译后的 Workflow 自动生效）
		// 注意：不修改 node.Task 以避免重试时累加
		taskContext := BuildUpstreamContext(s.wf, node)
		effectiveTask := node.Task
		if taskContext != "" {
			effectiveTask = taskContext
		}
		// 用含上游上下文的临时 task 执行，不改变 node 本身
		originalTask := node.Task
		node.Task = effectiveTask
		var execResult string
		var err error
		if loopFb != "" {
			// 目标模式闭环：带上一轮审查意见重跑（prevResult 传空，意见即修复依据）
			execResult, err = s.executor.ExecuteWithFeedback(ctx, node, "", loopFb)
		} else {
			execResult, err = s.executor.Execute(ctx, node)
		}
		node.Task = originalTask // 恢复原始 task
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
		// 如果执行期间被 terminate，handleTerminate 已设 NodeSkipped，不覆盖
		if s.isTerminated() {
			return
		}
		// 所有重试耗尽
		span.RecordError(lastErr)
		span.SetAttributes(
			attribute.String("node.final_status", "failed"),
			attribute.Int64("node.duration_ms", time.Since(nodeStart).Milliseconds()),
		)
		s.metrics.NodeFailed.Add(1)
		s.mu.Lock()
		node.Status = NodeFailed
		node.Error = lastErr.Error()
		completedAt := time.Now()
		node.CompletedAt = &completedAt
		s.mu.Unlock()
		s.persist()

		s.emitNodeEvent(ctx, outbound.EventWorkflowNodeFailed, map[string]any{
			"node_id":     node.ID,
			"retry_count": node.RetryCount,
			"error":       lastErr.Error(),
		})

		// 级联跳过下游节点
		s.mu.Lock()
		skippedIDs := CascadeSkip(s.wf, node.ID)
		s.mu.Unlock()
		s.persist()

		s.emitCascadeSkipEvent(ctx, node.ID, skippedIDs)
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
			// 目标模式闭环哨兵：review 不通过但仍有闭环额度，节点已被回退为 pending，
			// 由主调度循环重新执行 Feedback 目标节点。此处直接返回，不标记失败、不级联跳过。
			if errors.Is(err, errGoalFeedback) {
				return
			}
			if s.isTerminated() {
				return
			}
			span.RecordError(err)
			span.SetAttributes(
				attribute.String("node.final_status", "failed"),
				attribute.Int64("node.duration_ms", time.Since(nodeStart).Milliseconds()),
			)
			s.metrics.NodeFailed.Add(1)
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
			skippedIDs := CascadeSkip(s.wf, node.ID)
			s.mu.Unlock()
			s.persist()

			s.emitCascadeSkipEvent(ctx, node.ID, skippedIDs)
			return
		}
		result = finalResult
	}

	// 成功完成（如果在此期间被 terminate，不覆盖已设置的 NodeSkipped）
	if s.isTerminated() {
		return
	}
	span.SetAttributes(
		attribute.String("node.final_status", "completed"),
		attribute.Int64("node.duration_ms", time.Since(nodeStart).Milliseconds()),
	)
	s.mu.Lock()
	node.Status = NodeCompleted
	node.Result = result
	node.Error = ""
	completedAt := time.Now()
	node.CompletedAt = &completedAt
	s.mu.Unlock()
	s.persist()

	s.emitNodeEvent(ctx, outbound.EventWorkflowNodeCompleted, map[string]any{
		"node_id":         node.ID,
		"retry_count":     node.RetryCount,
		"iteration_count": node.IterationCount,
		"result_preview":  strutil.Truncate(result, 500),
	})

	logger.Infow("node completed", "node_id", node.ID,
		"retries", node.RetryCount, "iterations", node.IterationCount)
}

// reviewInfraMaxAttempts 是单次审查在遇到基础设施错误时的最大尝试次数。
// 3 次配合退避足以吃掉常见的模型网关抖动，又不至于把失败无限拖长。
const reviewInfraMaxAttempts = 3

// reviewInfraRetryBaseDelay 是基础设施错误重试的基础退避间隔（指数增长）。
const reviewInfraRetryBaseDelay = 2 * time.Second

// reviewWithInfraRetry 执行一次审查，并对「基础设施类错误」就地重试。
//
// 区分两类错误至关重要（见 review_error.go）：
//   - 基础设施错误（LLM 超时 / 限流 / 网关抖动）：模型没能给出结论 → 重试审查本身。
//   - 其他错误：原样上抛，交由调用方判失败。
//
// 审查「结论为不通过」不走这里——那是正常返回值（ReviewResult.Passed=false），
// 由 reviewLoop 触发带反馈的重新执行。
func (s *Scheduler) reviewWithInfraRetry(ctx context.Context, node *DAGNode, product string, iteration int) (*ReviewResult, error) {
	var lastErr error
	for attempt := 1; attempt <= reviewInfraMaxAttempts; attempt++ {
		if s.isTerminated() {
			return nil, errs.New("terminated during review")
		}

		res, err := s.executor.Review(ctx, node, product)
		if err == nil {
			if attempt > 1 {
				s.logger.Infow("review succeeded after infra retry",
					"node_id", node.ID, "iteration", iteration, "attempt", attempt)
			}
			return res, nil
		}
		lastErr = err

		// 非基础设施错误：不重试，立刻上抛。
		if !isReviewInfraError(err) {
			return nil, err
		}
		// 上层 ctx 已结束（终止或整体超时）：继续重试没有意义。
		if ctx.Err() != nil {
			return nil, err
		}
		// 已是最后一次尝试：不再等待，直接上抛。
		if attempt == reviewInfraMaxAttempts {
			break
		}

		delay := reviewInfraRetryBaseDelay * time.Duration(1<<(attempt-1))
		s.logger.Warnw("review hit infrastructure error, retrying",
			"node_id", node.ID, "iteration", iteration,
			"attempt", attempt, "max_attempts", reviewInfraMaxAttempts,
			"delay", delay.String(), "err", err)

		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delay):
		}
	}

	s.logger.Errorw("review failed after exhausting infra retries",
		"node_id", node.ID, "iteration", iteration,
		"attempts", reviewInfraMaxAttempts, "err", lastErr)
	return nil, lastErr
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

	ctx, span := s.tracer.Start(ctx, "workflow.node.review_loop",
		trace.WithAttributes(
			attribute.String("node.id", node.ID),
			attribute.Int("max_iterations", maxIter),
		))
	defer span.End()

	result := initialResult

	for iter := 0; iter < maxIter; iter++ {
		if s.isTerminated() {
			return result, errs.New("terminated during review")
		}

		// 设置 Review 状态
		s.mu.Lock()
		if s.terminated {
			s.mu.Unlock()
			return result, errs.New("terminated during review")
		}
		node.Status = NodeReviewing
		s.mu.Unlock()
		s.persist()

		s.metrics.NodeReviews.Add(1)

		s.emitNodeEvent(ctx, outbound.EventWorkflowNodeReviewing, map[string]any{
			"node_id":   node.ID,
			"iteration": iter + 1,
		})

		// 执行 Review（带基础设施错误重试）
		//
		// 「模型没能给出审查结论」≠「审查结论是不通过」。LLM 超时/限流/网关抖动属于前者，
		// 直接判失败会把好产物连同整个 workflow 一起废掉（下游全部 skipped）。
		// 这里对基础设施类错误就地重试若干次；仍失败才上抛。
		reviewResult, err := s.reviewWithInfraRetry(ctx, node, result, iter+1)
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
		if s.terminated {
			s.mu.Unlock()
			return result, errs.New("terminated during review")
		}
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

	// 目标模式闭环：节点级迭代（MaxIterations）仍不通过，但工作流开启了 GoalMode、
	// 存在 Feedback 回退边、且全局迭代额度未耗尽 → 回退到 Feedback 目标节点重跑，
	// 形成「工作→审查→修复→审查」的全局闭环，而非立即失败。
	if s.wf.GoalMode && len(node.Feedback) > 0 && s.goalCanIterate() {
		feedback := node.ReviewFeedback
		if feedback == "" {
			feedback = "(review failed, no detailed feedback)"
		}
		if s.isTerminated() {
			return result, errs.New("terminated during goal feedback")
		}
		if err := s.goalFeedbackReset(ctx, node, feedback); err != nil {
			return result, err
		}
		return result, errGoalFeedback
	}

	// 超过最大迭代次数（且目标模式不可用/额度耗尽）
	feedback := node.ReviewFeedback
	if feedback == "" {
		feedback = "(no feedback)"
	}
	return result, errs.Newf("node %q exceeded max review iterations (%d), last feedback: %s",
		node.ID, maxIter, strutil.Truncate(feedback, 200))
}

// goalCanIterate 返回目标模式是否还有闭环额度。优先使用工作流级 GoalMaxIterations，
// 否则回退到引擎默认配置，最后兜底为 5。
func (s *Scheduler) goalCanIterate() bool {
	max := s.wf.GoalMaxIterations
	if max <= 0 {
		max = s.ec.GoalMaxIterations
	}
	if max <= 0 {
		max = 5
	}
	return s.wf.GoalIteration < max
}

// goalFeedbackReset 执行目标模式闭环的「回退」：
//   - 将 review 节点与它的 Feedback 目标节点重置为 pending（连同清空运行态）；
//   - 把审查意见写入每个目标节点的 LoopFeedback，下一轮执行时作为修复依据注入；
//   - 全局闭环计数 GoalIteration +1。
//
// 调用方（reviewLoop）随后返回 errGoalFeedback，runNode 据此直接返回，
// 主调度循环会重新挑选就绪的目标节点（及其下游 review 节点）执行。
// 注意：Feedback 目标节点的「其它下游」不会被重置——若 review 节点是 DAG 的终点，
// 则其所有下游仅含自身，重置即完整重跑；若目标节点另有其它下游，它们不会看到新结果
// （目标模式的预期用法是 review 节点作为最终检查点）。
func (s *Scheduler) goalFeedbackReset(ctx context.Context, node *DAGNode, feedback string) error {
	s.mu.Lock()
	s.wf.GoalIteration++

	// 重置 review 节点自身
	node.Status = NodePending
	node.Error = ""
	node.Result = ""
	node.RetryCount = 0
	node.IterationCount = 0
	node.ReviewHistory = nil
	node.ReviewFeedback = ""
	node.CompletedAt = nil

	// 重置 Feedback 目标节点，并写入审查意见
	for _, fid := range node.Feedback {
		fn, ok := s.wf.GetNode(fid)
		if !ok {
			continue
		}
		fn.Status = NodePending
		fn.Error = ""
		fn.Result = ""
		fn.RetryCount = 0
		fn.IterationCount = 0
		fn.ReviewHistory = nil
		fn.CompletedAt = nil
		fn.LoopFeedback = feedback
	}
	s.mu.Unlock()

	s.persist()

	s.emitNodeEvent(ctx, outbound.EventWorkflowNodeReviewing, map[string]any{
		"node_id":      node.ID,
		"goal_mode":    true,
		"goal_round":   s.wf.GoalIteration,
		"loop_back_to": node.Feedback,
		"feedback":     strutil.Truncate(feedback, 300),
	})
	s.logger.Infow("goal-mode feedback loop",
		"node_id", node.ID, "goal_round", s.wf.GoalIteration, "loop_back_to", node.Feedback)
	return nil
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
	s.unskipDependentsDepth(nodeID, 0)
}

// unskipDependentsDepth 带递归深度保护的 unskipDependents 实现。
func (s *Scheduler) unskipDependentsDepth(nodeID string, depth int) {
	if depth > 1000 {
		s.logger.Warnw("unskipDependents recursion depth exceeded limit",
			"node_id", nodeID, "depth", depth)
		return
	}
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
					s.unskipDependentsDepth(n.ID, depth+1) // 递归恢复
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
			s.metrics.NodeSkipped.Add(1)
		}
	}
}

func (s *Scheduler) drainRetryRequests() {
	for {
		select {
		case nodeID := <-s.retryRequests:
			if err := s.RequestRetry(nodeID); err != nil {
				s.logger.Warnw("failed to process retry request",
					"node_id", nodeID, "error", err)
			}
		default:
			return
		}
	}
}

func (s *Scheduler) persist() {
	if s.repo == nil {
		return
	}
	// 在锁内克隆快照，避免序列化过程中其他 goroutine 并发修改 wf.Nodes
	s.mu.Lock()
	snapshot := cloneWorkflow(s.wf)
	s.mu.Unlock()
	if err := s.repo.Save(snapshot); err != nil {
		s.logger.Errorw("failed to persist workflow state", "error", err)
		s.metrics.PersistErrors.Add(1)
	}
}

// emitNodeEvent 发布节点级事件（透传 ctx 保持 trace 链路）。
func (s *Scheduler) emitNodeEvent(ctx context.Context, eventType outbound.EventType, data map[string]any) {
	s.emitter.Emit(ctx, eventType, s.wf.ID, data)
}

// emitCascadeSkipEvent 发布级联跳过事件。直接使用 CascadeSkip 返回的 skippedIDs，
// 避免通过字符串匹配过滤节点。
func (s *Scheduler) emitCascadeSkipEvent(ctx context.Context, failedNodeID string, skippedIDs []string) {
	if len(skippedIDs) > 0 {
		s.emitNodeEvent(ctx, outbound.EventWorkflowNodeSkipped, map[string]any{
			"caused_by":   failedNodeID,
			"skipped_ids": skippedIDs,
		})
	}
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
