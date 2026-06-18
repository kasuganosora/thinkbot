package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// SQLiteRepository — memory.Repository 的 SQLite 实现
// ============================================================================

// SQLiteRepositoryConfig 配置 SQLite 记忆仓储。
type SQLiteRepositoryConfig struct {
	// MaxEntriesPerScope 每个 scope 的最大记忆条目数（默认 1000）。
	// 超过时按最旧淘汰。
	MaxEntriesPerScope int
	// DefaultLimit 检索时的默认返回条数（默认 10）。
	DefaultLimit int
}

// DefaultSQLiteRepositoryConfig 返回默认配置。
func DefaultSQLiteRepositoryConfig() SQLiteRepositoryConfig {
	return SQLiteRepositoryConfig{
		MaxEntriesPerScope: 1000,
		DefaultLimit:       10,
	}
}

// SQLiteRepository 使用 GORM/SQLite 持久化记忆条目。
// 实现 memory.Repository 接口（Store + Retriever）。
//
// 线程安全：GORM 自身是线程安全的（使用连接池）。
type SQLiteRepository struct {
	db     *gorm.DB
	config SQLiteRepositoryConfig

	// metrics
	entriesAppended atomic.Int64
	entriesDeleted  atomic.Int64
	retrievals      atomic.Int64
}

// NewSQLiteRepository 创建 SQLite 记忆仓储。
// db 必须是已迁移过的 GORM 实例（调用过 Migrate）。
func NewSQLiteRepository(db *gorm.DB, opts ...SQLiteRepositoryConfig) *SQLiteRepository {
	cfg := DefaultSQLiteRepositoryConfig()
	if len(opts) > 0 {
		if opts[0].MaxEntriesPerScope > 0 {
			cfg.MaxEntriesPerScope = opts[0].MaxEntriesPerScope
		}
		if opts[0].DefaultLimit > 0 {
			cfg.DefaultLimit = opts[0].DefaultLimit
		}
	}
	return &SQLiteRepository{
		db:     db,
		config: cfg,
	}
}

// ============================================================================
// Store 实现（写入侧）
// ============================================================================

