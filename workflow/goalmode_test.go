package workflow

import (
	"context"
	"testing"
	"time"
)

// ============================================================================
// 目标模式（Goal Mode）闭环测试
//
// 验证 review 节点在节点级迭代仍不通过时，回退到 Feedback 目标节点重跑，
// 形成「工作→审查→修复→审查」的全局闭环，直到通过或达到最大轮数。
// ============================================================================

// --- 单节点自回退：闭环直到通过 ---

func TestGoalMode_LoopUntilPass(t *testing.T) {
	wf := NewWorkflow("wf-goal", "req", []*DAGNode{
		{ID: "n1", Name: "work", Task: "do it", Review: true, MaxIterations: 1, Feedback: []string{"n1"}},
	})
	wf.GoalMode = true
	wf.GoalMaxIterations = 2
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "v1",
		fbResult:   "v2",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "fix round 1"},
			{Passed: false, Feedback: "fix round 2"},
			{Passed: true},
		},
	}
	s := newMockScheduler(wf, exec)
	s.ec.GoalMaxIterations = 0 // 不使用引擎默认，走 wf.GoalMaxIterations

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
	if wf.GoalIteration != 2 {
		t.Errorf("expected 2 goal iterations, got %d", wf.GoalIteration)
	}
	// 至少 1 次普通执行 + N 次带反馈重跑（节点级 in-place 修复与闭环重跑都会用到
	// ExecuteWithFeedback，故只断言下界；revCalls 精确反映 review 轮数）。
	if exec.execCalls.Load() < 1 {
		t.Errorf("expected at least 1 plain Execute, got %d", exec.execCalls.Load())
	}
	if exec.fbCalls.Load() < 2 {
		t.Errorf("expected at least 2 ExecuteWithFeedback (goal loops), got %d", exec.fbCalls.Load())
	}
	if exec.revCalls.Load() != 3 {
		t.Errorf("expected 3 reviews (fail + fail + pass), got %d", exec.revCalls.Load())
	}
	if n1.LoopFeedback != "" {
		t.Errorf("LoopFeedback should be cleared after run, got %q", n1.LoopFeedback)
	}
}

// --- 单节点自回退：额度耗尽后失败 ---

func TestGoalMode_Exhausted(t *testing.T) {
	wf := NewWorkflow("wf-goal-fail", "req", []*DAGNode{
		{ID: "n1", Name: "work", Task: "do it", Review: true, MaxIterations: 1, Feedback: []string{"n1"}},
	})
	wf.GoalMode = true
	wf.GoalMaxIterations = 1 // 仅允许 1 轮闭环
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "v1",
		fbResult:   "v2",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "still bad"},
			{Passed: false, Feedback: "still bad 2"},
		},
	}
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := s.Run(ctx)
	if status != WorkflowFailed {
		t.Fatalf("expected failed after goal iterations exhausted, got %s", status)
	}

	n1, _ := wf.GetNode("n1")
	if n1.Status != NodeFailed {
		t.Errorf("n1 should be failed, got %s", n1.Status)
	}
	if wf.GoalIteration != 1 {
		t.Errorf("expected exactly 1 goal iteration (then exhausted), got %d", wf.GoalIteration)
	}
}

// --- 多节点：回退到独立工作节点重跑 ---

