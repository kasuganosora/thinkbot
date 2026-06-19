package workflow

import (
	"sync"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Repository — 持久化仓储（内存优先 + DB 双写）
// ============================================================================

// Repository 管理工作流的持久化。
// 读操作优先从内存 map 获取（O(1)），写操作同时更新内存和 DB。
type Repository struct {
	mu     sync.RWMutex
	cache  map[string]*Workflow
	db     *gorm.DB
	logger *zap.SugaredLogger
}

// NewRepository 创建仓储实例。
// db 可为 nil（纯内存模式，适用于测试）。
func NewRepository(db *gorm.DB, logger *zap.SugaredLogger) *Repository {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Repository{
		cache:  make(map[string]*Workflow),
		db:     db,
		logger: logger.With("component", "workflow_repo"),
	}
}

// Save 保存工作流（内存 + DB 双写）。
// 内存缓存存储 **深拷贝快照**，确保后续 Get 返回的是不可变快照，
// 不会被 Scheduler 的并发写操作影响。
func (r *Repository) Save(wf *Workflow) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 深拷贝存入缓存，隔离 Scheduler 的实时修改
	snapshot := cloneWorkflow(wf)
	r.cache[wf.ID] = snapshot

	if r.db != nil {
		model, err := ToModel(wf)
		if err != nil {
			return errs.Wrapf(err, "failed to serialize workflow %s", wf.ID)
		}
		if err := r.db.Save(model).Error; err != nil {
			r.logger.Errorw("failed to persist workflow to DB",
				"workflow_id", wf.ID, "error", err)
			return errs.Wrapf(err, "failed to save workflow %s to DB", wf.ID)
		}
	}

	return nil
}

// Get 从内存缓存获取工作流，缓存未命中时回退到 DB。
func (r *Repository) Get(id string) (*Workflow, error) {
	r.mu.RLock()
	if wf, ok := r.cache[id]; ok {
		r.mu.RUnlock()
		return wf, nil
	}
	r.mu.RUnlock()

	// 回退到 DB
	if r.db != nil {
		var model WorkflowModel
		if err := r.db.First(&model, "id = ?", id).Error; err != nil {
			return nil, errs.Wrapf(err, "workflow %s not found", id)
		}
		wf, err := FromModel(&model)
		if err != nil {
			return nil, errs.Wrapf(err, "failed to deserialize workflow %s", id)
		}
		// 填充缓存
		r.mu.Lock()
		r.cache[id] = wf
		r.mu.Unlock()
		return wf, nil
	}

	return nil, errs.Newf("workflow %s not found", id)
}

// List 列出最近的工作流（按创建时间降序，最多 limit 条）。
func (r *Repository) List(limit int) ([]*Workflow, error) {
	if limit <= 0 {
		limit = 20
	}

	if r.db != nil {
		var models []WorkflowModel
		if err := r.db.Order("created_at DESC").Limit(limit).Find(&models).Error; err != nil {
			return nil, errs.Wrap(err, "failed to list workflows from DB")
		}
		result := make([]*Workflow, 0, len(models))
		for i := range models {
			wf, err := FromModel(&models[i])
			if err != nil {
				continue
			}
			result = append(result, wf)
		}
		return result, nil
	}

	// 纯内存模式
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Workflow, 0, len(r.cache))
	for _, wf := range r.cache {
		result = append(result, wf)
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
