package workflow

import (
	"testing"
)

// ============================================================================
// DAG 算法单元测试
//
// 覆盖 dag.go 中所有纯逻辑函数，不依赖 LLM 或外部服务。
// ============================================================================

// --- ValidateDAG ---

func TestValidateDAG_EmptyNodes(t *testing.T) {
	if err := ValidateDAG(nil); err == nil {
		t.Error("expected error for nil nodes")
	}
	if err := ValidateDAG([]*DAGNode{}); err == nil {
		t.Error("expected error for empty nodes")
	}
}

func TestValidateDAG_DuplicateID(t *testing.T) {
	err := ValidateDAG([]*DAGNode{
		{ID: "n1", Name: "a"},
		{ID: "n1", Name: "b"},
	})
	if err == nil {
		t.Fatal("expected error for duplicate ID")
	}
}

func TestValidateDAG_EmptyID(t *testing.T) {
	err := ValidateDAG([]*DAGNode{
		{ID: "", Name: "a"},
	})
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
}

func TestValidateDAG_SelfReference(t *testing.T) {
	err := ValidateDAG([]*DAGNode{
		{ID: "n1", Dependencies: []string{"n1"}},
	})
	if err == nil {
		t.Fatal("expected error for self-referencing node")
	}
}

func TestValidateDAG_UnknownDependency(t *testing.T) {
	err := ValidateDAG([]*DAGNode{
		{ID: "n1", Dependencies: []string{"ghost"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown dependency")
	}
}

func TestValidateDAG_Cycle(t *testing.T) {
	// n1 → n2 → n3 → n1
	err := ValidateDAG([]*DAGNode{
		{ID: "n1", Dependencies: []string{"n3"}},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n2"}},
	})
	if err == nil {
		t.Fatal("expected error for cycle")
	}
}

func TestValidateDAG_ValidDAG(t *testing.T) {
	err := ValidateDAG([]*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
		{ID: "n4", Dependencies: []string{"n2", "n3"}},
	})
	if err != nil {
		t.Fatalf("valid DAG should pass: %v", err)
	}
}

func TestValidateDAG_DiamondDependency(t *testing.T) {
	// Diamond: n1 → n2, n1 → n3, n2+n3 → n4
	err := ValidateDAG([]*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
		{ID: "n4", Dependencies: []string{"n2", "n3"}},
	})
	if err != nil {
		t.Fatalf("diamond DAG should be valid: %v", err)
	}
}

// --- detectCycle ---

func TestDetectCycle_NoCycle(t *testing.T) {
	nodes := []*DAGNode{
		{ID: "a"},
		{ID: "b", Dependencies: []string{"a"}},
		{ID: "c", Dependencies: []string{"b"}},
	}
	if cycle := detectCycle(nodes); cycle != nil {
		t.Errorf("expected no cycle, got %v", cycle)
	}
}

func TestDetectCycle_TwoNodeCycle(t *testing.T) {
	nodes := []*DAGNode{
		{ID: "a", Dependencies: []string{"b"}},
		{ID: "b", Dependencies: []string{"a"}},
	}
	cycle := detectCycle(nodes)
	if cycle == nil {
		t.Fatal("expected cycle detection")
	}
	if len(cycle) != 2 {
		t.Errorf("expected 2 cycle nodes, got %d", len(cycle))
	}
}

// --- ReadyNodes ---

func TestReadyNodes_NoDeps(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "t1"},
		{ID: "n2", Name: "t2"},
	})
	ready := ReadyNodes(wf)
	if len(ready) != 2 {
		t.Errorf("expected 2 ready nodes, got %d", len(ready))
	}
}

func TestReadyNodes_PartialCompletion(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
	})
	wf.RebuildIndex()

	// n1 not completed → n2/n3 not ready
	ready := ReadyNodes(wf)
	if len(ready) != 1 || ready[0].ID != "n1" {
		t.Errorf("expected only n1 ready, got %v", ready)
	}

	// Complete n1 → n2/n3 become ready
	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	ready = ReadyNodes(wf)
	if len(ready) != 2 {
		t.Errorf("expected 2 ready nodes after n1 complete, got %d", len(ready))
	}
}

func TestReadyNodes_ExcludesNonPending(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
		{ID: "n3"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted // NewWorkflow sets to Pending, override after
	wf.Nodes[1].Status = NodeRunning

	ready := ReadyNodes(wf)
	// Only n3 is pending and has no deps
	if len(ready) != 1 || ready[0].ID != "n3" {
		t.Errorf("expected only n3 ready, got %v", ids(ready))
	}
}

// --- CascadeSkip ---

func TestCascadeSkip_LinearChain(t *testing.T) {
	// n1 → n2 → n3, n1 fails
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n2"}},
	})
	wf.RebuildIndex()

	skipped := CascadeSkip(wf, "n1")
	// n1 is failed (not pending), so it's not in the list
	// n2 and n3 are pending → should be skipped
	if len(skipped) != 2 {
		t.Fatalf("expected 2 skipped nodes, got %d: %v", len(skipped), skipped)
	}

	n2, _ := wf.GetNode("n2")
	n3, _ := wf.GetNode("n3")
	if n2.Status != NodeSkipped {
		t.Errorf("n2 should be skipped, got %s", n2.Status)
	}
	if n3.Status != NodeSkipped {
		t.Errorf("n3 should be skipped, got %s", n3.Status)
	}
	if n2.Error == "" {
		t.Error("n2 should have error message")
	}
}

