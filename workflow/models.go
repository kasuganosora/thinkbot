package workflow

import (
	"encoding/json"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
	"go.uber.org/zap"
)

// pkgLogger 供包级函数（如 cloneWorkflow）使用的结构化日志。
// 通过 SetPkgLogger 设置（由 NewRepository 调用）。
var pkgLogger = zap.NewNop().Sugar()

// SetPkgLogger 设置包级日志器。应在初始化阶段调用。
func SetPkgLogger(l *zap.SugaredLogger) {
	if l != nil {
		pkgLogger = l
	}
}

// ============================================================================
// 持久化转换函数
// ============================================================================

// ToModel 将领域对象转为持久化模型。
func ToModel(wf *Workflow) (*dao.WorkflowModel, error) {
	data, err := json.Marshal(wf)
	if err != nil {
		return nil, err
	}
	// UpdatedAt 命中 gorm 的 autoUpdateTime 约定，经 db.Save 时会被 gorm 用自己的
	// time.Now() 覆盖；Repository.Save 会把 gorm 回填的真实时间戳同步回缓存快照，
	// 以保证 Get 的新鲜度比对不会误判。此处的兜底是给「不经 gorm 直接使用 model」的
	// 场景用的，保证字段非零。
	updatedAt := wf.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	return &dao.WorkflowModel{
		ID:        wf.ID,
		Data:      string(data),
		CreatedAt: wf.CreatedAt,
		UpdatedAt: updatedAt,
	}, nil
}

// FromModel 将持久化模型还原为领域对象。
func FromModel(m *dao.WorkflowModel) (*Workflow, error) {
	var wf Workflow
	if err := json.Unmarshal([]byte(m.Data), &wf); err != nil {
		return nil, err
	}
	// 用模型列（GORM 自动维护）回填最后落库时间，供卡死看门狗判陈旧。
	wf.UpdatedAt = m.UpdatedAt
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
		pkgLogger.Errorw("cloneWorkflow marshal failed", "error", err, "workflow_id", wf.ID)
		return &Workflow{ID: wf.ID, Status: wf.Status, Nodes: []*DAGNode{}}
	}
	var clone Workflow
	if err := json.Unmarshal(data, &clone); err != nil {
		pkgLogger.Errorw("cloneWorkflow unmarshal failed", "error", err, "workflow_id", wf.ID)
		return &Workflow{ID: wf.ID, Status: wf.Status, Nodes: []*DAGNode{}}
	}
	clone.EnsureIndex()
	return &clone
}
