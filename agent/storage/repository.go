package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// compactCooldown 同一 scope 两次压缩之间的最小间隔，避免 LLM 无可合并项时
// 每轮写入都触发一次无意义的 LLM 调用。
const compactCooldown = 5 * time.Minute

// compactionLLMTimeout 是单次压缩（LLM 聚类合并）的上下文截止时间。
//
// 背景：压缩是写入路径上的 best-effort 后台任务，调用 main Provider（GLM/智谱
// 等首字节较慢的模型）。旧值 30s 过短——BigModel 高负载时首 token 常 >30s，
// 压缩 LLM 调用被 context deadline 精准卡死，导致 L1 记忆永远合并不了、只增不减
// （实测一整晚 136 次 "sqlite_compactor: LLM cluster+merge failed"，且随时间恶化）。
//
// 该上限只需比「正常慢」宽裕、同时仍能兜底「真卡死」：main Provider 的 HTTP 客户端
// 超时为 20min，这里取 5min 既给慢但活着的 BigModel 足够完成窗口，又不会让一个
// 卡死的压缩 goroutine 无限期占用该 scope 的 compacting 锁。
const compactionLLMTimeout = 5 * time.Minute

// MemoryCompactor 在 scope 字符数超过预算阈值时触发记忆压缩（压缩后入库）。
// SQLiteCompactor 实现该接口；生产路径通过 SQLiteRepository 的 Compactor 字段注入。
type MemoryCompactor interface {
	CompactScope(ctx context.Context, scope memory.Scope) error
}

// ============================================================================
// SQLiteRepository — memory.Repository 的 SQLite 实现
// ============================================================================

// 编译期契约：生产路径的记忆仓储必须同时满足 Repository 与 Replacer。
// Replacer 一旦丢失，记忆工具会静默降级到非原子的替换路径，务必让此处先编译失败。
var (
	_ memory.Repository = (*SQLiteRepository)(nil)
	_ memory.Replacer   = (*SQLiteRepository)(nil)
)

// SQLiteRepositoryConfig 配置 SQLite 记忆仓储。
type SQLiteRepositoryConfig struct {
	// MaxEntriesPerScope 每个 scope 的最大记忆条目数（默认 1000）。
	// 超过时按最旧淘汰。
	MaxEntriesPerScope int
	// DefaultLimit 检索时的默认返回条数（默认 10）。
	DefaultLimit int
	// Window 可选动态窗口，用于按模型上下文派生各 scope 的字符预算。
	// 注入后记忆块字符上限由 Window.MemoryBudget()*3 派生（与 snapshot 口径一致），
	// 并在超过预算时触发 Compactor 压缩后入库。nil 表示不限制、不压缩。
	Window *memory.Window
	// Compactor 可选记忆压缩器；当某 scope 字符数超过预算阈值时异步触发，
	// 将相似条目语义合并、归档来源（压缩后入库），取代简单的截断。
	Compactor MemoryCompactor
	// CompressThreshold 触发压缩的预算占用比例（默认 0.85）。
	CompressThreshold float64
}

// DefaultSQLiteRepositoryConfig 返回默认配置。
func DefaultSQLiteRepositoryConfig() SQLiteRepositoryConfig {
	return SQLiteRepositoryConfig{
		MaxEntriesPerScope: 1000,
		DefaultLimit:       10,
		CompressThreshold:  0.85,
	}
}

// SQLiteRepository 使用 GORM/SQLite 持久化记忆条目。
// 实现 memory.Repository 接口（Store + Retriever）。
//
// 线程安全：GORM 自身是线程安全的（使用连接池）。
type SQLiteRepository struct {
	db     *gorm.DB
	config SQLiteRepositoryConfig

	// window 可选动态窗口，用于推导各 scope 字符预算
	window *memory.Window
	// compactor 可选压缩器，超出预算时异步触发
	compactor MemoryCompactor

	// 压缩并发/冷却控制（零值即可用）
	compacting  sync.Map // scope.Key() -> true（压缩进行中）
	lastCompact sync.Map // scope.Key() -> time.Time（上次压缩时间，用于冷却）

	// metrics
	entriesAppended atomic.Int64
	entriesDeleted  atomic.Int64
	retrievals      atomic.Int64
}

