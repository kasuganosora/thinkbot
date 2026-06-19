package workflow

import (
	"encoding/json"
	"time"
)

// ============================================================================
// GORM 持久化模型
// ============================================================================

// WorkflowModel 是工作流的持久化模型。
// 使用 JSON 全量序列化策略：整个 Workflow 对象序列化为 Data 字段。
// 这样无需为每个字段建列，且支持未来结构变更的向前兼容。
type WorkflowModel struct {
	ID        string         `gorm:"primaryKey;column:id" json:"id"`
	Data      string         `gorm:"column:data;type:text" json:"data"` // 序列化的 Workflow JSON
	CreatedAt time.Time      `gorm:"column:created_at" json:"createdAt"`
	UpdatedAt time.Time      `gorm:"column:updated_at" json:"updatedAt"`
}

// TableName 指定表名。
func (WorkflowModel) TableName() string { return "workflow_workflows" }

// ToModel 将领域对象转为持久化模型。
func ToModel(wf *Workflow) (*WorkflowModel, error) {
	data, err := json.Marshal(wf)
	if err != nil {
		return nil, err
	}
	return &WorkflowModel{
		ID:        wf.ID,
		Data:      string(data),
		CreatedAt: wf.CreatedAt,
		UpdatedAt: time.Now(),
	}, nil
}

// FromModel 将持久化模型还原为领域对象。
func FromModel(m *WorkflowModel) (*Workflow, error) {
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
