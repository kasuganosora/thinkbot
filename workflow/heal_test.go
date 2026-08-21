package workflow

import (
	"testing"
)

// TestReplaceNodeWithSubgraphBasic 验证动态替换的核心不变量：
//   - 原失败节点被移除；
//   - 子图节点 id 重映射（前缀 oldID+"-hN"），无冲突；
//   - 子图入口继承原上游依赖；
//   - 子图叶子接回原下游（拓扑连续，下游不再依赖已删除的节点）；
//   - 替换后 DAG 仍合法、可重编译、主循环能正确调度。
func TestReplaceNodeWithSubgraphBasic(t *testing.T) {
	wf := &Workflow{
		ID: "wf-test",
		Nodes: []*DAGNode{
			{ID: "n1", Dependencies: []string{}, Status: NodePending},
			{ID: "n2", Dependencies: []string{"n1"}, Status: NodePending}, // 将被替换的失败节点
			{ID: "n3", Dependencies: []string{"n2"}, Status: NodePending},
		},
	}
	wf.EnsureIndex()
	if err := wf.Compile(); err != nil {
		t.Fatalf("initial compile: %v", err)
	}

	sub := []*DAGNode{
		{ID: "a", Dependencies: []string{healSentinelUpstream}, Status: NodePending},
		{ID: "b", Dependencies: []string{"a"}, Status: NodePending},
	}
	if err := wf.ReplaceNodeWithSubgraph("n2", sub); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if _, ok := wf.GetNode("n2"); ok {
		t.Fatal("old node n2 should be removed")
	}
	a, ok := wf.GetNode("n2-h1")
	if !ok {
		t.Fatal("n2-h1 missing")
	}
	b, ok := wf.GetNode("n2-h2")
	if !ok {
		t.Fatal("n2-h2 missing")
	}
	// 子图入口继承原上游 n1
	if len(a.Dependencies) != 1 || a.Dependencies[0] != "n1" {
		t.Fatalf("n2-h1 deps = %v, want [n1]", a.Dependencies)
	}
	// 子图内部边重映射 a->b
	if len(b.Dependencies) != 1 || b.Dependencies[0] != "n2-h1" {
		t.Fatalf("n2-h2 deps = %v, want [n2-h1]", b.Dependencies)
	}
	// 原下游 n3 接回子图叶子 n2-h2
	n3, _ := wf.GetNode("n3")
	if len(n3.Dependencies) != 1 || n3.Dependencies[0] != "n2-h2" {
		t.Fatalf("n3 deps = %v, want [n2-h2]", n3.Dependencies)
	}
	// 替换后仍合法、可重编译
	if err := wf.Compile(); err != nil {
		t.Fatalf("recompile: %v", err)
	}
	// 主循环：仅 n1 就绪，n2-h1 等 n1
	ready := ReadyNodes(wf)
	ids := make([]string, 0, len(ready))
	for _, n := range ready {
		ids = append(ids, n.ID)
	}
	if len(ids) != 1 || ids[0] != "n1" {
		t.Fatalf("ready = %v, want [n1]", ids)
	}
}

// TestReplaceNodeWithSubgraphDiamond 验证多叶子场景：下游节点应依赖所有叶子（AND）。
func TestReplaceNodeWithSubgraphDiamond(t *testing.T) {
	wf := &Workflow{
		ID: "wf-d",
		Nodes: []*DAGNode{
			{ID: "n1", Dependencies: []string{}, Status: NodePending},
			{ID: "n2", Dependencies: []string{"n1"}, Status: NodePending},
			{ID: "n3", Dependencies: []string{"n2"}, Status: NodePending},
		},
	}
	wf.EnsureIndex()
	if err := wf.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	// 两个入口（都依赖上游）、一个公共叶子
	sub := []*DAGNode{
		{ID: "x", Dependencies: []string{healSentinelUpstream}, Status: NodePending},
		{ID: "y", Dependencies: []string{healSentinelUpstream}, Status: NodePending},
		{ID: "z", Dependencies: []string{"x", "y"}, Status: NodePending},
	}
	if err := wf.ReplaceNodeWithSubgraph("n2", sub); err != nil {
		t.Fatalf("replace: %v", err)
	}
	n3, _ := wf.GetNode("n3")
	// 叶子是 z（n2-h3），下游 n3 应依赖 [n2-h3]
	if len(n3.Dependencies) != 1 || n3.Dependencies[0] != "n2-h3" {
		t.Fatalf("n3 deps = %v, want [n2-h3]", n3.Dependencies)
	}
	if err := wf.Compile(); err != nil {
		t.Fatalf("recompile: %v", err)
	}
}

// TestParseDiagnosis 验证诊断 JSON 的容错提取。
func TestParseDiagnosis(t *testing.T) {
	raw := `一些前言 <result>{"category":"granularity","confidence":0.85,"reason":"模块过大","suggested_action":"refine","refine_hint":"按子目录拆"}</result> 后缀`
	d, ok := parseDiagnosis(raw)
	if !ok {
		t.Fatal("parseDiagnosis should succeed")
	}
	if d.Category != "granularity" || d.Confidence != 0.85 || d.RefineHint == "" {
		t.Fatalf("unexpected: %+v", d)
	}

	if _, ok := parseDiagnosis("完全没有 json 块"); ok {
		t.Fatal("parseDiagnosis should fail on non-JSON")
	}
	if _, ok := parseDiagnosis("<result>{\"category\":\"\"}</result>"); ok {
		t.Fatal("parseDiagnosis should fail when category empty")
	}
}
