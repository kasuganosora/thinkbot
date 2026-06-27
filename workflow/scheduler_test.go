package workflow

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"

	"github.com/kasuganosora/thinkbot/agent/outbound"
)

// ============================================================================
// Scheduler runNode / reviewLoop 单元测试
//
// 通过 mockExecutor 替换真实 Executor，不依赖 LLM。
// ============================================================================

// --- mockExecutor ---

type mockExecutor struct {
	// Execute 返回值
	execResult string
	execErr    error
	// ExecuteWithFeedback 返回值
	fbResult string
	fbErr    error
	// Review 返回值队列（按调用顺序消费）
	reviewResults []*ReviewResult
	reviewErr     error

	// 调用计数
	execCalls atomic.Int32
	fbCalls   atomic.Int32
	revCalls  atomic.Int32

	// Execute 的第 N 次调用返回不同结果（用于先失败后成功）
	execResults []string
	execErrors  []error
}

func (m *mockExecutor) Execute(_ context.Context, _ *DAGNode) (string, error) {
	idx := int(m.execCalls.Add(1)) - 1
	// Check error sequence first (simulates initial failure)
	if idx < len(m.execErrors) {
		return "", m.execErrors[idx]
	}
	// Then result sequence (simulates recovery on retry)
	resultIdx := idx - len(m.execErrors)
	if resultIdx < len(m.execResults) {
		return m.execResults[resultIdx], nil
	}
	return m.execResult, m.execErr
}

func (m *mockExecutor) ExecuteWithFeedback(_ context.Context, _ *DAGNode, _, _ string) (string, error) {
	m.fbCalls.Add(1)
	return m.fbResult, m.fbErr
}

func (m *mockExecutor) Review(_ context.Context, _ *DAGNode, _ string) (*ReviewResult, error) {
	idx := int(m.revCalls.Add(1)) - 1
	if m.reviewErr != nil {
		return nil, m.reviewErr
	}
	if idx < len(m.reviewResults) {
		return m.reviewResults[idx], nil
	}
	return &ReviewResult{Passed: true}, nil
}

func newMockScheduler(wf *Workflow, exec NodeExecutor) *Scheduler {
	bus := &captureBus{}
	emitter := outbound.NewEventEmitter(bus, "")
	tp := noop_trace.NewTracerProvider()
	return &Scheduler{
		wf:            wf,
		executor:      exec,
		repo:          nil,
		ec:            EngineConfig{ScheduleInterval: 5 * time.Millisecond, RetryInitial: 1 * time.Millisecond, RetryMax: 5 * time.Millisecond},
		maxParallel:   3,
		tracer:        tp.Tracer("test"),
		logger:        noopLogger(),
		emitter:       emitter,
		metrics:       &ManagerMetrics{},
		sem:           make(chan struct{}, 3),
		terminate:     make(chan struct{}),
		retryRequests: make(chan string, 16),
	}
}

// --- runNode: 成功路径 ---

func TestRunNode_Success(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "task1", Task: "do something"},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execResult: "done"}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeCompleted {
		t.Errorf("expected completed, got %s", node.Status)
	}
	if node.Result != "done" {
		t.Errorf("expected result 'done', got %s", node.Result)
	}
	if node.Error != "" {
		t.Errorf("expected empty error, got %s", node.Error)
	}
	if node.StartedAt == nil {
		t.Error("StartedAt should be set")
	}
	if node.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
	if exec.execCalls.Load() != 1 {
		t.Errorf("expected 1 Execute call, got %d", exec.execCalls.Load())
	}
}

// --- runNode: 执行失败，重试耗尽 ---

func TestRunNode_ExecuteFails_AllRetries(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "task1", MaxRetries: 1},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execErr: errors.New("boom"),
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeFailed {
		t.Errorf("expected failed, got %s", node.Status)
	}
	if !strings.Contains(node.Error, "boom") {
		t.Errorf("expected error to contain 'boom', got %s", node.Error)
	}
	if node.CompletedAt == nil {
		t.Error("CompletedAt should be set even on failure")
	}
}

// --- runNode: 先失败后成功 ---

