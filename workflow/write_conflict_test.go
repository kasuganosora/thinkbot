package workflow

import (
	"sync"
	"testing"
)

// ============================================================================
// 并发写冲突检测
//
// 默认并行 3 个节点共享同一 bot 工作区、无任何文件锁。
// 两个节点覆盖同一文件时不报错、不留痕，这是最难排查的一类故障。
// 本文件锁死「冲突可被发现」。
// ============================================================================

func TestWriteRecorder_Basic(t *testing.T) {
	rec := newWriteRecorder()
	rec.RecordWrite("a.go", "write")
	rec.RecordWrite("b.go", "replace")
	rec.RecordWrite("a.go", "delete") // 同一路径第二次操作

	ops := rec.ops()
	if len(ops) != 2 {
		t.Fatalf("expected 2 distinct paths, got %d (%v)", len(ops), ops)
	}
	if got := ops["a.go"]; len(got) != 2 || got[0] != "write" || got[1] != "delete" {
		t.Errorf("a.go ops: got %v, want [write delete]", got)
	}
	if got := ops["b.go"]; len(got) != 1 || got[0] != "replace" {
		t.Errorf("b.go ops: got %v, want [replace]", got)
	}
}

// TestWriteRecorder_EmptyPathSkipped 空路径不该记录（防御脏数据）。
func TestWriteRecorder_EmptyPathSkipped(t *testing.T) {
	rec := newWriteRecorder()
	rec.RecordWrite("", "write")
	if got := rec.ops(); len(got) != 0 {
		t.Errorf("empty path should be skipped, got %v", got)
	}
}

// TestWriteRecorder_Concurrent 并行调用必须安全（节点内多步编排会并发调工具）。
func TestWriteRecorder_Concurrent(t *testing.T) {
	rec := newWriteRecorder()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rec.RecordWrite("shared.go", "write")
		}(i)
	}
	wg.Wait()

	ops := rec.ops()
	if got := ops["shared.go"]; len(got) != 50 {
		t.Errorf("expected 50 recorded writes, got %d", len(got))
	}
}

// newWorkflowForConflictTest 构造用于冲突检测的工作流。
//
// 刻意**不用** NewWorkflow：它会把所有节点状态强制重置为 NodePending
// （types.go:232），而冲突检测只看已执行过的节点——用它会让所有用例都测不到东西。
func newWorkflowForConflictTest(nodes []*DAGNode) *Workflow {
	wf := &Workflow{
		ID:        "wf",
		Status:    WorkflowRunning,
		Nodes:     nodes,
		nodeIndex: make(map[string]*DAGNode, len(nodes)),
	}
	for _, n := range nodes {
		wf.nodeIndex[n.ID] = n
	}
	return wf
}

