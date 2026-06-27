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

// ============================================================================
// Compile 编译测试
// ============================================================================

func TestCompile_ValidDAG(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
		{ID: "n4", Dependencies: []string{"n2", "n3"}},
	})
	err := wf.Compile()
	if err != nil {
		t.Fatalf("valid DAG should compile: %v", err)
	}
	if !wf.Compiled() {
		t.Error("workflow should be compiled")
	}
	if len(wf.topoOrder) != 4 {
		t.Errorf("expected 4 nodes in topo order, got %d", len(wf.topoOrder))
	}
	if len(wf.reverseAdj) == 0 {
		t.Error("expected non-empty reverse adjacency")
	}
	if len(wf.roots) != 1 || wf.roots[0] != "n1" {
		t.Errorf("expected roots=[n1], got %v", wf.roots)
	}
	// n1 has 0 dependencies, n4 has 2
	if wf.inDegree["n1"] != 0 {
		t.Errorf("n1 inDegree should be 0, got %d", wf.inDegree["n1"])
	}
	if wf.inDegree["n4"] != 2 {
		t.Errorf("n4 inDegree should be 2, got %d", wf.inDegree["n4"])
	}
}

func TestCompile_EmptyNodes(t *testing.T) {
	wf := NewWorkflow("wf", "req", nil)
	err := wf.Compile()
	if err == nil {
		t.Fatal("expected error for empty nodes")
	}
	if wf.Compiled() {
		t.Error("should not be compiled after error")
	}
}

func TestCompile_CycleRejected(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Dependencies: []string{"n2"}},
		{ID: "n2", Dependencies: []string{"n1"}},
	})
	err := wf.Compile()
	if err == nil {
		t.Fatal("expected error for cyclic DAG")
	}
	if wf.Compiled() {
		t.Error("should not be compiled after cycle detection")
	}
}

func TestCompile_ReverseAdjacency(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	// n1 → [n2, n3] (downstream)
	deps := wf.reverseAdj["n1"]
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependents of n1, got %d", len(deps))
	}
	for _, dep := range deps {
		if dep != "n2" && dep != "n3" {
			t.Errorf("unexpected dependent: %s", dep)
		}
	}
}

func TestCompile_TopoOrder(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "c", Dependencies: []string{"b"}},
		{ID: "a"},
		{ID: "b", Dependencies: []string{"a"}},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	order := wf.topoOrder
	if len(order) != 3 {
		t.Fatalf("expected 3 nodes in order, got %d", len(order))
	}
	if order[0] != "a" {
		t.Errorf("expected a first, got %s", order[0])
	}
	if order[1] != "b" {
		t.Errorf("expected b second, got %s", order[1])
	}
	if order[2] != "c" {
		t.Errorf("expected c third, got %s", order[2])
	}
}

func TestCompile_MultipleRoots(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "r1"},
		{ID: "r2"},
		{ID: "c1", Dependencies: []string{"r1"}},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	if len(wf.roots) != 2 {
		t.Errorf("expected 2 roots, got %d", len(wf.roots))
	}
}

func TestCompile_Diamond(t *testing.T) {
	// Diamond: n1 → n2, n1 → n3, n2+n3 → n4
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
		{ID: "n4", Dependencies: []string{"n2", "n3"}},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("diamond should compile: %v", err)
	}

	// Verify reverse adjacency for diamond
	if len(wf.reverseAdj["n1"]) != 2 {
		t.Errorf("n1 should have 2 dependents, got %d", len(wf.reverseAdj["n1"]))
	}
	// n4 has both n2 and n3 as direct dependents
	if len(wf.reverseAdj["n2"]) != 1 {
		t.Errorf("n2 should have 1 dependent (n4)")
	}
	if len(wf.reverseAdj["n3"]) != 1 {
		t.Errorf("n3 should have 1 dependent (n4)")
	}
}

// ============================================================================
// ReadyNodes 编译后快速路径测试
// ============================================================================

func TestReadyNodes_CompiledFastPath(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n1"}},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	// Initially only n1 ready
	ready := ReadyNodes(wf)
	if len(ready) != 1 || ready[0].ID != "n1" {
		t.Errorf("expected only n1 ready, got %v", ids(ready))
	}

	// Complete n1 → n2 and n3 become ready
	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	ready = ReadyNodes(wf)
	if len(ready) != 2 {
		t.Errorf("expected n2,n3 ready after n1 complete, got %v", ids(ready))
	}
}

func TestReadyNodes_UncompiledFallback(t *testing.T) {
	// Without Compile(), should still work via slow path
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
	})
	// Not calling Compile() - should use slow path
	ready := ReadyNodes(wf)
	if len(ready) != 1 || ready[0].ID != "n1" {
		t.Errorf("expected n1 ready (slow path), got %v", ids(ready))
	}
}

// ============================================================================
// CascadeSkip 编译后快速路径测试
// ============================================================================

