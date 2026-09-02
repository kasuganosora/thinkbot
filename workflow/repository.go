package workflow

import (
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Repository — 持久化仓储（内存优先 + DB 双写）
// ============================================================================

// maxCacheSize 缓存条目上限。超过时淘汰最早的终态工作流。
const maxCacheSize = 500

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
	SetPkgLogger(logger)
	return &Repository{
		cache:  make(map[string]*Workflow),
		db:     db,
		logger: logger.With("stage", "workflow_repo"),
	}
}

// Save 保存工作流（内存 + DB 双写）。
// 内存缓存存储 **深拷贝快照**，确保后续 Get 返回的是不可变快照，
// 不会被 Scheduler 的并发写操作影响。
func (r *Repository) Save(wf *Workflow) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 维护最后落库时间（与 DB updated_at 列一致），供卡死看门狗判陈旧。
	wf.UpdatedAt = time.Now()

	// 深拷贝存入缓存，隔离 Scheduler 的实时修改
	snapshot := cloneWorkflow(wf)
	r.cache[wf.ID] = snapshot

	// 缓存超过上限时淘汰终态工作流
	if len(r.cache) > maxCacheSize {
		r.evictTerminal()
	}

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
		// 用 DB 实际写入的时间戳校准缓存快照。
		//
		// WorkflowModel.UpdatedAt 命中 gorm 的 autoUpdateTime 约定，保存时会被 gorm
		// 用自己的 time.Now() 覆盖，比上面赋的值略晚。若不校准，Get 的新鲜度比对会
		// 认为「自己刚写的缓存已过期」，每次读都白白重新从 DB 反序列化一遍。
		if !model.UpdatedAt.IsZero() {
			snapshot.UpdatedAt = model.UpdatedAt
			wf.UpdatedAt = model.UpdatedAt
		}
	}

	return nil
}

// evictTerminal 从缓存中淘汰最早的终态工作流，直到缓存大小降到 maxCacheSize 以下。
// 调用方必须持有 r.mu 写锁。
func (r *Repository) evictTerminal() {
	type entry struct {
		id        string
		createdAt time.Time
	}
	var terminal []entry
	for id, wf := range r.cache {
		if wf.Status.IsTerminal() {
			terminal = append(terminal, entry{id, wf.CreatedAt})
		}
	}
	// 按 createdAt 升序（最旧优先淘汰）
	sort.Slice(terminal, func(i, j int) bool {
		return terminal[i].createdAt.Before(terminal[j].createdAt)
	})
	for _, e := range terminal {
		if len(r.cache) <= maxCacheSize {
			break
		}
		delete(r.cache, e.id)
	}
}

// Get 获取工作流。
//
// 内存缓存只作为「本实例写过的快照」的快速通道，命中后仍需与 DB 的 updated_at 比对，
// 因为**同一进程里存在多个 Repository 实例**：
//   - api/botservice.go 为每个 bot 建一个引擎（带工作空间工具），它才是真正执行工作流、写 DB 的那个
//   - api/workflow_service.go 另建一个引擎，专门服务 HTTP 查询
//
// 两者各有独立缓存。若无条件信任缓存，API 侧会永远返回自己创建工作流那一刻的快照
// （status=analyzing、nodeCount=0），而实际执行进度只存在于 bot 侧缓存与 DB 中——
// 表现为「后端在跑、UI 永远显示分析中」，且刷新、清缓存都无效。
func (r *Repository) Get(id string) (*Workflow, error) {
	r.mu.RLock()
	cached, hit := r.cache[id]
	var cachedAt time.Time
	if hit {
		cachedAt = cached.UpdatedAt
	}
	r.mu.RUnlock()

	// 纯内存模式（db == nil）：缓存即唯一数据源。
	if r.db == nil {
		if hit {
			r.mu.RLock()
			clone := cloneWorkflow(cached)
			r.mu.RUnlock()
			return clone, nil
		}
		return nil, errs.Newf("workflow %s not found", id)
	}

	if hit {
		// 只取 updated_at 做新鲜度判断，避免每次都反序列化整个 Data。
		var head dao.WorkflowModel
		err := r.db.Model(&dao.WorkflowModel{}).
			Select("id", "updated_at").
			First(&head, "id = ?", id).Error
		if err != nil {
			// DB 不可用时退回缓存，保证可用性（宁可旧也不要报错）。
			r.mu.RLock()
			clone := cloneWorkflow(cached)
			r.mu.RUnlock()
			return clone, nil
		}
		// 缓存不比 DB 旧 → 直接用缓存。
		if !head.UpdatedAt.After(cachedAt) {
			r.mu.RLock()
			clone := cloneWorkflow(cached)
			r.mu.RUnlock()
			return clone, nil
		}
		// 否则落到下面重新从 DB 载入。
	}

	var model dao.WorkflowModel
	if err := r.db.First(&model, "id = ?", id).Error; err != nil {
		return nil, errs.Wrapf(err, "workflow %s not found", id)
	}
	wf, err := FromModel(&model)
	if err != nil {
		return nil, errs.Wrapf(err, "failed to deserialize workflow %s", id)
	}
	// 填充缓存（存入 clone），FromModel 返回的 wf 已是独立对象，可直接返回
	r.mu.Lock()
	r.cache[id] = cloneWorkflow(wf)
	r.mu.Unlock()
	return wf, nil
}