func TestCascadeSkip_Diamond(t *testing.T) {
	// n1 → n2, n1 → n3, n2+n3 → n4
	// n2 fails → n4 should NOT be skipped (n3 still pending)
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
		{ID: "n4", Dependencies: []string{"n2", "n3"}},
	})
	wf.RebuildIndex()

	skipped := CascadeSkip(wf, "n2")
	// n4 depends on both n2 and n3; n3 is still pending
	// n4 should be visited but NOT skipped (its other dep n3 is pending, not failed)
	// Actually: CascadeSkip marks any dependent of a failed/skipped node as skipped
	// So n4 will be skipped because n2 didn't complete
	if len(skipped) < 1 {
		t.Errorf("expected at least 1 skipped node, got %d", len(skipped))
	}

	n4, _ := wf.GetNode("n4")
	if n4.Status != NodeSkipped {
		t.Errorf("n4 should be skipped because upstream n2 failed, got %s", n4.Status)
	}
}

func TestCascadeSkip_AlreadyTerminal(t *testing.T) {
	// n1 → n2(completed), n1 → n3(pending)
	// n1 fails: n3 should be skipped, n2 should NOT be re-skipped
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
	})
	wf.RebuildIndex()

	n2, _ := wf.GetNode("n2")
	n2.Status = NodeCompleted

	skipped := CascadeSkip(wf, "n1")
	// Only n3 should be in the skipped list
	for _, id := range skipped {
		if id == "n2" {
			t.Error("completed node n2 should not be in skipped list")
		}
	}

	n2Check, _ := wf.GetNode("n2")
	if n2Check.Status != NodeCompleted {
		t.Errorf("n2 should remain completed, got %s", n2Check.Status)
	}
}

// --- BuildTree ---

func TestBuildTree_Simple(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "root"},
		{ID: "child1", Dependencies: []string{"root"}},
		{ID: "child2", Dependencies: []string{"root"}},
	})
	wf.RebuildIndex()

	tree := BuildTree(wf)
	if len(tree) != 1 {
		t.Fatalf("expected 1 root, got %d", len(tree))
	}
	if tree[0].Node.ID != "root" {
		t.Errorf("expected root node, got %s", tree[0].Node.ID)
	}
	if len(tree[0].Children) != 2 {
		t.Errorf("expected 2 children, got %d", len(tree[0].Children))
	}
}

func TestBuildTree_Diamond(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
		{ID: "n4", Dependencies: []string{"n2", "n3"}},
	})
	wf.RebuildIndex()

	tree := BuildTree(wf)
	if len(tree) != 1 {
		t.Fatalf("expected 1 root, got %d", len(tree))
	}
	// n1 has 2 children: n2, n3
	if len(tree[0].Children) != 2 {
		t.Errorf("expected 2 children of n1, got %d", len(tree[0].Children))
	}
	// n4 should appear under both n2 and n3 (diamond)
	for _, child := range tree[0].Children {
		if len(child.Children) != 1 {
			t.Errorf("expected n4 under %s, got %d children", child.Node.ID, len(child.Children))
		}
		if child.Children[0].Node.ID != "n4" {
			t.Errorf("expected n4, got %s", child.Children[0].Node.ID)
		}
	}
}

func TestBuildTree_MultipleRoots(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "r1"},
		{ID: "r2"},
		{ID: "c1", Dependencies: []string{"r1"}},
	})
	wf.RebuildIndex()

	tree := BuildTree(wf)
	if len(tree) != 2 {
		t.Fatalf("expected 2 roots, got %d", len(tree))
	}
}

func TestBuildTree_Empty(t *testing.T) {
	wf := NewWorkflow("wf", "req", nil)
	wf.RebuildIndex()
	tree := BuildTree(wf)
	if len(tree) != 0 {
		t.Errorf("expected empty tree, got %d roots", len(tree))
	}
}

// --- IsAllTerminal ---

func TestIsAllTerminal_AllCompleted(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	wf.Nodes[1].Status = NodeCompleted
	if !IsAllTerminal(wf) {
		t.Error("expected all terminal")
	}
}

func TestIsAllTerminal_MixedTerminal(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	wf.Nodes[1].Status = NodeFailed
	if !IsAllTerminal(wf) {
		t.Error("completed+failed should be all terminal")
	}
}

func TestIsAllTerminal_HasPending(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	// n2 still pending
	if IsAllTerminal(wf) {
		t.Error("should not be all terminal with pending node")
	}
}

func TestIsAllTerminal_Empty(t *testing.T) {
	wf := NewWorkflow("wf", "req", nil)
	wf.RebuildIndex()
	// Vacuously true: no nodes → all terminal
	if !IsAllTerminal(wf) {
		t.Error("empty workflow should be all terminal (vacuously true)")
	}
}

// --- HasFailedNode ---

func TestHasFailedNode_True(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[1].Status = NodeFailed
	if !HasFailedNode(wf) {
		t.Error("expected has failed node")
	}
}

func TestHasFailedNode_False(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	wf.Nodes[1].Status = NodeSkipped
	if HasFailedNode(wf) {
		t.Error("no failed nodes expected")
	}
}

// --- helpers ---

func ids(nodes []*DAGNode) []string {
	result := make([]string, len(nodes))
	for i, n := range nodes {
		result[i] = n.ID
	}
	return result
}