// Append 追加一条记忆到指定 scope。
func (r *SQLiteRepository) Append(ctx context.Context, entry memory.Entry) error {
	// 自动填充默认值
	if entry.ID == "" {
		entry.ID = idgen.New("mem")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.LastAccessedAt.IsZero() {
		entry.LastAccessedAt = entry.CreatedAt
	}

	model := entryToModel(entry)

	if err := r.db.WithContext(ctx).Create(&model).Error; err != nil {
		return fmt.Errorf("sqlite_repository: append failed: %w", err)
	}

	r.entriesAppended.Add(1)

	// 容量限制：异步检查并淘汰最旧条目
	go r.evictIfNeeded(entry.Scope)

	return nil
}

// Delete 按 ID 删除指定 scope 下的一条记忆。
func (r *SQLiteRepository) Delete(ctx context.Context, scope memory.Scope, entryID string) error {
	result := r.db.WithContext(ctx).
		Where("id = ? AND scope_kind = ? AND scope_id = ?", entryID, string(scope.Kind), scope.ID).
		Delete(&EntryModel{})

	if result.Error != nil {
		return fmt.Errorf("sqlite_repository: delete failed: %w", result.Error)
	}
	if result.RowsAffected > 0 {
		r.entriesDeleted.Add(result.RowsAffected)
	}
	return nil
}

// Clear 清空指定 scope 的所有记忆。
func (r *SQLiteRepository) Clear(ctx context.Context, scope memory.Scope) error {
	result := r.db.WithContext(ctx).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Delete(&EntryModel{})

	if result.Error != nil {
		return fmt.Errorf("sqlite_repository: clear failed: %w", result.Error)
	}
	if result.RowsAffected > 0 {
		r.entriesDeleted.Add(result.RowsAffected)
	}
	return nil
}

// ============================================================================
// Retriever 实现（查询侧）
// ============================================================================

// Retrieve 根据查询条件检索记忆。
func (r *SQLiteRepository) Retrieve(ctx context.Context, query memory.Query) ([]memory.Entry, error) {
	r.retrievals.Add(1)

	limit := query.Limit
	if limit <= 0 {
		limit = r.config.DefaultLimit
	}

	tx := r.db.WithContext(ctx).Model(&EntryModel{})

	// Scope 过滤
	if len(query.Scopes) > 0 {
		scopeConditions := make([][]interface{}, 0, len(query.Scopes))
		for _, scope := range query.Scopes {
			scopeConditions = append(scopeConditions, []interface{}{string(scope.Kind), scope.ID})
		}
		tx = tx.Where("(scope_kind, scope_id) IN ?", scopeConditions)
	}

	// Category 过滤
	if query.Category != "" {
		tx = tx.Where("category = ?", query.Category)
	}

	// Importance 过滤
	if query.MinImportance > 0 {
		tx = tx.Where("importance >= ?", query.MinImportance)
	}

	// 文本关键词匹配
	if query.Text != "" {
		tx = tx.Where("content LIKE ?", "%"+query.Text+"%")
	}

	// 按时间倒序 + limit
	var models []EntryModel
	if err := tx.Order("created_at DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, fmt.Errorf("sqlite_repository: retrieve failed: %w", err)
	}

	// 更新 LastAccessedAt（批量）
	if len(models) > 0 {
		ids := make([]string, len(models))
		for i, m := range models {
			ids[i] = m.ID
		}
		go func() {
			r.db.Model(&EntryModel{}).Where("id IN ?", ids).
				Update("last_accessed_at", time.Now())
		}()
	}

	return modelsToEntries(models), nil
}

// Recent 获取指定 scope 的最近 N 条记忆。
func (r *SQLiteRepository) Recent(ctx context.Context, scope memory.Scope, limit int) ([]memory.Entry, error) {
	r.retrievals.Add(1)

	if limit <= 0 {
		limit = r.config.DefaultLimit
	}

	var models []EntryModel
	err := r.db.WithContext(ctx).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Order("created_at DESC").
		Limit(limit).
		Find(&models).Error

	if err != nil {
		return nil, fmt.Errorf("sqlite_repository: recent failed: %w", err)
	}

	return modelsToEntries(models), nil
}

// Count 返回指定 scope 的记忆总数。
func (r *SQLiteRepository) Count(ctx context.Context, scope memory.Scope) (int, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&EntryModel{}).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Count(&count).Error

	if err != nil {
		return 0, fmt.Errorf("sqlite_repository: count failed: %w", err)
	}
	return int(count), nil
}

// ============================================================================
// Metrics
// ============================================================================

// Metrics 返回当前指标快照。
func (r *SQLiteRepository) Metrics() memory.RepositoryMetrics {
	var totalEntries int64
	r.db.Model(&EntryModel{}).Count(&totalEntries)

	var totalScopes int64
	r.db.Model(&EntryModel{}).Distinct("scope_kind", "scope_id").Count(&totalScopes)

	return memory.RepositoryMetrics{
		TotalScopes:     int(totalScopes),
		TotalEntries:    int(totalEntries),
		EntriesAppended: r.entriesAppended.Load(),
		EntriesDeleted:  r.entriesDeleted.Load(),
		Retrievals:      r.retrievals.Load(),
	}
}

// ============================================================================
// eviction
// ============================================================================

// evictIfNeeded 检查 scope 是否超出容量，超出时淘汰最旧条目。
func (r *SQLiteRepository) evictIfNeeded(scope memory.Scope) {
	var count int64
	r.db.Model(&EntryModel{}).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Count(&count)

	if int(count) <= r.config.MaxEntriesPerScope {
		return
	}

	// 计算需要淘汰的数量
	excess := int(count) - r.config.MaxEntriesPerScope

	// 找出最旧的 N 条 ID
	var oldIDs []string
	r.db.Model(&EntryModel{}).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Order("created_at ASC").
		Limit(excess).
		Pluck("id", &oldIDs)

	if len(oldIDs) > 0 {
		result := r.db.Where("id IN ?", oldIDs).Delete(&EntryModel{})
		if result.RowsAffected > 0 {
			r.entriesDeleted.Add(result.RowsAffected)
		}
	}
}

// ============================================================================
// WindowState — Window 持久化扩展
// ============================================================================

