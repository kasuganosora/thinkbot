package storage

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// TieredL1Retriever — 从分层记忆表读取 L1（梦境升华后的长期知识）
// ============================================================================
//
// 背景：recall stage 此前只消费 memory_entries（原始笔记），梦境子系统每天
// 凌晨把碎碎念提炼成「人物画像 / 项目事实」存入 tiered_memories(tier=1)，但这些
// 蒸馏知识从未进入对话 prompt（实测 110 条 L1 与 memory_entries 零重合）。
// 整个 dreaming 子系统等于对对话不可见。
//
// 本 retriever 把 tiered_memories 的 L1 暴露为 memory.Retriever，与 memory_entries
// 的 memRepo 经 MergedRetriever 合并后注入召回，形成「潜水学到的经验在真人交互里
// 浮现」的完整闭环。
//
// 注意：recall stage 在 lurk（只读）消息上会提前返回，故 L1 只在真人对话轮次被召回，
// 不会在观察者自身产出笔记时回环。

// TieredL1Retriever 从 tiered_memories 表读取 tier=1 的记忆。
// 编译期契约：实现 memory.Retriever，可直接作为 recall stage 的检索源。
var _ memory.Retriever = (*TieredL1Retriever)(nil)

type TieredL1Retriever struct {
	db *gorm.DB
}

// NewTieredL1Retriever 创建 L1 检索器。
// db 必须是已迁移过的 GORM 实例（调用过 dao.Migrate，含 TieredMemoryModel）。
func NewTieredL1Retriever(db *gorm.DB) *TieredL1Retriever {
	return &TieredL1Retriever{db: db}
}

// Recent 返回指定 scope 最近的 N 条 L1 记忆（按时间倒序）。
func (r *TieredL1Retriever) Recent(_ context.Context, scope memory.Scope, limit int) ([]memory.Entry, error) {
	if limit <= 0 {
		limit = 200
	}
	var models []dao.TieredMemoryModel
	err := r.db.
		Where("tier = ? AND scope_kind = ? AND scope_id = ?", 1, string(scope.Kind), scope.ID).
		Order("created_at DESC").
		Limit(limit).
		Find(&models).Error
	if err != nil {
		return nil, errs.Wrap(err, "tiered_l1_retriever: recent failed")
	}
	return tieredModelsToEntries(models), nil
}

// Retrieve 按查询条件检索 L1 记忆（scope / category / 文本 过滤）。
func (r *TieredL1Retriever) Retrieve(_ context.Context, query memory.Query) ([]memory.Entry, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 200
	}
	tx := r.db.Model(&dao.TieredMemoryModel{}).Where("tier = ?", 1)
	if len(query.Scopes) > 0 {
		scopeConditions := make([][]interface{}, 0, len(query.Scopes))
		for _, scope := range query.Scopes {
			scopeConditions = append(scopeConditions, []interface{}{string(scope.Kind), scope.ID})
		}
		tx = tx.Where("(scope_kind, scope_id) IN ?", scopeConditions)
	}
	if query.Category != "" {
		tx = tx.Where("category = ?", query.Category)
	}
	if query.Text != "" {
		tx = tx.Where("content LIKE ?", "%"+query.Text+"%")
	}
	var models []dao.TieredMemoryModel
	if err := tx.Order("created_at DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "tiered_l1_retriever: retrieve failed")
	}
	return tieredModelsToEntries(models), nil
}

// Count 返回指定 scope 的 L1 记忆总数。
func (r *TieredL1Retriever) Count(_ context.Context, scope memory.Scope) (int, error) {
	var count int64
	err := r.db.Model(&dao.TieredMemoryModel{}).
		Where("tier = ? AND scope_kind = ? AND scope_id = ?", 1, string(scope.Kind), scope.ID).
		Count(&count).Error
	if err != nil {
		return 0, errs.Wrap(err, "tiered_l1_retriever: count failed")
	}
	return int(count), nil
}

// tieredModelsToEntries 将持久化模型映射为领域 Entry（含 scope 与 metadata）。
func tieredModelsToEntries(models []dao.TieredMemoryModel) []memory.Entry {
	out := make([]memory.Entry, 0, len(models))
	for i := range models {
		m := &models[i]
		var meta map[string]any
		if m.MetadataJSON != "" {
			_ = json.Unmarshal([]byte(m.MetadataJSON), &meta)
		}
		out = append(out, memory.Entry{
			ID:             m.ID,
			Scope:          memory.Scope{Kind: memory.ScopeKind(m.ScopeKind), ID: m.ScopeID},
			Content:        m.Content,
			Category:       m.Category,
			Source:         m.Source,
			Importance:     m.Importance,
			Metadata:       meta,
			CreatedAt:      m.CreatedAt,
			LastAccessedAt: m.LastAccessedAt,
		})
	}
	return out
}

// ============================================================================
// MergedRetriever — 合并多个 memory.Retriever 的检索结果
// ============================================================================
//
// 当前用于把 memory_entries（原始笔记，memRepo）与 tiered_memories L1（蒸馏知识）
// 合并召回。合并策略：
//   - 各源按传入顺序检索，先加入的源优先保留（故 L1 源应排在最前，确保其蒸馏知识
//     在 Snapshot 字符预算截断时不被丢弃）；
//   - 按内容去重，避免两源对同一事实重复计权。
//
// 单源检索失败属非致命，跳过该源不影响其他源（与 recall stage 的容错语义一致）。

var _ memory.Retriever = (*MergedRetriever)(nil)

type MergedRetriever struct {
	sources []memory.Retriever
}

// NewMergedRetriever 创建合并检索器。sources 的顺序即优先级：
// 排在前面的源在内容去重时优先保留（推荐把高价值 / 蒸馏知识的源放在最前）。
func NewMergedRetriever(sources ...memory.Retriever) *MergedRetriever {
	return &MergedRetriever{sources: sources}
}

// Recent 合并各源的最近 N 条记忆，按源顺序保留、内容去重。
func (m *MergedRetriever) Recent(ctx context.Context, scope memory.Scope, limit int) ([]memory.Entry, error) {
	seen := make(map[string]bool)
	out := make([]memory.Entry, 0, len(m.sources)*limit)
	for _, src := range m.sources {
		entries, err := src.Recent(ctx, scope, limit)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if seen[e.Content] {
				continue
			}
			seen[e.Content] = true
			out = append(out, e)
		}
	}
	return out, nil
}

// Retrieve 合并各源的检索结果，按源顺序保留、内容去重。
func (m *MergedRetriever) Retrieve(ctx context.Context, query memory.Query) ([]memory.Entry, error) {
	seen := make(map[string]bool)
	out := make([]memory.Entry, 0, 64)
	for _, src := range m.sources {
		entries, err := src.Retrieve(ctx, query)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if seen[e.Content] {
				continue
			}
			seen[e.Content] = true
			out = append(out, e)
		}
	}
	return out, nil
}

// Count 返回各源计数之和（单源失败按 0 计）。
func (m *MergedRetriever) Count(ctx context.Context, scope memory.Scope) (int, error) {
	total := 0
	for _, src := range m.sources {
		c, err := src.Count(ctx, scope)
		if err != nil {
			continue
		}
		total += c
	}
	return total, nil
}