func TestDetectWriteConflicts(t *testing.T) {
	tests := []struct {
		name      string
		nodes     []*DAGNode
		wantPaths []string
		destruct  bool
	}{
		{
			name: "两节点写同一路径→冲突",
			nodes: []*DAGNode{
				{ID: "n1", Status: NodeCompleted, WrittenOps: map[string][]string{"a.go": {"write"}}},
				{ID: "n2", Status: NodeCompleted, WrittenOps: map[string][]string{"a.go": {"write"}}},
			},
			wantPaths: []string{"a.go"},
		},
		{
			name: "不同路径不算冲突",
			nodes: []*DAGNode{
				{ID: "n1", Status: NodeCompleted, WrittenOps: map[string][]string{"a.go": {"write"}}},
				{ID: "n2", Status: NodeCompleted, WrittenOps: map[string][]string{"b.go": {"write"}}},
			},
			wantPaths: nil,
		},
		{
			name: "同一节点重复写自己不算冲突",
			nodes: []*DAGNode{
				{ID: "n1", Status: NodeCompleted, WrittenOps: map[string][]string{"a.go": {"write", "write", "replace"}}},
			},
			wantPaths: nil,
		},
		{
			name: "pending节点的路径不算（避免虚假冲突）",
			nodes: []*DAGNode{
				{ID: "n1", Status: NodeCompleted, WrittenOps: map[string][]string{"a.go": {"write"}}},
				{ID: "n2", Status: NodePending, WrittenOps: map[string][]string{"a.go": {"write"}}},
			},
			wantPaths: nil,
		},
		{
			name: "删除操作标记为破坏性",
			nodes: []*DAGNode{
				{ID: "n1", Status: NodeCompleted, WrittenOps: map[string][]string{"a.go": {"write"}}},
				{ID: "n2", Status: NodeCompleted, WrittenOps: map[string][]string{"a.go": {"delete"}}},
			},
			wantPaths: []string{"a.go"},
			destruct:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := newWorkflowForConflictTest(tt.nodes)
			got := wf.detectWriteConflicts()

			if len(got) != len(tt.wantPaths) {
				t.Fatalf("conflict count: got %d, want %d (%+v)", len(got), len(tt.wantPaths), got)
			}
			for i, c := range got {
				if c.Path != tt.wantPaths[i] {
					t.Errorf("conflict[%d].Path: got %q, want %q", i, c.Path, tt.wantPaths[i])
				}
				if len(c.NodeIDs) < 2 {
					t.Errorf("conflict should involve >= 2 nodes, got %v", c.NodeIDs)
				}
				if tt.destruct && !c.Destructive {
					t.Errorf("conflict on %q should be marked destructive, ops=%v", c.Path, c.Ops)
				}
				if !tt.destruct && c.Destructive {
					t.Errorf("conflict on %q should NOT be destructive, ops=%v", c.Path, c.Ops)
				}
			}
		})
	}
}

// TestDetectWriteConflicts_DeterministicOrder 输出顺序必须稳定。
// 否则同一条工作流的详情接口每次返回的顺序都不同，前端展示会跳动。
func TestDetectWriteConflicts_DeterministicOrder(t *testing.T) {
	nodes := []*DAGNode{
		{ID: "n1", Status: NodeCompleted, WrittenOps: map[string][]string{
			"z.go": {"write"}, "a.go": {"write"}, "m.go": {"write"},
		}},
		{ID: "n2", Status: NodeCompleted, WrittenOps: map[string][]string{
			"z.go": {"write"}, "a.go": {"write"}, "m.go": {"write"},
		}},
	}
	wf := newWorkflowForConflictTest(nodes)

	for i := 0; i < 5; i++ {
		got := wf.detectWriteConflicts()
		if len(got) != 3 {
			t.Fatalf("run %d: expected 3 conflicts, got %d", i, len(got))
		}
		want := []string{"a.go", "m.go", "z.go"}
		for j, c := range got {
			if c.Path != want[j] {
				t.Fatalf("run %d: conflict[%d] = %q, want %q (order not stable)", i, j, c.Path, want[j])
			}
		}
	}
}

// TestRecordWrittenPaths_MergesAcrossRounds 多轮执行合并，不覆盖前几轮。
func TestRecordWrittenPaths_MergesAcrossRounds(t *testing.T) {
	e := &Executor{}
	node := &DAGNode{ID: "n1"}

	r1 := newWriteRecorder()
	r1.RecordWrite("a.go", "write")
	e.recordWrittenPaths(node, r1)

	r2 := newWriteRecorder()
	r2.RecordWrite("b.go", "replace")
	e.recordWrittenPaths(node, r2)

	if got := node.WrittenOps["a.go"]; len(got) != 1 || got[0] != "write" {
		t.Errorf("a.go should survive from round 1, got %v", got)
	}
	if got := node.WrittenOps["b.go"]; len(got) != 1 || got[0] != "replace" {
		t.Errorf("b.go from round 2: got %v", got)
	}
}

// TestRecordWrittenPaths_NoopOnEmpty 没有写操作时不产生空 map。
func TestRecordWrittenPaths_NoopOnEmpty(t *testing.T) {
	e := &Executor{}
	node := &DAGNode{ID: "n1"}
	e.recordWrittenPaths(node, newWriteRecorder())

	if node.WrittenOps != nil {
		t.Errorf("WrittenOps should stay nil when nothing was written, got %v", node.WrittenOps)
	}
}