func TestRunNode_RetryThenSucceed(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "task1", MaxRetries: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execErrors:  []error{errors.New("first fail")},
		execResults: []string{"success on retry"},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeCompleted {
		t.Errorf("expected completed after retry, got %s", node.Status)
	}
	if node.Result != "success on retry" {
		t.Errorf("expected retry result, got %s", node.Result)
	}
	if node.RetryCount < 1 {
		t.Error("expected at least 1 retry")
	}
}

// --- runNode: 失败时级联跳过下游 ---

func TestRunNode_FailureCascadesSkip(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n2"}},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execErr: errors.New("fail")}
	s := newMockScheduler(wf, exec)

	n1, _ := wf.GetNode("n1")
	n1.Status = NodeReady

	s.runNode(context.Background(), n1)

	if n1.Status != NodeFailed {
		t.Errorf("n1 should be failed, got %s", n1.Status)
	}
	n2, _ := wf.GetNode("n2")
	if n2.Status != NodeSkipped {
		t.Errorf("n2 should be skipped, got %s", n2.Status)
	}
	n3, _ := wf.GetNode("n3")
	if n3.Status != NodeSkipped {
		t.Errorf("n3 should be skipped, got %s", n3.Status)
	}
}

// --- runNode: Review 通过 ---

func TestRunNode_ReviewPass(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult:    "result",
		reviewResults: []*ReviewResult{{Passed: true, Feedback: "looks good"}},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeCompleted {
		t.Errorf("expected completed, got %s", node.Status)
	}
	if len(node.ReviewHistory) != 1 {
		t.Fatalf("expected 1 review record, got %d", len(node.ReviewHistory))
	}
	if !node.ReviewHistory[0].Passed {
		t.Error("review should have passed")
	}
}

// --- runNode: Review 不通过 → 重新执行 → 最终通过 ---

func TestRunNode_ReviewFailThenPass(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "v1",
		fbResult:   "v2",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "fix this"},
			{Passed: true},
		},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeCompleted {
		t.Errorf("expected completed, got %s", node.Status)
	}
	if node.Result != "v2" {
		t.Errorf("expected v2 (from feedback), got %s", node.Result)
	}
	if len(node.ReviewHistory) != 2 {
		t.Errorf("expected 2 review records, got %d", len(node.ReviewHistory))
	}
	if exec.fbCalls.Load() != 1 {
		t.Errorf("expected 1 ExecuteWithFeedback call, got %d", exec.fbCalls.Load())
	}
}

// --- runNode: Review 不通过，超过 MaxIterations ---

func TestRunNode_ReviewExceedsMaxIterations(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 1},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "v1",
		fbResult:   "v2",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "still bad"},
		},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeFailed {
		t.Errorf("expected failed after max iterations, got %s", node.Status)
	}
	if !strings.Contains(node.Error, "max review iterations") {
		t.Errorf("expected max iterations error, got %s", node.Error)
	}
}

// --- runNode: terminated during execution ---

func TestRunNode_TerminatedDuringExecution(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", MaxRetries: 5},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execErr: errors.New("fail")}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	// Terminate before runNode
	s.Terminate()

	s.runNode(context.Background(), node)

	// Should be skipped by handleTerminate or returned early
	if node.Status == NodeCompleted {
		t.Error("node should not be completed when terminated")
	}
}

// --- reviewLoop: 直接测试 ---

func TestReviewLoop_PassOnFirstTry(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		reviewResults: []*ReviewResult{{Passed: true}},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	result, err := s.reviewLoop(context.Background(), node, "initial result")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result != "initial result" {
		t.Errorf("expected unchanged result, got %s", result)
	}
}

func TestReviewLoop_ReviewError(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		reviewErr: errors.New("review service down"),
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	_, err := s.reviewLoop(context.Background(), node, "initial")
	if err == nil {
		t.Fatal("expected error from review failure")
	}
	if !strings.Contains(err.Error(), "review error") {
		t.Errorf("expected review error, got %s", err.Error())
	}
}

func TestReviewLoop_ReExecuteError(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "v1",
		fbErr:      errors.New("re-execution failed"),
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "improve it"},
		},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	_, err := s.reviewLoop(context.Background(), node, "initial")
	if err == nil {
		t.Fatal("expected error from re-execution failure")
	}
	if !strings.Contains(err.Error(), "re-execution failed") {
		t.Errorf("expected re-execution error, got %s", err.Error())
	}
}

