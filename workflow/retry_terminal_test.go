package workflow

import (
	"testing"
)

// failedWorkflow 构造一个「n1 失败、下游被级联 skip」的终态工作流，
// 即用户点「重试」时的真实现场。
//
// 注意：NewWorkflow 会把所有节点强制置为 pending，所以状态必须在构造之后再设。
func failedWorkflow() *Workflow {
	wf := NewWorkflow("wf-retry", "req", []*DAGNode{
		{ID: "n1", Name: "失败节点"},
		{ID: "n2", Name: "下游1", Dependencies: []string{"n1"}},
		{ID: "n3", Name: "下游2", Dependencies: []string{"n2"}},
	})
	n1, _ := wf.GetNode("n1")
	n1.Status = NodeFailed
	n1.Error = "boom"
	n1.RetryCount = 2

	n2, _ := wf.GetNode("n2")
	n2.Status = NodeSkipped
	n2.Error = `upstream node "n1" did not complete`

	n3, _ := wf.GetNode("n3")
	n3.Status = NodeSkipped
	n3.Error = `upstream node "n2" did not complete`

	wf.Status = WorkflowFailed
	wf.Error = "node n1 failed"
	return wf
}

// TestResetForRetry_RevivesSkippedDownstream 验证重试会一并复活被级联跳过的下游。
//
// 若只把目标节点标回 pending，它跑完后下游仍是 skipped 终态，调度器会立刻收尾，
// 整个重试等于白做。
func TestResetForRetry_RevivesSkippedDownstream(t *testing.T) {
	wf := failedWorkflow()

	revived := resetForRetry(wf, "n1")
	if revived != 2 {
		t.Errorf("revived = %d, want 2 (both cascade-skipped nodes)", revived)
	}

	n1, _ := wf.GetNode("n1")
	if n1.Status != NodePending {
		t.Errorf("n1 status = %v, want pending", n1.Status)
	}
	if n1.Error != "" || n1.RetryCount != 0 {
		t.Errorf("n1 should have cleared error/retryCount, got error=%q retry=%d", n1.Error, n1.RetryCount)
	}

	for _, id := range []string{"n2", "n3"} {
		n, _ := wf.GetNode(id)
		if n.Status != NodePending {
			t.Errorf("%s status = %v, want pending (cascade-skipped nodes must be revived)", id, n.Status)
		}
		if n.Error != "" {
			t.Errorf("%s should have cleared its upstream error, got %q", id, n.Error)
		}
	}
}

// TestResetForRetry_KeepsCompletedWork 验证已完成的节点不被重置。
//
// 重试一个失败节点不该让前面成功的工作重跑，那会白烧大量模型调用。
func TestResetForRetry_KeepsCompletedWork(t *testing.T) {
	wf := NewWorkflow("wf-keep", "req", []*DAGNode{
		{ID: "n1", Name: "已完成"},
		{ID: "n2", Name: "失败", Dependencies: []string{"n1"}},
	})
	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	n1.Result = "done"
	n2, _ := wf.GetNode("n2")
	n2.Status = NodeFailed
	n2.Error = "boom"

	if revived := resetForRetry(wf, "n2"); revived != 0 {
		t.Errorf("revived = %d, want 0 (nothing was skipped here)", revived)
	}

	if n1.Status != NodeCompleted {
		t.Errorf("n1 status = %v, want completed (finished work must not be redone)", n1.Status)
	}
	if n1.Result != "done" {
		t.Errorf("n1 result was lost: %q", n1.Result)
	}
	if n2.Status != NodePending || n2.Error != "" {
		t.Errorf("n2 should be reset, got status=%v error=%q", n2.Status, n2.Error)
	}
}

// TestRestartFromNode_RejectsDoubleStart 验证已在运行时不会重复拉起调度。
//
// 用户连点两次「重试」不该产生两个调度器同时操作同一个工作流。
// 本用例刻意在预置running 实例的前提下调用，因此不会真正启动后台 goroutine。
func TestRestartFromNode_RejectsDoubleStart(t *testing.T) {
	wf := failedWorkflow()
	repo := NewRepository(nil, nil)
	if err := repo.Save(wf); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := NewManager(repo, nil, nil, nil, EngineConfig{MaxParallel: 1}, nil, nil)

	m.mu.Lock()
	m.running[wf.ID] = &runningInstance{wf: wf}
	m.mu.Unlock()

	if _, err := m.restartFromNode(wf, "n1"); err == nil {
		t.Fatal("expected an error when a running instance already exists")
	}
}

// TestRestartFromNode_UnknownNode 验证未知节点被拒绝（不会启动调度）。
func TestRestartFromNode_UnknownNode(t *testing.T) {
	wf := failedWorkflow()
	repo := NewRepository(nil, nil)
	if err := repo.Save(wf); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := NewManager(repo, nil, nil, nil, EngineConfig{MaxParallel: 1}, nil, nil)

	if _, err := m.restartFromNode(wf, "nope"); err == nil {
		t.Fatal("expected an error for an unknown node id")
	}
}
