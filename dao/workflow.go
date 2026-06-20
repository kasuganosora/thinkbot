package dao

import "time"

// WorkflowModel 工作流持久化表。
// 使用 JSON 全量序列化策略：整个 Workflow 对象序列化为 Data 字段。
type WorkflowModel struct {
	ID        string    `gorm:"primaryKey;column:id" json:"id"`
	Data      string    `gorm:"column:data;type:text" json:"data"` // 序列化的 Workflow JSON
	CreatedAt time.Time `gorm:"column:created_at" json:"createdAt"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updatedAt"`
}

// TableName 指定表名。
func (WorkflowModel) TableName() string { return "workflow_workflows" }