// FindNonTerminal 从 DB 中查找所有非终态工作流（analyzing/running/interrupted）。
// 纯内存模式下扫描缓存。
// 用于启动时崩溃恢复。
func (r *Repository) FindNonTerminal() ([]*Workflow, error) {
	if r.db != nil {
		var models []dao.WorkflowModel
		// GORM 无法在 JSON 内查询，所以取出全部然后内存过滤
		if err := r.db.Find(&models).Error; err != nil {
			return nil, errs.Wrap(err, "failed to query workflows from DB")
		}

		var result []*Workflow
		for i := range models {
			wf, err := FromModel(&models[i])
			if err != nil {
				r.logger.Warnw("failed to deserialize workflow during recovery scan",
					"workflow_id", models[i].ID, "error", err)
				continue
			}
			if wf.Status.IsRecoverable() {
				result = append(result, wf)
			}
		}
		return result, nil
	}

	// 纯内存模式
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Workflow
	for _, wf := range r.cache {
		if wf.Status.IsRecoverable() {
			result = append(result, cloneWorkflow(wf))
		}
	}
	return result, nil
}

// FindNeedingContinuation 从 DB 扫描终态且 NeedsContinuation==true 的工作流。
// 纯内存模式扫描缓存。用于启动续跑恢复：识别「工作流已完成但续跑回复因重启丢失」的
// 工作流，重新注入续跑消息唤醒 agent（仅一次）。
func (r *Repository) FindNeedingContinuation() ([]*Workflow, error) {
	if r.db != nil {
		var models []dao.WorkflowModel
		// GORM 无法在 JSON 内查询，所以取出全部然后内存过滤。
		if err := r.db.Find(&models).Error; err != nil {
			return nil, errs.Wrap(err, "failed to query workflows from DB")
		}
		var result []*Workflow
		for i := range models {
			wf, err := FromModel(&models[i])
			if err != nil {
				r.logger.Warnw("failed to deserialize workflow during continuation scan",
					"workflow_id", models[i].ID, "error", err)
				continue
			}
			if wf.Status.IsTerminal() && wf.NeedsContinuation {
				result = append(result, wf)
			}
		}
		return result, nil
	}

	// 纯内存模式
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Workflow
	for _, wf := range r.cache {
		if wf.Status.IsTerminal() && wf.NeedsContinuation {
			result = append(result, cloneWorkflow(wf))
		}
	}
	return result, nil
}

// List 列出最近的工作流（按创建时间降序，最多 limit 条）。
func (r *Repository) List(limit int) ([]*Workflow, error) {
	if limit <= 0 {
		limit = 20
	}

	if r.db != nil {
		var models []dao.WorkflowModel
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
	result := make([]*Workflow, 0, len(r.cache))
	for _, wf := range r.cache {
		result = append(result, cloneWorkflow(wf))
	}
	r.mu.RUnlock()
	// 按 CreatedAt 降序排序，与 DB 模式的 ORDER BY created_at DESC 保持一致
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
