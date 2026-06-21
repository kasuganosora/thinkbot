package workflow

import (
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// DAG 领域算法
// ============================================================================

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
func ReadyNodes(wf *Workflow) []*DAGNode {
	ready := make([]*DAGNode, 0, len(wf.Nodes))
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
	return ready
}

// CascadeSkip 将指定失败节点的所有下游节点标记为 skipped。
// 递归传播：被跳过的节点也会导致其下游节点被跳过。
func CascadeSkip(wf *Workflow, failedNodeID string) {
	// 构建邻接表：node → dependents
	dependents := make(map[string][]string)
	for _, n := range wf.Nodes {
		for _, dep := range n.Dependencies {
			dependents[dep] = append(dependents[dep], n.ID)
		}
	}

	// BFS 传播
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
			}
			queue = append(queue, childID)
		}
	}
}

// BuildTree 将平铺的 DAG 节点列表构建为依赖树。
// 根节点 = 无依赖的节点；子节点 = 依赖该节点的节点。
func BuildTree(wf *Workflow) []*TreeNode {
	// 构建邻接表
	children := make(map[string][]string)
	for _, n := range wf.Nodes {
		for _, dep := range n.Dependencies {
			children[dep] = append(children[dep], n.ID)
		}
	}

	var roots []*TreeNode
	for _, n := range wf.Nodes {
		if len(n.Dependencies) == 0 {
			roots = append(roots, buildTreeNode(n.ID, wf, children, make(map[string]bool)))
		}
	}
	return roots
}

func buildTreeNode(id string, wf *Workflow, children map[string][]string, visited map[string]bool) *TreeNode {
	if visited[id] {
		return nil // 防止无限递归（虽然已验证无环，但防御性编程）
	}
	visited[id] = true

	node, ok := wf.GetNode(id)
	if !ok {
		return nil
	}

	tn := &TreeNode{Node: node}
	for _, childID := range children[id] {
		if child := buildTreeNode(childID, wf, children, visited); child != nil {
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
