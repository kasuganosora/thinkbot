// Package workflow 实现供主 Agent 使用的复杂任务 DAG 工作流引擎。
//
// 核心流程：
//  1. 主 Agent 提交需求文本 → Analyzer 用 LLM 分解为 DAG 节点图
//  2. Scheduler 按 DAG 拓扑序调度，同层无依赖节点并行执行
//  3. 每个节点由独立 SubAgent 执行，支持失败重试 + Review 自循环迭代
//  4. 全程异步：提交后立即返回 workflow_id，LLM 通过工具轮询进度
package workflow

import (
	"time"
)

// ============================================================================
// 枚举
// ============================================================================

// NodeStatus 节点运行状态。
type NodeStatus string

const (
	NodePending   NodeStatus = "pending"   // 等待依赖完成
	NodeReady     NodeStatus = "ready"     // 依赖已满足，等待执行
	NodeRunning   NodeStatus = "running"   // 正在执行
	NodeReviewing NodeStatus = "reviewing" // 正在 Review
	NodeCompleted NodeStatus = "completed" // 成功完成
	NodeFailed    NodeStatus = "failed"    // 执行失败（重试耗尽）
	NodeSkipped   NodeStatus = "skipped"   // 因上游失败而跳过
)

// WorkflowStatus 工作流整体状态。
type WorkflowStatus string

const (
	WorkflowAnalyzing   WorkflowStatus = "analyzing"   // 正在分析需求、构建 DAG
	WorkflowRunning     WorkflowStatus = "running"     // DAG 正在执行
	WorkflowInterrupted WorkflowStatus = "interrupted" // 进程崩溃中断（等待恢复）
	WorkflowCompleted   WorkflowStatus = "completed"   // 全部节点成功
	WorkflowFailed      WorkflowStatus = "failed"      // 存在失败节点
	WorkflowTerminated  WorkflowStatus = "terminated"  // 被手动终止
)

// ============================================================================
// 领域模型
// ============================================================================

// DAGNode 是 DAG 图中的一个执行节点。
//
// 配置字段（由 Analyzer 生成）：
//   - Dependencies: 依赖节点 ID 列表（AND 依赖，全部完成后才能执行）
//   - Review: 为 true 时执行后启动 Review SubAgent 检查产物
//   - MaxRetries: 执行报错时的最大重试次数
//   - MaxIterations: Review 不通过时的最大迭代次数
type DAGNode struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Task          string   `json:"task"`                   // SubAgent 执行的任务描述
	SystemPrompt  string   `json:"systemPrompt,omitempty"` // SubAgent 角色 prompt
	Dependencies  []string `json:"dependencies,omitempty"` // 依赖节点 ID（AND 依赖）
	Review        bool     `json:"review"`                 // 是否需要结果审查
	ReviewPrompt  string   `json:"reviewPrompt,omitempty"` // 审查 SubAgent 的自定义 prompt
	MaxRetries    int      `json:"maxRetries"`             // 执行错误最大重试次数（默认 2）
	MaxIterations int      `json:"maxIterations"`          // Review 迭代上限（默认 3）

	// Feedback 声明该节点 review 不通过时回退到的「上游节点」列表（目标模式闭环）。
	// 仅当所属工作流开启 GoalMode 且本节点 Review=true 时生效。
	// 反馈边是「受控闭环」：它不计入 DAG 的 Dependencies，因此不影响拓扑排序与环检测
	// （detectCycle 只看 Dependencies），逻辑图始终保持无环。
	// 当 review 节点在节点级迭代（MaxIterations）后仍不通过，且目标模式迭代额度未耗尽，
	// Scheduler 会把这些 Feedback 目标节点（及本节点）重置为 pending 重新执行，
	// 并把 review 意见作为反馈注入目标节点——即「工作→审查→修复→审查」的全局循环。
	Feedback []string `json:"feedback,omitempty"`

	// 运行时状态（非 Analyzer 生成，由 Scheduler 更新）
	Status         NodeStatus     `json:"status"`
	Result         string         `json:"result,omitempty"`
	Error          string         `json:"error,omitempty"`
	RetryCount     int            `json:"retryCount"`
	IterationCount int            `json:"iterationCount"`
	StartedAt      *time.Time     `json:"startedAt,omitempty"`
	CompletedAt    *time.Time     `json:"completedAt,omitempty"`
	ReviewFeedback string         `json:"reviewFeedback,omitempty"`
	ReviewHistory  []ReviewRecord `json:"reviewHistory,omitempty"`

	// LoopFeedback 目标模式闭环时由 Scheduler 写入的「审查意见」，
	// 供节点在下一轮执行（ExecuteWithFeedback）时作为修复依据。执行后立即清空。
	// 运行时字段（不持久化语义依赖，但序列化无害）。
	LoopFeedback string `json:"loopFeedback,omitempty"`
}

// ReviewRecord 记录一次 Review 的结果。
type ReviewRecord struct {
	Iteration int    `json:"iteration"`
	Passed    bool   `json:"passed"`
	Feedback  string `json:"feedback,omitempty"`
}