// NewSQLiteRepository 创建 SQLite 记忆仓储。
// db 必须是已迁移过的 GORM 实例（调用过 dao.Migrate）。
func NewSQLiteRepository(db *gorm.DB, opts ...SQLiteRepositoryConfig) *SQLiteRepository {
	cfg := DefaultSQLiteRepositoryConfig()
	if len(opts) > 0 {
		o := opts[0]
		if o.MaxEntriesPerScope > 0 {
			cfg.MaxEntriesPerScope = o.MaxEntriesPerScope
		}
		if o.DefaultLimit > 0 {
			cfg.DefaultLimit = o.DefaultLimit
		}
		if o.Window != nil {
			cfg.Window = o.Window
		}
		if o.Compactor != nil {
			cfg.Compactor = o.Compactor
		}
		if o.CompressThreshold > 0 && o.CompressThreshold <= 1.0 {
			cfg.CompressThreshold = o.CompressThreshold
		}
	}
	return &SQLiteRepository{
		db:        db,
		config:    cfg,
		window:    cfg.Window,
		compactor: cfg.Compactor,
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
		return errs.Wrap(err, "sqlite_repository: append failed")
	}

	r.entriesAppended.Add(1)

	// 容量限制（按条数）：异步检查并淘汰最旧条目
	go func() {
		evictCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.evictIfNeeded(evictCtx, entry.Scope)
	}()

	// 字符预算（按 window 派生）：超出阈值时异步触发语义压缩（压缩后入库），
	// 取代简单的截断。渲染层仍保留截断作为兜底，但正常情况下存储已自行收敛。
	if r.compactor != nil && r.window != nil {
		scope := entry.Scope
		go func() {
			cmpCtx, cancel := context.WithTimeout(context.Background(), compactionLLMTimeout)
			defer cancel()
			r.maybeCompact(cmpCtx, scope)
		}()
	}

	return nil
}

// Delete 按 ID 删除指定 scope 下的一条记忆。
func (r *SQLiteRepository) Delete(ctx context.Context, scope memory.Scope, entryID string) error {
	result := r.db.WithContext(ctx).
		Where("id = ? AND scope_kind = ? AND scope_id = ?", entryID, string(scope.Kind), scope.ID).
		Delete(&dao.EntryModel{})

	if result.Error != nil {
		return errs.Wrap(result.Error, "sqlite_repository: delete failed")
	}
	if result.RowsAffected > 0 {
		r.entriesDeleted.Add(result.RowsAffected)
	}
	return nil
}

// Replace 原子性地替换指定 scope 下的一条记忆（实现 memory.Replacer）。
//
// 「删除旧条目 + 写入新条目」在同一个事务内完成，因此允许 newEntry.ID 与
// deleteID 相同 —— 这正是记忆工具 replace / batch-update 的语义（就地改写内容、
// 保留原 ID 与 created_at）。deleteID 为空或指向不存在的条目时退化为纯追加。
//
// 本方法缺失时调用方会降级为「先 Append 再 Delete」，而复用同一 ID 的 Append
// 走的是 INSERT，必然撞上 memory_entries.id 唯一约束，使记忆更新永久失败。
func (r *SQLiteRepository) Replace(ctx context.Context, scope memory.Scope, deleteID string, newEntry memory.Entry) error {
	if newEntry.ID == "" {
		newEntry.ID = idgen.New("mem")
	}
	if newEntry.CreatedAt.IsZero() {
		newEntry.CreatedAt = time.Now()
	}
	if newEntry.LastAccessedAt.IsZero() {
		newEntry.LastAccessedAt = newEntry.CreatedAt
	}
	newEntry.Scope = scope

	model := entryToModel(newEntry)

	var deleted int64
	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if deleteID != "" {
			res := tx.Where("id = ? AND scope_kind = ? AND scope_id = ?",
				deleteID, string(scope.Kind), scope.ID).
				Delete(&dao.EntryModel{})
			if res.Error != nil {
				return res.Error
			}
			deleted = res.RowsAffected
		}
		return tx.Create(&model).Error
	}); err != nil {
		return errs.Wrap(err, "sqlite_repository: replace failed")
	}

	if deleted > 0 {
		r.entriesDeleted.Add(deleted)
	}
	r.entriesAppended.Add(1)

	// 与 Append 保持一致的收敛行为：替换后字符总量可能上涨（deleteID 不存在时
	// 条目数也会 +1），仍需异步做容量淘汰与预算压缩。
	go func() {
		evictCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.evictIfNeeded(evictCtx, scope)
	}()
	if r.compactor != nil && r.window != nil {
		go func() {
			cmpCtx, cancel := context.WithTimeout(context.Background(), compactionLLMTimeout)
			defer cancel()
			r.maybeCompact(cmpCtx, scope)
		}()
	}

	return nil
}