// WindowStateStore 提供 Window 状态的持久化能力。
// 按 scope key 存储和加载。
type WindowStateStore struct {
	db *gorm.DB
}

// NewWindowStateStore 创建窗口状态存储。
func NewWindowStateStore(db *gorm.DB) *WindowStateStore {
	return &WindowStateStore{db: db}
}

// WindowSnapshot 表示 Window 的可持久化快照。
type WindowSnapshot struct {
	ScopeKey          string
	UsedTokens        int
	RoundCount        int
	TotalInputTokens  int64
	TotalOutputTokens int64
	Compressions      int64
}

// Save 保存窗口快照（upsert 语义）。
func (s *WindowStateStore) Save(ctx context.Context, snap WindowSnapshot) error {
	model := WindowStateModel{
		ScopeKey:          snap.ScopeKey,
		UsedTokens:        snap.UsedTokens,
		RoundCount:        snap.RoundCount,
		TotalInputTokens:  snap.TotalInputTokens,
		TotalOutputTokens: snap.TotalOutputTokens,
		Compressions:      snap.Compressions,
		UpdatedAt:         time.Now(),
	}

	err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "scope_key"}},
			DoUpdates: clause.AssignmentColumns([]string{"used_tokens", "round_count", "total_input_tokens", "total_output_tokens", "compressions", "updated_at"}),
		}).
		Create(&model).Error

	if err != nil {
		return fmt.Errorf("window_state_store: save failed: %w", err)
	}
	return nil
}

// Load 加载窗口快照。不存在时返回 nil, nil。
func (s *WindowStateStore) Load(ctx context.Context, scopeKey string) (*WindowSnapshot, error) {
	var model WindowStateModel
	err := s.db.WithContext(ctx).Where("scope_key = ?", scopeKey).First(&model).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("window_state_store: load failed: %w", err)
	}
	return &WindowSnapshot{
		ScopeKey:          model.ScopeKey,
		UsedTokens:        model.UsedTokens,
		RoundCount:        model.RoundCount,
		TotalInputTokens:  model.TotalInputTokens,
		TotalOutputTokens: model.TotalOutputTokens,
		Compressions:      model.Compressions,
	}, nil
}

// Delete 删除指定 scope 的窗口快照。
func (s *WindowStateStore) Delete(ctx context.Context, scopeKey string) error {
	err := s.db.WithContext(ctx).Where("scope_key = ?", scopeKey).Delete(&WindowStateModel{}).Error
	if err != nil {
		return fmt.Errorf("window_state_store: delete failed: %w", err)
	}
	return nil
}

// ============================================================================
// Conversion helpers
// ============================================================================

func entryToModel(entry memory.Entry) EntryModel {
	metadataJSON := ""
	if entry.Metadata != nil {
		if b, err := json.Marshal(entry.Metadata); err == nil {
			metadataJSON = string(b)
		}
	}

	return EntryModel{
		ID:             entry.ID,
		ScopeKind:      string(entry.Scope.Kind),
		ScopeID:        entry.Scope.ID,
		Content:        entry.Content,
		Category:       entry.Category,
		Source:         entry.Source,
		Importance:     entry.Importance,
		MetadataJSON:   metadataJSON,
		CreatedAt:      entry.CreatedAt,
		LastAccessedAt: entry.LastAccessedAt,
	}
}

func modelToEntry(m EntryModel) memory.Entry {
	var metadata map[string]any
	if m.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(m.MetadataJSON), &metadata)
	}

	return memory.Entry{
		ID: m.ID,
		Scope: memory.Scope{
			Kind: memory.ScopeKind(m.ScopeKind),
			ID:   m.ScopeID,
		},
		Content:        m.Content,
		Category:       m.Category,
		Source:         m.Source,
		Importance:     m.Importance,
		Metadata:       metadata,
		CreatedAt:      m.CreatedAt,
		LastAccessedAt: m.LastAccessedAt,
	}
}

func modelsToEntries(models []EntryModel) []memory.Entry {
	entries := make([]memory.Entry, len(models))
	for i, m := range models {
		entries[i] = modelToEntry(m)
	}
	return entries
}