func TestCascadeSkip_CompiledFastPath(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
		{ID: "n3", Dependencies: []string{"n2"}},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	skipped := CascadeSkip(wf, "n1")
	if len(skipped) != 2 {
		t.Fatalf("expected 2 skipped nodes, got %d: %v", len(skipped), skipped)
	}

	n2, _ := wf.GetNode("n2")
	n3, _ := wf.GetNode("n3")
	if n2.Status != NodeSkipped || n3.Status != NodeSkipped {
		t.Errorf("expected n2,n3 skipped, got %s,%s", n2.Status, n3.Status)
	}
}

// ============================================================================
// BuildTree 编译后快速路径测试
// ============================================================================

func TestBuildTree_CompiledFastPath(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "root"},
		{ID: "child1", Dependencies: []string{"root"}},
		{ID: "child2", Dependencies: []string{"root"}},
	})
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	tree := BuildTree(wf)
	if len(tree) != 1 {
		t.Fatalf("expected 1 root, got %d", len(tree))
	}
	if len(tree[0].Children) != 2 {
		t.Errorf("expected 2 children (compiled path), got %d", len(tree[0].Children))
	}
}

// ============================================================================
// BuildUpstreamContext 上游结果注入测试
// ============================================================================

func TestBuildUpstreamContext_NoDeps(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
	})
	wf.RebuildIndex()

	node, _ := wf.GetNode("n1")
	ctx := BuildUpstreamContext(wf, node)
	if ctx != "" {
		t.Errorf("expected empty context for node with no deps, got: %s", ctx)
	}
}

func TestBuildUpstreamContext_UpstreamNotCompleted(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}},
	})
	wf.RebuildIndex()

	// n1 not completed → no context for n2
	node, _ := wf.GetNode("n2")
	ctx := BuildUpstreamContext(wf, node)
	if ctx != "" {
		t.Errorf("expected empty context when upstream not completed, got: %s", ctx)
	}
}

func TestBuildUpstreamContext_SingleUpstream(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "extract"},
		{ID: "n2", Dependencies: []string{"n1"}, Name: "summary"},
	})
	wf.RebuildIndex()

	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	n1.Result = "营收增长 12%，毛利率下降 3%"

	node, _ := wf.GetNode("n2")
	ctx := BuildUpstreamContext(wf, node)

	if ctx == "" {
		t.Fatal("expected non-empty upstream context")
	}
	if !contains(ctx, "[上游任务汇总]") {
		t.Error("expected upstream header")
	}
	if !contains(ctx, "extract") {
		t.Error("expected upstream node name")
	}
	if !contains(ctx, "营收增长") {
		t.Error("expected upstream result content")
	}
	if !contains(ctx, "[你的任务]") {
		t.Error("expected task separator")
	}
}

func TestBuildUpstreamContext_MultipleUpstreams(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "finance"},
		{ID: "n2", Name: "market"},
		{ID: "n3", Dependencies: []string{"n1", "n2"}, Name: "report"},
	})
	wf.RebuildIndex()

	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	n1.Result = "财务数据: 营收+12%"

	n2, _ := wf.GetNode("n2")
	n2.Status = NodeCompleted
	n2.Result = "市场数据: 份额+3%"

	node, _ := wf.GetNode("n3")
	ctx := BuildUpstreamContext(wf, node)

	if !contains(ctx, "finance") || !contains(ctx, "market") {
		t.Errorf("expected both upstream nodes in context, got: %s", ctx)
	}
}

func TestBuildUpstreamContext_PartialUpstream(t *testing.T) {
	// n1 completed, n2 still running → only n1's result should be visible
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "done"},
		{ID: "n2", Name: "pending"},
		{ID: "n3", Dependencies: []string{"n1", "n2"}, Name: "merge"},
	})
	wf.RebuildIndex()

	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	n1.Result = "done result"

	// n2 still pending — not included

	node, _ := wf.GetNode("n3")
	ctx := BuildUpstreamContext(wf, node)

	if !contains(ctx, "done") {
		t.Error("expected completed upstream in context")
	}
	if contains(ctx, "pending") {
		t.Error("expected pending upstream to be excluded")
	}
}

func TestBuildUpstreamContext_LargeResult(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "large"},
		{ID: "n2", Dependencies: []string{"n1"}, Name: "consumer"},
	})
	wf.RebuildIndex()

	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted
	// Generate a result > 4000 chars (single upstream cap)
	large := make([]byte, 5000)
	for i := range large {
		large[i] = 'x'
	}
	n1.Result = string(large)

	node, _ := wf.GetNode("n2")
	ctx := BuildUpstreamContext(wf, node)

	// Should be truncated with "..."
	if !contains(ctx, "...") {
		t.Errorf("expected truncation marker in large result, got len=%d", len(ctx))
	}
}

// ============================================================================
// Helpers
// ============================================================================

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