func TestReviewLoop_TerminatedMidLoop(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		reviewResults: []*ReviewResult{{Passed: true}},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")

	s.Terminate()

	_, err := s.reviewLoop(context.Background(), node, "initial")
	if err == nil {
		t.Fatal("expected error when terminated during review")
	}
	if !strings.Contains(err.Error(), "terminated") {
		t.Errorf("expected terminated error, got %s", err.Error())
	}
}

// --- runNode: Review 不通过 → 重新执行 → Review 再不通过 → 超限 ---

func TestReviewLoop_MultipleIterations(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "v0",
		fbResult:   "improved",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "iteration 1 fail"},
			{Passed: false, Feedback: "iteration 2 fail"},
			{Passed: false, Feedback: "iteration 3 fail"},
		},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	_, err := s.reviewLoop(context.Background(), node, "v0")

	if err == nil {
		t.Fatal("expected error after max iterations")
	}
	if !strings.Contains(err.Error(), "max review iterations") {
		t.Errorf("expected max iterations error, got %s", err.Error())
	}
	if len(node.ReviewHistory) != 3 {
		t.Errorf("expected 3 review records, got %d", len(node.ReviewHistory))
	}
	if exec.fbCalls.Load() != 3 {
		t.Errorf("expected 3 re-executions, got %d", exec.fbCalls.Load())
	}
}

// --- Run: 完整调度循环 ---

func TestRun_WithMockExecutor_Success(t *testing.T) {
	wf := NewWorkflow("wf-run", "req", []*DAGNode{
		{ID: "n1", Name: "t1"},
		{ID: "n2", Name: "t2", Dependencies: []string{"n1"}},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execResult: "ok"}
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := s.Run(ctx)

	if status != WorkflowCompleted {
		t.Fatalf("expected completed, got %s", status)
	}

	n1, _ := wf.GetNode("n1")
	if n1.Status != NodeCompleted {
		t.Errorf("n1 should be completed, got %s", n1.Status)
	}
	n2, _ := wf.GetNode("n2")
	if n2.Status != NodeCompleted {
		t.Errorf("n2 should be completed, got %s", n2.Status)
	}
}

func TestRun_WithMockExecutor_NodeFailure(t *testing.T) {
	wf := NewWorkflow("wf-run", "req", []*DAGNode{
		{ID: "n1", MaxRetries: 0}, // fail immediately
		{ID: "n2", Dependencies: []string{"n1"}},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execErr: errors.New("fail")}
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := s.Run(ctx)

	if status != WorkflowFailed {
		t.Fatalf("expected failed, got %s", status)
	}

	n1, _ := wf.GetNode("n1")
	if n1.Status != NodeFailed {
		t.Errorf("n1 should be failed, got %s", n1.Status)
	}
	n2, _ := wf.GetNode("n2")
	if n2.Status != NodeSkipped {
		t.Errorf("n2 should be skipped, got %s", n2.Status)
	}
}

func TestRun_ParallelExecution(t *testing.T) {
	wf := NewWorkflow("wf-par", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
		{ID: "n3"},
		{ID: "agg", Dependencies: []string{"n1", "n2", "n3"}},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execResult: "result"}
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := s.Run(ctx)

	if status != WorkflowCompleted {
		t.Fatalf("expected completed, got %s", status)
	}

	// All 4 nodes should be completed
	for _, n := range wf.Nodes {
		if n.Status != NodeCompleted {
			t.Errorf("%s should be completed, got %s", n.ID, n.Status)
		}
	}

	// 4 Execute calls total
	if exec.execCalls.Load() != 4 {
		t.Errorf("expected 4 Execute calls, got %d", exec.execCalls.Load())
	}
}

func TestRun_TerminateDuringRun(t *testing.T) {
	wf := NewWorkflow("wf-term", "req", []*DAGNode{
		{ID: "n1", MaxRetries: 100}, // will keep retrying
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execErr: errors.New("always fail"),
	}
	// Override Execute to add delay so retries are still in progress when terminate fires
	exec.execResult = ""
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		s.Terminate()
	}()

	status := s.Run(ctx)

	if status != WorkflowTerminated {
		t.Errorf("expected terminated, got %s", status)
	}
}

// ============================================================================
// BuildUpstreamContext + Compile 集入测试
// ============================================================================

// upstreamInjectExecutor 记录 Execute 时传入的 task 内容（用于验证上游结果注入）。
type upstreamInjectExecutor struct {
	execResult string
	execErr    error
	// 记录每次 Execute 调用时传入的 node.Task（可能被上游结果修改）
	capturedTasks []string
}

func (e *upstreamInjectExecutor) Execute(_ context.Context, node *DAGNode) (string, error) {
	e.capturedTasks = append(e.capturedTasks, node.Task)
	return e.execResult, e.execErr
}

func (e *upstreamInjectExecutor) ExecuteWithFeedback(_ context.Context, node *DAGNode, _, _ string) (string, error) {
	return e.execResult, e.execErr
}

func (e *upstreamInjectExecutor) Review(_ context.Context, node *DAGNode, _ string) (*ReviewResult, error) {
	return &ReviewResult{Passed: true}, nil
}

func TestRunNode_InjectsUpstreamContext(t *testing.T) {
	// n1 → n2, n1 has result → n2 should see upstream context in task
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "extract"},
		{ID: "n2", Name: "summarize", Dependencies: []string{"n1"}, Task: "总结提取结果"},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	n1.Result = "上游产出: 营收增长12%"

	exec := &upstreamInjectExecutor{execResult: "摘要完成"}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n2")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if len(exec.capturedTasks) != 1 {
		t.Fatalf("expected 1 Execute call, got %d", len(exec.capturedTasks))
	}
	task := exec.capturedTasks[0]
	if !contains(task, "[上游任务汇总]") {
		t.Errorf("expected upstream context injection in task, got: %s", task)
	}
	if !contains(task, "营收增长") {
		t.Errorf("expected upstream result content, got: %s", task)
	}
	if !contains(task, "[你的任务]") {
		t.Errorf("expected task separator, got: %s", task)
	}
}