// Workflow 是一个完整的工作流实例。
type Workflow struct {
	ID          string         `json:"id"`
	Status      WorkflowStatus `json:"status"`
	Requirement string         `json:"requirement"`
	Nodes       []*DAGNode     `json:"nodes"`
	CreatedAt   time.Time      `json:"createdAt"`
	StartedAt   *time.Time     `json:"startedAt,omitempty"`
	FinishedAt  *time.Time     `json:"finishedAt,omitempty"`
	Error       string         `json:"error,omitempty"`

	// AnalyzeMessage 分析阶段的实时进度文案（如「模型返回异常，第 2/5 次重试」）。
	// 用于前端在分析阶段展示进展，避免「分析中…」长期无变化被误判为卡死。
	// 由 Manager 在 Analyze 的 onProgress 回调中更新并持久化。
	AnalyzeMessage string `json:"analyzeMessage,omitempty"`
	// UpdatedAt 最后落库时间（由 Repository.Save 维护，与 DB 的 updated_at 列一致）。
	// 供卡死看门狗（Sweeper）判断工作流是否长时间无进展。
	UpdatedAt time.Time `json:"updatedAt,omitempty"`

	// 目标模式（Goal Mode）：开启后，review 节点在节点级迭代（MaxIterations）仍不通过时，
	// 不立即失败，而是回退到其 Feedback 目标节点重新执行（并注入审查意见），
	// 形成「工作→审查→修复→审查」的全局闭环，直到 review 通过或达到最大迭代轮数。
	// 由提交时（SubmitRequest.GoalMode）或分析器（GoalMode 开启时自动接线）设置。
	GoalMode bool `json:"goalMode,omitempty"`
	// GoalIteration 目标模式已执行的闭环轮数（运行时计数，持久化以在前端展示）。
	GoalIteration int `json:"goalIteration,omitempty"`
	// GoalMaxIterations 目标模式最大闭环轮数（0 表示使用引擎默认配置）。
	GoalMaxIterations int `json:"goalMaxIterations,omitempty"`

	// 内部索引，不序列化
	nodeIndex map[string]*DAGNode `json:"-"`

	// 编译缓存（由 Compile() 填充，不序列化）
	compiled   bool                `json:"-"`
	topoOrder  []string            `json:"-"` // 拓扑排序后的节点 ID 序列
	reverseAdj map[string][]string `json:"-"` // nodeID → 依赖该节点的节点列表（下游）
	inDegree   map[string]int      `json:"-"` // nodeID → 入度（依赖数）
	roots      []string            `json:"-"` // 入度为 0 的根节点
}

// Compiled returns true if Compile() has been called successfully.
func (wf *Workflow) Compiled() bool { return wf.compiled }

// NewWorkflow 创建工作流并初始化节点索引。
func NewWorkflow(id, requirement string, nodes []*DAGNode) *Workflow {
	wf := &Workflow{
		ID:          id,
		Status:      WorkflowAnalyzing,
		Requirement: requirement,
		Nodes:       nodes,
		CreatedAt:   time.Now(),
		nodeIndex:   make(map[string]*DAGNode, len(nodes)),
	}
	for _, n := range nodes {
		n.Status = NodePending
		wf.nodeIndex[n.ID] = n
	}
	return wf
}

// GetNode 根据 ID 查找节点。
func (wf *Workflow) GetNode(id string) (*DAGNode, bool) {
	n, ok := wf.nodeIndex[id]
	return n, ok
}

// EnsureIndex 确保节点索引已初始化（反序列化后调用）。
// 如果索引为 nil 则构建；如果已存在则保留（避免并发重建）。
func (wf *Workflow) EnsureIndex() {
	if wf.nodeIndex == nil {
		wf.nodeIndex = make(map[string]*DAGNode, len(wf.Nodes))
		for _, n := range wf.Nodes {
			wf.nodeIndex[n.ID] = n
		}
	}
}

// RebuildIndex 强制重建节点索引（当 Nodes 列表发生变化后调用）。
func (wf *Workflow) RebuildIndex() {
	wf.nodeIndex = make(map[string]*DAGNode, len(wf.Nodes))
	for _, n := range wf.Nodes {
		wf.nodeIndex[n.ID] = n
	}
}

// IsTerminal 判断节点状态是否为终态。
func (s NodeStatus) IsTerminal() bool {
	return s == NodeCompleted || s == NodeFailed || s == NodeSkipped
}

// IsTerminal 判断工作流状态是否为终态。
func (s WorkflowStatus) IsTerminal() bool {
	return s == WorkflowCompleted || s == WorkflowFailed || s == WorkflowTerminated
}

// IsRecoverable 判断工作流状态是否可恢复（非终态）。
func (s WorkflowStatus) IsRecoverable() bool {
	return s == WorkflowAnalyzing || s == WorkflowRunning || s == WorkflowInterrupted
}

// ============================================================================
// 视图结构（用于 API 返回）
// ============================================================================

// TreeNode 用于树状展示节点依赖关系。
type TreeNode struct {
	Node     *DAGNode    `json:"node"`
	Children []*TreeNode `json:"children,omitempty"`
}

// NodeFlat 用于平铺展示节点列表。
type NodeFlat struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Status         NodeStatus `json:"status"`
	Task           string     `json:"task"`
	Result         string     `json:"result,omitempty"`
	Error          string     `json:"error,omitempty"`
	Dependencies   []string   `json:"dependencies,omitempty"`
	Review         bool       `json:"review"`
	RetryCount     int        `json:"retryCount"`
	IterationCount int        `json:"iterationCount"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}

// ToFlat 将 DAGNode 转为精简的 NodeFlat 视图。
func (n *DAGNode) ToFlat() NodeFlat {
	return NodeFlat{
		ID:             n.ID,
		Name:           n.Name,
		Status:         n.Status,
		Task:           n.Task,
		Result:         n.Result,
		Error:          n.Error,
		Dependencies:   n.Dependencies,
		Review:         n.Review,
		RetryCount:     n.RetryCount,
		IterationCount: n.IterationCount,
		StartedAt:      n.StartedAt,
		CompletedAt:    n.CompletedAt,
	}
}