// Clear 清空指定 scope 的所有记忆。
func (r *SQLiteRepository) Clear(ctx context.Context, scope memory.Scope) error {
	result := r.db.WithContext(ctx).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Delete(&dao.EntryModel{})

	if result.Error != nil {
		return errs.Wrap(result.Error, "sqlite_repository: clear failed")
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

	tx := r.db.WithContext(ctx).Model(&dao.EntryModel{})

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
	var models []dao.EntryModel
	if err := tx.Order("created_at DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: retrieve failed")
	}

	// 更新 LastAccessedAt（批量）
	if len(models) > 0 {
		ids := make([]string, len(models))
		for i, m := range models {
			ids[i] = m.ID
		}
		go func() {
			updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r.db.WithContext(updateCtx).Model(&dao.EntryModel{}).Where("id IN ?", ids).
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

	var models []dao.EntryModel
	err := r.db.WithContext(ctx).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Order("created_at DESC").
		Limit(limit).
		Find(&models).Error

	if err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: recent failed")
	}

	return modelsToEntries(models), nil
}

// Count 返回指定 scope 的记忆总数。
func (r *SQLiteRepository) Count(ctx context.Context, scope memory.Scope) (int, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&dao.EntryModel{}).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Count(&count).Error

	if err != nil {
		return 0, errs.Wrap(err, "sqlite_repository: count failed")
	}
	return int(count), nil
}

// ============================================================================
// Metrics
// ============================================================================

// Metrics 返回当前指标快照。
func (r *SQLiteRepository) Metrics() memory.RepositoryMetrics {
	var totalEntries int64
	r.db.Model(&dao.EntryModel{}).Count(&totalEntries)

	var totalScopes int64
	r.db.Model(&dao.EntryModel{}).Distinct("scope_kind", "scope_id").Count(&totalScopes)

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
func (r *SQLiteRepository) evictIfNeeded(ctx context.Context, scope memory.Scope) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&dao.EntryModel{}).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Count(&count).Error; err != nil {
		return // DB 错误，跳过淘汰
	}

	if int(count) <= r.config.MaxEntriesPerScope {
		return
	}

	// 计算需要淘汰的数量
	excess := int(count) - r.config.MaxEntriesPerScope

	// 找出最旧的 N 条 ID
	var oldIDs []string
	if err := r.db.WithContext(ctx).Model(&dao.EntryModel{}).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Order("created_at ASC").
		Limit(excess).
		Pluck("id", &oldIDs).Error; err != nil {
		return
	}

	if len(oldIDs) > 0 {
		result := r.db.WithContext(ctx).Where("id IN ?", oldIDs).Delete(&dao.EntryModel{})
		if result.RowsAffected > 0 {
			r.entriesDeleted.Add(result.RowsAffected)
		}
	}
}

// ============================================================================
// auto-compaction — 到达字符预算时压缩后入库（取代截断）
// ============================================================================

// charBudget 返回指定 scope 的字符预算（按 window 派生）。
// 与 snapshot.renderBlock 的口径完全一致：budget = Window.MemoryBudget()*3，
// user 类 scope 保持原 2200/1375 比例。返回 0 表示不限制（未注入 window）。
func (r *SQLiteRepository) charBudget(scope memory.Scope) int {
	if r.window == nil {
		return 0
	}
	budget := r.window.MemoryBudget()
	if budget <= 0 {
		return 0
	}
	budgetChars := budget * 3
	if scope.Kind == memory.ScopeUser {
		// 保持原 2200/1375 的比例（≈0.625），让 user 块占 memory 块的一部分
		return budgetChars * 1375 / 2200
	}
	return budgetChars
}

// maybeCompact 在 scope 字符数超过预算阈值时触发异步压缩。
// 非阻塞调用方；内部已做并发重入保护与冷却，压缩失败不影响正常写入。
func (r *SQLiteRepository) maybeCompact(ctx context.Context, scope memory.Scope) {
	budget := r.charBudget(scope)
	if budget <= 0 {
		return
	}
	total, err := r.totalChars(ctx, scope)
	if err != nil {
		return
	}
	threshold := int(float64(budget) * r.config.CompressThreshold)
	if total <= threshold {
		return
	}

	key := scope.Key()

	// 冷却：避免 LLM 无可合并项时每轮都打 LLM
	if v, ok := r.lastCompact.Load(key); ok {
		if t, ok := v.(time.Time); ok && time.Since(t) < compactCooldown {
			return
		}
	}

	// 并发重入保护：同一 scope 压缩进行中则跳过
	if _, loaded := r.compacting.LoadOrStore(key, true); loaded {
		return
	}
	defer r.compacting.Delete(key)

	r.lastCompact.Store(key, time.Now())

	// 非致命：压缩失败不影响正常写入；渲染层仍有截断兜底
	_ = r.compactor.CompactScope(ctx, scope)
}