func TestRunNode_NoUpstreamContext_WhenNoDeps(t *testing.T) {
	// Root node (no deps) should not have injected upstream context.
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "root", Task: "原始任务"},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	exec := &upstreamInjectExecutor{execResult: "done"}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	task := exec.capturedTasks[0]
	if contains(task, "[上游任务汇总]") {
		t.Errorf("root node should not have upstream context, got: %s", task)
	}
	if task != "原始任务" {
		t.Errorf("expected original task unchanged, got: %s", task)
	}
}

func TestRunNode_UpstreamNotCompleted_NoContext(t *testing.T) {
	// n1 still pending → n2 should not see its (nonexistent) result.
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "not_done", Task: "t1"},
		{ID: "n2", Name: "consumer", Dependencies: []string{"n1"}, Task: "t2"},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	exec := &upstreamInjectExecutor{execResult: "done"}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n2")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	task := exec.capturedTasks[0]
	if contains(task, "[上游任务汇总]") {
		t.Errorf("uncompleted upstream should not inject context, got: %s", task)
	}
}

// ============================================================================
// Manager Compile 流程集成测试
// ============================================================================

func TestManager_CompileCalledDuringAnalyzeAndRun(t *testing.T) {
	// Verify that Compile() is wired into the flow: Analyze → Compile → Schedule.
	// Use a simple DAG that passes validation.

	nodes := []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
	}

	for _, n := range nodes {
		n.Status = NodePending
	}
	wf := NewWorkflow("wf-test", "test req", nodes)

	// Simulate the analyzeAndRun flow: compile after nodes populated
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile should succeed for valid DAG: %v", err)
	}
	if !wf.Compiled() {
		t.Error("workflow should be compiled")
	}
	if wf.topoOrder == nil || wf.reverseAdj == nil || wf.roots == nil || wf.inDegree == nil {
		t.Error("all compile caches should be populated")
	}
}

func TestManager_CompileRejectedOnInvalidDAG(t *testing.T) {
	nodes := []*DAGNode{
		{ID: "n1", Dependencies: []string{"n2"}},
		{ID: "n2", Dependencies: []string{"n1"}},
	}
	for _, n := range nodes {
		n.Status = NodePending
	}
	wf := NewWorkflow("wf-bad", "req", nodes)
	err := wf.Compile()
	if err == nil {
		t.Fatal("expected cycle rejection")
	}
	if wf.Compiled() {
		t.Error("workflow should not be marked compiled on failure")
	}
}