func TestGoalMode_FeedbackToWorkNode(t *testing.T) {
	wf := NewWorkflow("wf-goal-2", "req", []*DAGNode{
		{ID: "work", Name: "implement", Task: "write code"},
		{ID: "review", Name: "check", Task: "review code", Dependencies: []string{"work"}, Review: true, MaxIterations: 1, Feedback: []string{"work"}},
	})
	wf.GoalMode = true
	wf.GoalMaxIterations = 1
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "out",
		fbResult:   "out-fixed",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "bugs found"},
			{Passed: true},
		},
	}
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := s.Run(ctx)
	if status != WorkflowCompleted {
		t.Fatalf("expected completed, got %s", status)
	}

	work, _ := wf.GetNode("work")
	review, _ := wf.GetNode("review")
	if work.Status != NodeCompleted || review.Status != NodeCompleted {
		t.Fatalf("both nodes should be completed, got work=%s review=%s", work.Status, review.Status)
	}
	if wf.GoalIteration != 1 {
		t.Errorf("expected 1 goal iteration, got %d", wf.GoalIteration)
	}
	// work 应被闭环重跑（至少 1 次带反馈执行）；revCalls=2 反映 review 轮数（fail→pass）。
	if exec.fbCalls.Load() < 1 {
		t.Errorf("expected at least 1 ExecuteWithFeedback (goal loop back to work), got %d", exec.fbCalls.Load())
	}
	if exec.revCalls.Load() != 2 {
		t.Errorf("expected 2 reviews (fail + pass), got %d", exec.revCalls.Load())
	}
	if work.LoopFeedback != "" {
		t.Errorf("work.LoopFeedback should be cleared, got %q", work.LoopFeedback)
	}
}

// --- 目标模式关闭时：review 不通过直接失败（不影响既有行为） ---

func TestGoalMode_DisabledFailsImmediately(t *testing.T) {
	wf := NewWorkflow("wf-no-goal", "req", []*DAGNode{
		{ID: "n1", Review: true, MaxIterations: 1, Feedback: []string{"n1"}},
	})
	// GoalMode 默认 false
	wf.RebuildIndex()

	exec := &mockExecutor{
		execResult: "v1",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "bad"},
		},
	}
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := s.Run(ctx)
	if status != WorkflowFailed {
		t.Fatalf("expected failed when goal mode disabled, got %s", status)
	}
	if wf.GoalIteration != 0 {
		t.Errorf("GoalIteration should stay 0 when goal mode disabled, got %d", wf.GoalIteration)
	}
}

// --- wireGoalMode：终点节点 review + 回退上游；非终点节点强制 review 且自环 ---

func TestWireGoalMode_AutoWiresSink(t *testing.T) {
	nodes := []*DAGNode{
		{ID: "a", Dependencies: []string{}},
		{ID: "b", Dependencies: []string{"a"}},
		{ID: "c", Dependencies: []string{"b"}}, // 终点（无下游）
	}
	wireGoalMode(nodes)

	c, _ := findByID(nodes, "c")
	if !c.Review {
		t.Error("sink node c should have review=true auto-enabled")
	}
	if len(c.Feedback) != 1 || c.Feedback[0] != "b" {
		t.Errorf("sink node c feedback should point to its upstream 'b', got %v", c.Feedback)
	}

	// 目标模式下非终点节点也强制 review，且自环（支撑逐模块门禁）。
	a, _ := findByID(nodes, "a")
	if !a.Review {
		t.Error("non-sink node a should be auto-reviewed in goal mode")
	}
	if len(a.Feedback) != 1 || a.Feedback[0] != "a" {
		t.Errorf("non-sink node a should self-loop in goal mode, got %v", a.Feedback)
	}

	b, _ := findByID(nodes, "b")
	if !b.Review {
		t.Error("non-sink node b should be auto-reviewed in goal mode")
	}
	if len(b.Feedback) != 1 || b.Feedback[0] != "b" {
		t.Errorf("non-sink node b should self-loop in goal mode, got %v", b.Feedback)
	}
}

// --- wireGoalMode：多模块逐模块门禁（m1 -> m2 -> m3 -> 整体）---

func TestWireGoalMode_PerModuleConvergence(t *testing.T) {
	nodes := []*DAGNode{
		{ID: "m1", Dependencies: []string{}},
		{ID: "m2", Dependencies: []string{"m1"}},
		{ID: "m3", Dependencies: []string{"m2"}},
		{ID: "overall", Dependencies: []string{"m3"}},
	}
	wireGoalMode(nodes)

	for _, id := range []string{"m1", "m2", "m3", "overall"} {
		n, _ := findByID(nodes, id)
		if !n.Review {
			t.Errorf("node %s should be auto-reviewed in goal mode", id)
		}
	}
	// 中间模块自环：闭环时只重跑自身，不波及下游。
	for _, id := range []string{"m1", "m2", "m3"} {
		n, _ := findByID(nodes, id)
		if len(n.Feedback) != 1 || n.Feedback[0] != id {
			t.Errorf("module %s should self-loop, got %v", id, n.Feedback)
		}
	}
	// 终点回退到直接上游。
	ov, _ := findByID(nodes, "overall")
	if len(ov.Feedback) != 1 || ov.Feedback[0] != "m3" {
		t.Errorf("overall should feedback to its upstream 'm3', got %v", ov.Feedback)
	}
}

