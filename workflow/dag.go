package workflow

import (
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// DAG 领域算法
// ============================================================================

// Compile 编译工作流图：校验 DAG + 计算拓扑排序 + 构建邻接表和入度缓存。
// 必须在 Analyzer 生成节点后、Scheduler 运行前调用。
//
// 编译完成后 ReadyNodes/CascadeSkip/BuildTree 可复用预计算索引，
// 避免每次调用时遍历全图重建数据结构。
func (wf *Workflow) Compile() error {
	if len(wf.Nodes) == 0 {
		return errs.New("workflow has no nodes")
	}

	// 1. 校验 DAG 完整性（复用现有逻辑）
	if err := ValidateDAG(wf.Nodes); err != nil {
		return err
	}

	// 2. 构建编译缓存
	wf.topoOrder = topologicalSort(wf.Nodes)
	wf.reverseAdj = buildReverseAdjacency(wf.Nodes)
	wf.roots = findRoots(wf.Nodes)
	wf.inDegree = buildInDegrees(wf.Nodes)
	wf.compiled = true
	return nil
}

// topologicalSort 返回拓扑排序后的节点 ID 序列（Kahn 算法）。
func topologicalSort(nodes []*DAGNode) []string {
	inDegree := make(map[string]int, len(nodes))
	adjList := make(map[string][]string)

	for _, n := range nodes {
		inDegree[n.ID] = len(n.Dependencies)
		for _, dep := range n.Dependencies {
			adjList[dep] = append(adjList[dep], n.ID)
		}
	}

	queue := make([]string, 0)
	for _, n := range nodes {
		if inDegree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}

	result := make([]string, 0, len(nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		result = append(result, id)
		for _, dep := range adjList[id] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	return result
}

// buildReverseAdjacency 构建反向邻接表：nodeID → 依赖该节点的下游节点列表。
func buildReverseAdjacency(nodes []*DAGNode) map[string][]string {
	adj := make(map[string][]string)
	for _, n := range nodes {
		for _, dep := range n.Dependencies {
			adj[dep] = append(adj[dep], n.ID)
		}
	}
	return adj
}

// findRoots 返回入度为 0 的根节点 ID 列表。
func findRoots(nodes []*DAGNode) []string {
	var roots []string
	for _, n := range nodes {
		if len(n.Dependencies) == 0 {
			roots = append(roots, n.ID)
		}
	}
	return roots
}

// buildInDegrees 返回每个节点的入度（依赖数）。
func buildInDegrees(nodes []*DAGNode) map[string]int {
	deg := make(map[string]int, len(nodes))
	for _, n := range nodes {
		deg[n.ID] = len(n.Dependencies)
	}
	return deg
}

// ValidateDAG 校验 DAG 图的完整性：
//   - 节点 ID 唯一
//   - 所有依赖指向已存在的节点
//   - 无自引用（节点不能依赖自身）
//   - 无环（拓扑排序可完成）
func ValidateDAG(nodes []*DAGNode) error {
	if len(nodes) == 0 {
		return errs.New("workflow has no nodes")
	}

	// 检查 ID 唯一性 + 构建索引
	index := make(map[string]*DAGNode, len(nodes))
	for _, n := range nodes {
		if n.ID == "" {
			return errs.New("node with empty ID")
		}
		if _, exists := index[n.ID]; exists {
			return errs.Newf("duplicate node ID: %s", n.ID)
		}
		index[n.ID] = n
	}

	// 检查依赖有效性 + 无自引用
	for _, n := range nodes {
		for _, dep := range n.Dependencies {
			if dep == n.ID {
				return errs.Newf("node %q depends on itself", n.ID)
			}
			if _, ok := index[dep]; !ok {
				return errs.Newf("node %q depends on unknown node %q", n.ID, dep)
			}
		}
	}

	// 环检测（Kahn 算法）
	if cycle := detectCycle(nodes); cycle != nil {
		return errs.Newf("cycle detected: %s", strings.Join(cycle, " → "))
	}

	return nil
}

// detectCycle 使用 Kahn 拓扑排序检测环，返回环路径或 nil。
func detectCycle(nodes []*DAGNode) []string {
	inDegree := make(map[string]int, len(nodes))
	adjList := make(map[string][]string) // dep → dependents

	for _, n := range nodes {
		inDegree[n.ID] = len(n.Dependencies)
		for _, dep := range n.Dependencies {
			adjList[dep] = append(adjList[dep], n.ID)
		}
	}

	// 入度为 0 的节点入队
	queue := make([]string, 0)
	for _, n := range nodes {
		if inDegree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}

	processed := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		processed++
		for _, dependent := range adjList[id] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if processed == len(nodes) {
		return nil // 无环
	}

	// 找到环中的节点（入度仍 > 0 的节点）
	var cycleNodes []string
	for _, n := range nodes {
		if inDegree[n.ID] > 0 {
			cycleNodes = append(cycleNodes, n.ID)
		}
	}
	return cycleNodes
}

// ReadyNodes 返回所有依赖均已 completed 的 pending 节点。
// 编译后使用预计算的入度缓存，将检查从 O(n×deps) 降到 O(n)。
func ReadyNodes(wf *Workflow) []*DAGNode {
	ready := make([]*DAGNode, 0, len(wf.Nodes))
	if wf.compiled {
		// 快速路径：已有编译缓存，对每个 pending 节点只统计已完成依赖数
		for _, n := range wf.Nodes {
			if n.Status != NodePending {
				continue
			}
			done := countCompletedDeps(n, wf)
			if done == wf.inDegree[n.ID] {
				ready = append(ready, n)
			}
		}
	} else {
		// 慢速路径：无编译缓存时的兼容逻辑
		for _, n := range wf.Nodes {
			if n.Status != NodePending {
				continue
			}
			allDone := true
			for _, dep := range n.Dependencies {
				depNode, ok := wf.GetNode(dep)
				if !ok || depNode.Status != NodeCompleted {
					allDone = false
					break
				}
			}
			if allDone {
				ready = append(ready, n)
			}
		}
	}
	return ready
}

// countCompletedDeps 统计节点的依赖中有多少个已完成。
func countCompletedDeps(n *DAGNode, wf *Workflow) int {
	count := 0
	for _, dep := range n.Dependencies {
		if depNode, ok := wf.GetNode(dep); ok && depNode.Status == NodeCompleted {
			count++
		}
	}
	return count
}

// CascadeSkip 将指定失败节点的所有下游节点标记为 skipped。
// 递归传播：被跳过的节点也会导致其下游节点被跳过。
// 返回新被跳过的节点 ID 列表（不含已是终态的节点）。
// 编译后复用预计算的 reverseAdj，避免每次 BFS 时重建邻接表。
func CascadeSkip(wf *Workflow, failedNodeID string) []string {
	var dependents map[string][]string
	if wf.compiled && wf.reverseAdj != nil {
		dependents = wf.reverseAdj
	} else {
		dependents = make(map[string][]string)
		for _, n := range wf.Nodes {
			for _, dep := range n.Dependencies {
				dependents[dep] = append(dependents[dep], n.ID)
			}
		}
	}

	// BFS 传播
	var skipped []string
	queue := []string{failedNodeID}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		for _, childID := range dependents[id] {
			node, ok := wf.GetNode(childID)
			if !ok {
				continue
			}
			if node.Status == NodePending {
				node.Status = NodeSkipped
				node.Error = fmt.Sprintf("upstream node %q did not complete", id)
				skipped = append(skipped, childID)
			}
			queue = append(queue, childID)
		}
	}
	return skipped
}

// BuildTree 将平铺的 DAG 节点列表构建为依赖树。
// 根节点 = 无依赖的节点；子节点 = 依赖该节点的节点。
// 编译后复用预计算的 reverseAdj 和 roots。
func BuildTree(wf *Workflow) []*TreeNode {
	var children map[string][]string
	if wf.compiled && wf.reverseAdj != nil {
		children = wf.reverseAdj
	} else {
		children = make(map[string][]string)
		for _, n := range wf.Nodes {
			for _, dep := range n.Dependencies {
				children[dep] = append(children[dep], n.ID)
			}
		}
	}

	var roots []*TreeNode
	for _, n := range wf.Nodes {
		if len(n.Dependencies) == 0 {
			roots = append(roots, buildTreeNode(n.ID, wf, children, 0))
		}
	}
	return roots
}

// buildTreeNode 递归构建依赖树。使用 depth 限制替代全局 visited，
// 使得 DAG 中的菱形依赖（节点同时依赖多个上游）能在每个父节点下完整展示。
// ValidateDAG 已保证无环，depth 仅作防御性兜底。
func buildTreeNode(id string, wf *Workflow, children map[string][]string, depth int) *TreeNode {
	if depth > 256 { // 防御性深度限制
		return nil
	}

	node, ok := wf.GetNode(id)
	if !ok {
		return nil
	}

	tn := &TreeNode{Node: node}
	for _, childID := range children[id] {
		if child := buildTreeNode(childID, wf, children, depth+1); child != nil {
			tn.Children = append(tn.Children, child)
		}
	}
	return tn
}

// IsAllTerminal 检查所有节点是否都处于终态。
func IsAllTerminal(wf *Workflow) bool {
	for _, n := range wf.Nodes {
		if !n.Status.IsTerminal() {
			return false
		}
	}
	return true
}

// HasFailedNode 检查是否存在 failed 节点。
func HasFailedNode(wf *Workflow) bool {
	for _, n := range wf.Nodes {
		if n.Status == NodeFailed {
			return true
		}
	}
	return false
}

// ============================================================================
// 上游结果聚合
// ============================================================================

// maxUpstreamContextChars 单节点注入上游结果的最大字符数。
const maxUpstreamContextChars = 8000

// BuildUpstreamContext 为指定节点构建上游结果上下文。
// 将已完成依赖节点的产物聚合为 SubAgent 输入前缀，
// 使 LLM 能引用上游产出提高输出质量。
//
// 设计原则：
//   - 每个上游节点标注"节点名称 + 产出摘要"
//   - 总长度超过 maxUpstreamContextChars 时截断
//   - 未完成的依赖不包含（运行时自然为空）
func BuildUpstreamContext(wf *Workflow, node *DAGNode) string {
	if len(node.Dependencies) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[Upstream Task Results]\n")
	totalChars := 0
	included := 0

	for _, depID := range node.Dependencies {
		dep, ok := wf.GetNode(depID)
		if !ok || dep.Status != NodeCompleted || dep.Result == "" {
			continue
		}
		if totalChars >= maxUpstreamContextChars {
			fmt.Fprintf(&sb, "\n... (%d more upstream node results omitted)\n", len(node.Dependencies)-included)
			break
		}
		line := fmt.Sprintf("%s(%s): %s\n", dep.ID, dep.Name, strings.TrimSpace(dep.Result))
		// 单个上游结果限制 4000 字符
		if len(line) > 4000 {
			line = line[:3997] + "...\n"
		}
		sb.WriteString(line)
		totalChars += len(line)
		included++
	}

	if included == 0 {
		return ""
	}

	sb.WriteString("\n[Your Task]\n")
	sb.WriteString(node.Task)
	return sb.String()
}

// ============================================================================
// 动态 DAG 修补：把失败节点替换为细化后的子图
// ============================================================================

// healSentinelUpstream 是 RefineNode 产出的子图节点用来标记「继承原节点上游依赖」的哨兵。
const healSentinelUpstream = "__UP__"

// ReplaceNodeWithSubgraph 将 oldID 节点从 DAG 中移除，替换为 sub 子图，并保持拓扑连续：
//   - 原节点的上游依赖（Dependencies）转移给子图入口节点（依赖含 healSentinelUpstream 哨兵者）；
//   - 子图内部依赖按相对 id 重映射为新 id（避免与现有节点 id 冲突，前缀 oldID+"-hN"）；
//   - 子图叶子（子图内无节点依赖它的节点）自动成为原节点下游节点的新依赖；
//   - 若子图没有任何入口声明上游（模型漏写哨兵），兜底把所有「无内部依赖」的子图节点接回上游，
//     防止子图浮空、上游未就绪就执行。
//
// 调用方必须持有 s.mu（Scheduler 在 trySelfHeal 内已加锁）。本方法会重校验 DAG 并重新 Compile
// （重算 topoOrder/reverseAdj/inDegree 缓存），使调度主循环下一轮能自然纳入新子图节点。
// 返回 error 表示替换破坏了 DAG 契约（调用方应退回原 failed 路径，不应用本次替换）。
func (wf *Workflow) ReplaceNodeWithSubgraph(oldID string, sub []*DAGNode) error {
	old, ok := wf.GetNode(oldID)
	if !ok {
		return errs.Newf("heal: replace target node %q not found", oldID)
	}
	if len(sub) == 0 {
		return errs.New("heal: empty subgraph")
	}

	// 1. 原节点的上游依赖与下游（依赖原节点的节点）。
	upDeps := append([]string(nil), old.Dependencies...)
	downstream := wf.reverseAdj[oldID]
	if downstream == nil {
		// 未编译时退化为临时构建（极端防御，运行时 Workflow 必已编译）。
		downstream = buildReverseAdjacency(wf.Nodes)[oldID]
	}

	// 2. 重命名子图节点 + 重映射内部依赖 + 标记 __UP__ 哨兵。
	renamed := make([]*DAGNode, 0, len(sub))
	idMap := make(map[string]string, len(sub)) // 相对 id -> 新 id
	for i, sn := range sub {
		newID := fmt.Sprintf("%s-h%d", oldID, i+1)
		idMap[sn.ID] = newID
		rn := *sn // 浅拷贝配置字段；运行时段（Status/Result/...）归零
		rn.ID = newID
		rn.Status = NodePending
		rn.Result = ""
		rn.Error = ""
		rn.RetryCount = 0
		rn.IterationCount = 0
		rn.Dependencies = make([]string, len(sn.Dependencies))
		for j, dep := range sn.Dependencies {
			switch dep {
			case healSentinelUpstream:
				rn.Dependencies[j] = "" // 占位，稍后填 upDeps
			case "":
				rn.Dependencies[j] = "" // 同样视作哨兵占位
			default:
				if mapped, ok := idMap[dep]; ok {
					rn.Dependencies[j] = mapped
				} else {
					rn.Dependencies[j] = dep
				}
			}
		}
		renamed = append(renamed, &rn)
	}

	// 3. 把 __UP__ 哨兵（占位 ""）替换为原节点的真实上游依赖。
	for _, rn := range renamed {
		replaced := false
		for j, dep := range rn.Dependencies {
			if dep == "" { // 哨兵占位
				head := rn.Dependencies[:j]
				tail := rn.Dependencies[j+1:]
				rn.Dependencies = append(append(head, upDeps...), tail...)
				replaced = true
				break
			}
		}
		// 兜底：该子图节点既无哨兵也无内部依赖（模型漏写上游）→ 接回原上游，防浮空。
		if !replaced && len(rn.Dependencies) == 0 {
			rn.Dependencies = append(rn.Dependencies, upDeps...)
		}
	}

	// 4. 找子图叶子 = 子图内没有其它子图节点以其为依赖的节点。
	childSet := make(map[string]bool, len(renamed))
	for _, rn := range renamed {
		for _, dep := range rn.Dependencies {
			childSet[dep] = true
		}
	}
	leaves := make([]string, 0, len(renamed))
	for _, rn := range renamed {
		if !childSet[rn.ID] {
			leaves = append(leaves, rn.ID)
		}
	}
	if len(leaves) == 0 {
		// 子图成环（不应发生，ValidateDAG 会拦），保底用最后一个节点当出口。
		leaves = []string{renamed[len(renamed)-1].ID}
	}

	// 5. 原节点的下游：dependencies 中的 oldID 替换为子图叶子（AND 依赖，全部完成才续跑）。
	for _, dnID := range downstream {
		dn, ok := wf.GetNode(dnID)
		if !ok {
			continue
		}
		newDeps := make([]string, 0, len(dn.Dependencies)+len(leaves))
		replaced := false
		for _, dep := range dn.Dependencies {
			if dep == oldID {
				newDeps = append(newDeps, leaves...)
				replaced = true
			} else {
				newDeps = append(newDeps, dep)
			}
		}
		if replaced {
			dn.Dependencies = newDeps
		}
	}

	// 6. 从 Nodes 移除 old，插入 renamed。
	newNodes := make([]*DAGNode, 0, len(wf.Nodes)+len(renamed))
	for _, n := range wf.Nodes {
		if n.ID == oldID {
			continue
		}
		newNodes = append(newNodes, n)
	}
	newNodes = append(newNodes, renamed...)
	wf.Nodes = newNodes

	// 7. 校验 + 重建索引 + 重编译（刷新调度缓存）。
	if err := ValidateDAG(wf.Nodes); err != nil {
		return errs.Wrap(err, "heal: refined subgraph invalidates DAG")
	}
	wf.RebuildIndex()
	if err := wf.Compile(); err != nil {
		return errs.Wrap(err, "heal: refined subgraph compile failed")
	}
	return nil
}
