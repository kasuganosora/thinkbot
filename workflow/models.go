package workflow

import (
	"encoding/json"
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
		// 理论上不会失败（Workflow 只含基本类型）
		return wf // fallback：返回原指针（降级但不崩溃）
	}
	var clone Workflow
	if err := json.Unmarshal(data, &clone); err != nil {
		return wf
	}
	clone.EnsureIndex()
	return &clone
}