// --- wireGoalMode：单节点工作流回退到自身 ---

func TestWireGoalMode_SingleNodeSelfLoop(t *testing.T) {
	nodes := []*DAGNode{
		{ID: "only"},
	}
	wireGoalMode(nodes)

	if !nodes[0].Review {
		t.Error("single node should have review auto-enabled")
	}
	if len(nodes[0].Feedback) != 1 || nodes[0].Feedback[0] != "only" {
		t.Errorf("single node feedback should point to itself, got %v", nodes[0].Feedback)
	}
}

// --- 目标模式：多模块逐模块门禁集成测试 ---
//
// 结构 m1 -> m2 -> m3 -> overall，每个节点第一轮 review 不通过、第二轮通过；
// 终点 overall 在节点级迭代失败后还会触发一次终点闭环（重置 overall 与 m3）。
// 验证：依赖链保证逐模块顺序门禁、每个模块通过自环收敛、整体审查后完成。

func TestGoalMode_PerModuleSequentialGating(t *testing.T) {
	wf := NewWorkflow("wf-permodule", "review program", []*DAGNode{
		{ID: "m1", Name: "review mod1", Task: "review module 1", Review: true, MaxIterations: 1, Feedback: []string{"m1"}},
		{ID: "m2", Name: "review mod2", Task: "review module 2", Dependencies: []string{"m1"}, Review: true, MaxIterations: 1, Feedback: []string{"m2"}},
		{ID: "m3", Name: "review mod3", Task: "review module 3", Dependencies: []string{"m2"}, Review: true, MaxIterations: 1, Feedback: []string{"m3"}},
		{ID: "overall", Name: "overall review", Task: "review whole program", Dependencies: []string{"m3"}, Review: true, MaxIterations: 1, Feedback: []string{"m3"}},
	})
	wf.GoalMode = true
	wf.GoalMaxIterations = 5
	wf.RebuildIndex()

	// 4 个节点各需 2 次 review（fail→pass），共 8 次显式结果；之后默认通过。
	exec := &mockExecutor{
		execResult: "v",
		fbResult:   "v-fixed",
		reviewResults: []*ReviewResult{
			{Passed: false, Feedback: "m1 issues"},
			{Passed: true},
			{Passed: false, Feedback: "m2 issues"},
			{Passed: true},
			{Passed: false, Feedback: "m3 issues"},
			{Passed: true},
			{Passed: false, Feedback: "overall issues"},
			{Passed: true},
		},
	}
	s := newMockScheduler(wf, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := s.Run(ctx)
	if status != WorkflowCompleted {
		t.Fatalf("expected completed, got %s", status)
	}

	for _, id := range []string{"m1", "m2", "m3", "overall"} {
		n, _ := wf.GetNode(id)
		if n.Status != NodeCompleted {
			t.Errorf("node %s should be completed, got %s", id, n.Status)
		}
		if n.LoopFeedback != "" {
			t.Errorf("node %s LoopFeedback should be cleared after run, got %q", id, n.LoopFeedback)
		}
	}
	// 顺序门禁由 Dependencies 结构性保证：m1/m2/m3 各自自环 1 次 + 终点闭环 1 次 = 4。
	if wf.GoalIteration != 4 {
		t.Errorf("expected 4 goal iterations (3 per-module self-loops + 1 sink loop), got %d", wf.GoalIteration)
	}
	// 每个节点至少经过 2 次 review（fail→pass），证明逐模块收敛确实发生。
	if exec.revCalls.Load() < 8 {
		t.Errorf("expected at least 8 reviews (2 per node), got %d", exec.revCalls.Load())
	}
}

func findByID(nodes []*DAGNode, id string) (*DAGNode, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return nil, false
}