// totalChars 统计指定 scope 下所有「活跃（未归档）」记忆的字符总数。
// 用于判断是否需要触发压缩。归档条目（archived=true）不计入。
func (r *SQLiteRepository) totalChars(ctx context.Context, scope memory.Scope) (int, error) {
	var models []dao.EntryModel
	if err := r.db.WithContext(ctx).
		Where("scope_kind = ? AND scope_id = ? AND (metadata_json IS NULL OR metadata_json NOT LIKE ?)",
			string(scope.Kind), scope.ID, "%\"archived\":true%").
		Find(&models).Error; err != nil {
		return 0, errs.Wrap(err, "sqlite_repository: totalChars failed")
	}
	total := 0
	for _, m := range models {
		total += len([]rune(m.Content))
	}
	return total, nil
}

// GetAllActive 返回指定 scope 下所有未归档（活跃）的记忆条目，按创建时间升序。
// 供压缩器读取待合并的来源。
func (r *SQLiteRepository) GetAllActive(ctx context.Context, scope memory.Scope) ([]memory.Entry, error) {
	var models []dao.EntryModel
	if err := r.db.WithContext(ctx).
		Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).
		Order("created_at ASC").
		Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: get all active failed")
	}
	out := make([]memory.Entry, 0, len(models))
	for _, m := range models {
		e := modelToEntry(m)
		if isEntryArchived(e.Metadata) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ArchiveByID 将指定条目标记为已归档（保留可追溯，不物理删除）。
// 压缩后的来源条目通过此方法归档，避免重复记忆持续累积。
func (r *SQLiteRepository) ArchiveByID(ctx context.Context, scope memory.Scope, entryID string) bool {
	var model dao.EntryModel
	if err := r.db.WithContext(ctx).
		Where("id = ? AND scope_kind = ? AND scope_id = ?", entryID, string(scope.Kind), scope.ID).
		First(&model).Error; err != nil {
		return false
	}
	var meta map[string]any
	if model.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(model.MetadataJSON), &meta)
	}
	if meta == nil {
		meta = make(map[string]any)
	}
	if v, ok := meta["archived"].(bool); ok && v {
		return true // 已归档，幂等
	}
	meta["archived"] = true
	meta["archived_at"] = time.Now()
	meta["archived_by"] = "compactor"
	b, err := json.Marshal(meta)
	if err != nil {
		return false
	}
	if err := r.db.WithContext(ctx).Model(&dao.EntryModel{}).
		Where("id = ?", entryID).
		Update("metadata_json", string(b)).Error; err != nil {
		return false
	}
	return true
}

// isEntryArchived 检查元数据中是否有 archived=true 标记。
func isEntryArchived(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	archived, ok := meta["archived"].(bool)
	return ok && archived
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
	model := dao.WindowStateModel{
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
		return errs.Wrap(err, "window_state_store: save failed")
	}
	return nil
}

// Load 加载窗口快照。不存在时返回 nil, nil。
func (s *WindowStateStore) Load(ctx context.Context, scopeKey string) (*WindowSnapshot, error) {
	var model dao.WindowStateModel
	err := s.db.WithContext(ctx).Where("scope_key = ?", scopeKey).First(&model).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, errs.Wrap(err, "window_state_store: load failed")
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
	err := s.db.WithContext(ctx).Where("scope_key = ?", scopeKey).Delete(&dao.WindowStateModel{}).Error
	if err != nil {
		return errs.Wrap(err, "window_state_store: delete failed")
	}
	return nil
}

// ============================================================================
// Conversion helpers
// ============================================================================

func entryToModel(entry memory.Entry) dao.EntryModel {
	metadataJSON := ""
	if entry.Metadata != nil {
		if b, err := json.Marshal(entry.Metadata); err == nil {
			metadataJSON = string(b)
		}
	}

	return dao.EntryModel{
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

func modelToEntry(m dao.EntryModel) memory.Entry {
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

func modelsToEntries(models []dao.EntryModel) []memory.Entry {
	entries := make([]memory.Entry, len(models))
	for i, m := range models {
		entries[i] = modelToEntry(m)
	}
	return entries
}
