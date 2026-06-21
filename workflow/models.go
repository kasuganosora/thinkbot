package workflow

import (
	"encoding/json"
	"log"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// 持久化转换函数
// ============================================================================

// ToModel 将领域对象转为持久化模型。
func ToModel(wf *Workflow) (*dao.WorkflowModel, error) {
	data, err := json.Marshal(wf)
	if err != nil {
		return nil, err
	}
	return &dao.WorkflowModel{
		ID:        wf.ID,
		Data:      string(data),
		CreatedAt: wf.CreatedAt,
		UpdatedAt: time.Now(),
	}, nil
}

// FromModel 将持久化模型还原为领域对象。
func FromModel(m *dao.WorkflowModel) (*Workflow, error) {
	var wf Workflow
	if err := json.Unmarshal([]byte(m.Data), &wf); err != nil {
		return nil, err
	}
	wf.EnsureIndex()
	return &wf, nil
}

// cloneWorkflow 通过 JSON 序列化/反序列化创建工作流的深拷贝。
// 用于 Repository 存储快照，隔离 Scheduler 的并发写操作。
func cloneWorkflow(wf *Workflow) *Workflow {
	data, err := json.Marshal(wf)
	if err != nil {
		// 极不应该发生：Workflow 只含基本类型和切片。
		// 记录日志便于排查，返回包含 ID 和 Status 的空快照（绝不返回原指针）。
		log.Printf("[workflow] cloneWorkflow marshal failed: %v (workflow_id=%s)", err, wf.ID)
		return &Workflow{ID: wf.ID, Status: wf.Status, Nodes: []*DAGNode{}}
	}
	var clone Workflow
	if err := json.Unmarshal(data, &clone); err != nil {
		log.Printf("[workflow] cloneWorkflow unmarshal failed: %v (workflow_id=%s)", err, wf.ID)
		return &Workflow{ID: wf.ID, Status: wf.Status, Nodes: []*DAGNode{}}
	}
	clone.EnsureIndex()
	return &clone
}
