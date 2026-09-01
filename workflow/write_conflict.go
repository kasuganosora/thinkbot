package workflow

import (
	"sort"
	"sync"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 并发写冲突检测
//
// 背景：工作流默认并行 3 个节点（MaxParallel），它们共享**同一个 bot 工作区**
// （sandbox/tools.go:86-89），且工作区层面**没有任何文件锁**。
// 两个节点覆盖同一文件时不会报错、不会留痕，表现为「结果莫名不对」。
//
// 设计取舍：**只检测、不阻断**。
//
// 串行化写操作会废掉并行这个核心价值，而我们对冲突的真实频率毫无数据。
// 先把冲突变成可见事件，积累数据后再决定要不要限制。
// 两种极端情况下的正确对策完全不同（没冲突→什么都不用做；
// 很频繁→需要按路径范围调度），没数据就动手是赌博。
// ============================================================================

// writeRecorder 记录单次节点执行期间的写操作。
//
// 实现 llm.PathRecorder，通过 ctx 注入给 sandbox 的写类工具。
// 必须线程安全：节点内部的多步编排可能并发调工具。
type writeRecorder struct {
	mu     sync.Mutex
	byPath map[string][]string // path → 操作列表
	order  []string            // 保持首次出现顺序，便于输出稳定
}

var _ llm.PathRecorder = (*writeRecorder)(nil)

func newWriteRecorder() *writeRecorder {
	return &writeRecorder{byPath: make(map[string][]string)}
}

// RecordWrite 实现 llm.PathRecorder。
func (r *writeRecorder) RecordWrite(path string, op string) {
	if path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byPath[path]; !exists {
		r.order = append(r.order, path)
	}
	r.byPath[path] = append(r.byPath[path], op)
}

// ops 返回本次执行写过的路径及其操作类型（拷贝，调用方可安全持有）。
func (r *writeRecorder) ops() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]string, len(r.order))
	for _, p := range r.order {
		out[p] = append([]string(nil), r.byPath[p]...)
	}
	return out
}

// WriteConflict 一次并发写冲突。
type WriteConflict struct {
	// Path 发生冲突的相对路径。
	Path string `json:"path"`
	// NodeIDs 涉及该路径的节点（按 ID 排序，便于稳定比对）。
	NodeIDs []string `json:"nodeIds"`
	// Ops 各节点在该路径上的操作类型（write/replace/delete/move）。
	Ops []string `json:"ops"`
	// Destructive 是否涉及删除或移动。
	//
	// 破坏性冲突更严重：一个节点删掉/移走文件，另一个节点正在读或写它，
	// 损失不可逆且通常不表现为错误。
	Destructive bool `json:"destructive"`
}

// destructiveOps 视为破坏性的操作类型。
func isDestructiveOp(op string) bool {
	return op == "delete" || op == "move"
}

// detectWriteConflicts 在整条工作流范围内检测写路径冲突。
//
// 只统计**已完成或正在执行**的节点——pending 节点还没写过任何东西，
// 把它们算进来会产生大量虚假冲突。
func (wf *Workflow) detectWriteConflicts() []WriteConflict {
	type entry struct {
		nodeID string
		op     string
	}
	byPath := make(map[string][]entry)

	for _, n := range wf.Nodes {
		if len(n.WrittenOps) == 0 {
			continue
		}
		if n.Status != NodeCompleted && n.Status != NodeRunning && n.Status != NodeReviewing {
			continue
		}
		for p, ops := range n.WrittenOps {
			for _, op := range ops {
				byPath[p] = append(byPath[p], entry{nodeID: n.ID, op: op})
			}
		}
	}

	var conflicts []WriteConflict
	for path, entries := range byPath {
		if len(entries) < 2 {
			continue
		}
		// 同一节点重复写同一路径不算冲突（它自己覆盖自己是有意的）
		nodeSet := make(map[string]bool, len(entries))
		for _, e := range entries {
			nodeSet[e.nodeID] = true
		}
		if len(nodeSet) < 2 {
			continue
		}

		ids := make([]string, 0, len(nodeSet))
		for id := range nodeSet {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		ops := make([]string, 0, len(entries))
		destructive := false
		for _, e := range entries {
			ops = append(ops, e.op)
			if isDestructiveOp(e.op) {
				destructive = true
			}
		}
		conflicts = append(conflicts, WriteConflict{
			Path:        path,
			NodeIDs:     ids,
			Ops:         ops,
			Destructive: destructive,
		})
	}

	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].Path < conflicts[j].Path
	})
	return conflicts
}
