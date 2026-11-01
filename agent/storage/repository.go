package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/traceid"
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
	// BotID binds this repository to one bot. Empty is reserved for offline/app maintenance.
	BotID string
	// MaxEntriesPerScope 每个 scope 的最大记忆条目数（默认 1000）。
	// 超过时先删已归档条目，再压缩/淘汰最旧活跃条目；每日摘要不计数、不淘汰。
	// 对应配置键 memory.max_entries_per_scope。
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
	// DedupTriggerNew 某 scope 未经去重检查的新条目达到该数量即触发增量去重
	// （默认 DefaultDedupTriggerNew=8）；超出字符预算时只要有未检查条目也会触发。
	DedupTriggerNew int
	// DedupCooldown 同一 scope 两次自动去重的最小间隔（默认 5 分钟，持久化到
	// memory_compact_states，重启不重置）。
	DedupCooldown time.Duration
	// Logger 可选日志器（淘汰日志等）；nil 时静默。
	Logger *zap.SugaredLogger
}

// DefaultSQLiteRepositoryConfig 返回默认配置。
func DefaultSQLiteRepositoryConfig() SQLiteRepositoryConfig {
	return SQLiteRepositoryConfig{
		MaxEntriesPerScope: DefaultMaxEntriesPerScope,
		DefaultLimit:       10,
		CompressThreshold:  0.85,
		DedupTriggerNew:    DefaultDedupTriggerNew,
		DedupCooldown:      DefaultDedupCooldown,
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
	evicting    sync.Map // scope.Key() -> true（淘汰进行中）

	// budgetWarned 字符预算为 0 的告警只打一次。
	// 预算是 window 级配置，每个 scope 都告警会刷屏。
	budgetWarned atomic.Bool

	// metrics
	entriesAppended atomic.Int64
	entriesDeleted  atomic.Int64
	retrievals      atomic.Int64
	vec             *memory.VecIndex

	// touches 把 last_accessed_at 的刷新合并到固定间隔，避免每次 Retrieve
	// 都抢 SQLite 的写锁。
	touches accessToucher
}

// accessTouchInterval 是同一条记忆刷新 last_accessed_at 的最小间隔。
//
// 这一列只被存下来、没有查询按它排序或淘汰。每次 Retrieve 都 UPDATE
// 会占住 SQLite 唯一的写锁，memory_vec 的 MATCH、stats_usage_daily 和
// chat_messages 的写入一起变慢。十分钟的粒度对「最近访问」够用。
const accessTouchInterval = 10 * time.Minute

// accessToucher 记录每个 id 上次认领刷新的时间。claim 在启动写 goroutine
// 之前就把时间记上，并发的 Retrieve 会合成一次 UPDATE。
type accessToucher struct {
	mu       sync.Mutex
	last     map[string]time.Time
	interval time.Duration
}

// claim 返回这次应该写入的 id。interval 为零时用 accessTouchInterval。
func (t *accessToucher) claim(ids []string, now time.Time) []string {
	if t == nil || len(ids) == 0 {
		return nil
	}
	interval := t.interval
	if interval == 0 {
		interval = accessTouchInterval
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = make(map[string]time.Time, len(ids))
	}
	if len(t.last) > 8192 {
		for id, ts := range t.last {
			if now.Sub(ts) >= interval {
				delete(t.last, id)
			}
		}
	}
	due := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if prev, ok := t.last[id]; ok && now.Sub(prev) < interval {
			continue
		}
		t.last[id] = now
		due = append(due, id)
	}
	return due
}

// NewSQLiteRepository 创建 SQLite 记忆仓储。
// db 必须是已迁移过的 GORM 实例（调用过 dao.Migrate）。
func NewSQLiteRepository(db *gorm.DB, opts ...SQLiteRepositoryConfig) *SQLiteRepository {
	cfg := DefaultSQLiteRepositoryConfig()
	if len(opts) > 0 {
		o := opts[0]
		cfg.BotID = o.BotID
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
		if o.DedupTriggerNew > 0 {
			cfg.DedupTriggerNew = o.DedupTriggerNew
		}
		if o.DedupCooldown > 0 {
			cfg.DedupCooldown = o.DedupCooldown
		}
		if o.Logger != nil {
			cfg.Logger = o.Logger
		}
	}
	return &SQLiteRepository{
		db:        db,
		config:    cfg,
		window:    cfg.Window,
		compactor: cfg.Compactor,
		vec:       memory.OpenVecIndex(db),
	}
}

var vecBackfillDone sync.Map // *gorm.DB -> struct{}

// StartAppVecBackfill runs both startup vector backfills (tiered_memories,
// then memory_entries) in the background at app init, independent of whether
// any bot is running: per-bot startup only happens for running bots, so with
// stopped bots an emptied memory_vec would stay empty. Once per DB per
// process; a later per-bot StartVecBackfill on the same DB is a no-op, and the
// per-bot TieredStore backfill then only finds already-present rows.
func StartAppVecBackfill(gdb *gorm.DB, logger *zap.SugaredLogger) {
	if gdb == nil {
		return
	}
	idx := memory.OpenVecIndex(gdb)
	if !idx.Enabled() {
		return
	}
	if _, loaded := vecBackfillDone.LoadOrStore(gdb, struct{}{}); loaded {
		return
	}
	go func() { _, _ = runAppVecBackfill(context.Background(), gdb, idx, logger) }()
}

// runAppVecBackfill is the synchronous body of StartAppVecBackfill.
func runAppVecBackfill(ctx context.Context, gdb *gorm.DB, idx *memory.VecIndex, logger *zap.SugaredLogger) ([2]memory.VecBackfillStats, error) {
	var out [2]memory.VecBackfillStats
	var err1, err2 error
	out[0], err1 = memory.BackfillTieredVectors(ctx, gdb, idx, logger)
	r := &SQLiteRepository{db: gdb, vec: idx}
	out[1], err2 = r.backfillVec(ctx, logger)
	if err1 != nil {
		return out, err1
	}
	return out, err2
}

// StartVecBackfill indexes active memory_entries that have no vector yet, once
// per DB per process, in the background. The TieredStore backfill only covers
// tiered_memories; without this, memory_entries vectors cleared by an embed
// scheme change (or never written) would stay missing until rewritten.
func (r *SQLiteRepository) StartVecBackfill(logger *zap.SugaredLogger) {
	if r == nil || r.db == nil || !r.vec.Enabled() {
		return
	}
	var key any = r.db
	if r.config.BotID != "" {
		// App-wide backfill already covers every owner; otherwise each bot must
		// backfill its own rows rather than the first bot suppressing all others.
		if _, done := vecBackfillDone.Load(r.db); done {
			return
		}
		key = struct {
			db    *gorm.DB
			botID string
		}{r.db, r.config.BotID}
	}
	if _, loaded := vecBackfillDone.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	go func() {
		// start / done / error lines: memory_vec_backfill_* table=memory_entries
		_, _ = r.backfillVec(context.Background(), logger)
	}()
}

// BackfillVec synchronously indexes active memory_entries missing a vector.
// Returns (already present, written).
func (r *SQLiteRepository) BackfillVec(ctx context.Context) (int, int, error) {
	st, err := r.backfillVec(ctx, nil)
	return st.Present, st.Written, err
}

func (r *SQLiteRepository) backfillVec(ctx context.Context, logger *zap.SugaredLogger) (memory.VecBackfillStats, error) {
	if r == nil || !r.vec.Enabled() {
		return memory.VecBackfillStats{}, nil
	}
	var models []dao.EntryModel
	if err := r.owned(r.db.WithContext(ctx)).Where(activeWhere, activeArgs...).Find(&models).Error; err != nil {
		if logger != nil {
			logger.Errorw(memory.VecBackfillErrorEvent, "table", "memory_entries", "stage", "load", "err", err)
		}
		return memory.VecBackfillStats{}, err
	}
	items := make([]memory.VecItem, 0, len(models))
	for _, e := range modelsToActiveEntries(models) {
		items = append(items, memory.VecItem{ScopeKey: memory.VectorScopeKey(e.Scope, e.Metadata), EntryID: e.ID, Content: e.Content})
	}
	return r.vec.BackfillMissing(ctx, "memory_entries", items, logger)
}

// CompactScope 对该 scope 执行语义压缩（委派给内部 MemoryCompactor，即带 pre-LLM
// 预压缩的 SQLiteCompactor）。供 /compact 等命令按需手动触发记忆压缩。
// compactor 未配置时为空操作（返回 nil），不影响调用方。
func (r *SQLiteRepository) Vec() *memory.VecIndex {
	if r == nil {
		return nil
	}
	return r.vec
}

// CompactScope 对该 scope 执行语义压缩（委派给内部 MemoryCompactor，即带 pre-LLM
// 预压缩的 SQLiteCompactor）。供 /compact 等命令按需手动触发记忆压缩。
// compactor 未配置时为空操作（返回 nil），不影响调用方。
func (r *SQLiteRepository) CompactScope(ctx context.Context, scope memory.Scope) error {
	if r.compactor == nil {
		return nil
	}
	return r.compactor.CompactScope(ctx, scope)
}

// ============================================================================
// Store 实现（写入侧）
// ============================================================================

// Append 追加一条记忆到指定 scope。
func (r *SQLiteRepository) Append(ctx context.Context, entry memory.Entry) error {
	var err error
	entry, err = r.ownedEntry(entry)
	if err != nil {
		return err
	}
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
	if r.vec.Enabled() {
		r.vec.Upsert(memory.VectorScopeKey(entry.Scope, entry.Metadata), entry.ID, entry.Content)
	}

	// 容量限制（按条数）：异步检查并淘汰最旧条目
	go func() {
		evictCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.evictIfNeeded(evictCtx, entry.Scope)
	}()

	// 字符预算（按 window 派生）：超出阈值时异步触发语义压缩（压缩后入库），
	// 取代简单的截断。渲染层仍保留截断作为兜底，但正常情况下存储已自行收敛。
	if r.compactor != nil && !precheckedForDedup(entry) {
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
	var existing dao.EntryModel
	_ = r.exactScope(r.db.WithContext(ctx), scope).Where("id = ?", entryID).
		First(&existing).Error
	if existing.Content != "" {
		tombstoneScope := scope
		if r.config.BotID != "" {
			tombstoneScope = memory.BotScope(r.config.BotID)
		}
		memory.RecordExternalTombstone(r.db, tombstoneScope, existing.Content, existing.Source)
	}
	result := r.exactScope(r.db.WithContext(ctx), scope).Where("id = ?", entryID).
		Delete(&dao.EntryModel{})

	if result.Error != nil {
		return errs.Wrap(result.Error, "sqlite_repository: delete failed")
	}
	if result.RowsAffected > 0 {
		r.entriesDeleted.Add(result.RowsAffected)
		if r.vec.Enabled() {
			r.vec.Delete(entryID)
		}
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
	var err error
	newEntry, err = r.ownedEntry(newEntry)
	if err != nil {
		return err
	}

	model := entryToModel(newEntry)

	var deleted int64
	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if deleteID != "" {
			res := r.exactScope(tx, scope).Where("id = ?", deleteID).Delete(&dao.EntryModel{})
			if res.Error != nil {
				return res.Error
			}
			deleted = res.RowsAffected
			if r.config.BotID != "" && deleted == 0 {
				return errs.New("sqlite_repository: replacement source not found in this bot and scope")
			}
		}
		return tx.Create(&model).Error
	}); err != nil {
		return errs.Wrap(err, "sqlite_repository: replace failed")
	}

	if deleted > 0 {
		r.entriesDeleted.Add(deleted)
	}
	r.entriesAppended.Add(1)
	if r.vec.Enabled() {
		if deleted > 0 && deleteID != "" && deleteID != newEntry.ID {
			r.vec.Delete(deleteID)
		}
		r.vec.Upsert(memory.VectorScopeKey(newEntry.Scope, newEntry.Metadata), newEntry.ID, newEntry.Content)
	}

	// 与 Append 保持一致的收敛行为：替换后字符总量可能上涨（deleteID 不存在时
	// 条目数也会 +1），仍需异步做容量淘汰与预算压缩。
	go func() {
		evictCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.evictIfNeeded(evictCtx, scope)
	}()
	if r.compactor != nil && !precheckedForDedup(newEntry) {
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
	result := r.exactScope(r.db.WithContext(ctx), scope).
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

	tx := r.owned(r.db.WithContext(ctx)).Model(&dao.EntryModel{})

	// Scope 过滤
	if len(query.Scopes) > 0 {
		tx = r.forScopes(tx, query.Scopes)
	}

	// Category 过滤
	if query.Category != "" {
		tx = tx.Where("category = ?", query.Category)
	}

	// Importance 过滤
	if query.MinImportance > 0 {
		tx = tx.Where("importance >= ?", query.MinImportance)
	}

	// 时间范围过滤（闭区间）
	if !query.Since.IsZero() {
		tx = tx.Where("created_at >= ?", query.Since)
	}
	if !query.Until.IsZero() {
		tx = tx.Where("created_at <= ?", query.Until)
	}

	// 文本：sqlite-vec 可用时按 scope 近邻取回，失败或未启用则子串。
	if query.Text != "" && !query.Lexical && r.vec.Enabled() && (len(query.Scopes) > 0 || r.config.BotID != "") {
		ids := make([]string, 0, limit)
		vectorScopes := query.Scopes
		if r.config.BotID != "" {
			// The vector copy is bot-wide; SQL above still enforces the requested
			// original scopes, owner, category, importance and time bounds.
			vectorScopes = []memory.Scope{memory.BotScope(r.config.BotID)}
		}
		for _, scope := range vectorScopes {
			for _, hit := range r.vec.Search(scope.Key(), query.Text, limit) {
				ids = append(ids, hit.EntryID)
			}
		}
		if len(ids) > 0 {
			var models []dao.EntryModel
			if err := tx.Session(&gorm.Session{}).Where("id IN ?", ids).Limit(limit).Find(&models).Error; err == nil && len(models) > 0 {
				return modelsToEntries(models), nil
			}
		}
	}
	if query.Text != "" {
		if query.Lexical {
			tx = tx.Where("content LIKE ? ESCAPE '\\'", likeContains(query.Text))
		} else {
			tx = tx.Where("content LIKE ?", "%"+query.Text+"%")
		}
	}

	// 排序 + limit：默认倒序（最新在前），order=asc 时升序（最早在前）。
	//
	// 升序是「最早的记忆是什么」这类问题的唯一正确路径：先按时间倒序再截断，
	// 无论 limit 多大都只能看到最近的一批，答案必然偏新。
	var models []dao.EntryModel
	order := "created_at DESC"
	if query.Order == memory.OrderAsc {
		order = "created_at ASC"
	}
	if err := tx.Order(order).Limit(limit).Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: retrieve failed")
	}

	// 更新 LastAccessedAt（批量）。同一 id 在 accessTouchInterval 内只写一次，
	// 认领发生在 goroutine 之外，重叠的 Retrieve 不会各打一笔 UPDATE。
	if len(models) > 0 {
		ids := make([]string, len(models))
		for i, m := range models {
			ids[i] = m.ID
		}
		due := r.touches.claim(ids, time.Now())
		if len(due) > 0 {
			go func() {
				updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				r.owned(r.db.WithContext(updateCtx)).Model(&dao.EntryModel{}).Where("id IN ?", due).
					Update("last_accessed_at", time.Now())
			}()
		}
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
	err := r.forScope(r.db.WithContext(ctx), scope).
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
	err := r.forScope(r.db.WithContext(ctx).Model(&dao.EntryModel{}), scope).
		Count(&count).Error

	if err != nil {
		return 0, errs.Wrap(err, "sqlite_repository: count failed")
	}
	return int(count), nil
}

// MemoryStats 统计条目总数与时间跨度（实现 memory.MemoryStatsProvider）。
//
// 单次聚合查询（COUNT + MIN + MAX），避免把全表读进内存。
// scopes 为空表示统计全库 —— 「我最早的记忆是什么时候」这类问题必须跨 scope 统计，
// 否则在当前会话 scope 里算出的「最早」只是该频道的起点，不是 bot 的起点。
func (r *SQLiteRepository) MemoryStats(ctx context.Context, scopes []memory.Scope) (memory.MemoryStatsInfo, error) {
	tx := r.owned(r.db.WithContext(ctx)).Model(&dao.EntryModel{})
	if len(scopes) > 0 {
		tx = r.forScopes(tx, scopes)
	}

	// MIN/MAX 作用在 datetime(created_at) 上而非原始列：
	// 原始列是带时区偏移的文本（如 "2026-08-12 19:39:29.93974+08:00"），
	// 直接按文本比较在**混用不同时区偏移**的记录间会排错序；datetime() 先
	// 归一化成 UTC 文本再比较，跨时区也正确。
	//
	// 另：GORM 把聚合结果扫到 *time.Time 会报 unsupported Scan（驱动返回
	// string），因此先扫成 string 再自行解析。
	var row struct {
		Total  int64
		Oldest *string
		Newest *string
	}
	if err := tx.Select("COUNT(*) AS total, MIN(datetime(created_at)) AS oldest, MAX(datetime(created_at)) AS newest").
		Scan(&row).Error; err != nil {
		return memory.MemoryStatsInfo{}, errs.Wrap(err, "sqlite_repository: memory stats failed")
	}

	info := memory.MemoryStatsInfo{Total: int(row.Total)}
	if row.Oldest != nil {
		info.Oldest = parseSQLiteTime(*row.Oldest)
	}
	if row.Newest != nil {
		info.Newest = parseSQLiteTime(*row.Newest)
	}
	return info, nil
}

// parseSQLiteTime 解析 SQLite datetime() 产出的 UTC 文本，并转换到本地时区。
// 解析失败返回零值 —— 统计只是元信息，不该因为它让整次检索失败。
func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
	} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.Local()
		}
	}
	return time.Time{}
}

// scopeConditions 把 scope 列表转成 GORM 的多元组 IN 条件。
func scopeConditions(scopes []memory.Scope) [][]interface{} {
	cond := make([][]interface{}, 0, len(scopes))
	for _, scope := range scopes {
		cond = append(cond, []interface{}{string(scope.Kind), scope.ID})
	}
	return cond
}

// ============================================================================
// Metrics
// ============================================================================

// Metrics 返回当前指标快照。
func (r *SQLiteRepository) Metrics() memory.RepositoryMetrics {
	var totalEntries int64
	r.owned(r.db).Model(&dao.EntryModel{}).Count(&totalEntries)

	var totalScopes int64
	r.owned(r.db).Model(&dao.EntryModel{}).Distinct("scope_kind", "scope_id").Count(&totalScopes)

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

// evictWhereScope / evictArchived / evictDigest 是淘汰路径使用的 SQL 片段。
// 每日摘要（source=daily_digest）永不淘汰、也不计入上限；
// 已归档条目（已摘要/已压缩）优先淘汰；bot scope 中的 xch: 镜像优先于自有条目淘汰。
const (
	evictWhereScope   = "scope_kind = ? AND scope_id = ? AND (source IS NULL OR source <> ?)"
	evictArchivedCond = "metadata_json IS NOT NULL AND metadata_json LIKE ?"
	evictActiveCond   = "(metadata_json IS NULL OR metadata_json NOT LIKE ?)"
	evictOrder        = "CASE WHEN id LIKE 'xch:%' THEN 0 ELSE 1 END, created_at ASC"
	archivedLike      = "%\"archived\":true%"
)

// evictIfNeeded 检查 scope 是否超出容量，超出时按以下顺序淘汰：
//  1. 每日摘要条目永不淘汰、不计数；
//  2. 先删除已归档条目（已被摘要/压缩，内容已保留在摘要里）；
//  3. 活跃条目仍超过上限时，先交给 Compactor 压缩（压缩后入库），
//     压缩失败则保留并告警；未配置 Compactor 时退化为按最旧删除（带日志）。
//
// 上限按「非摘要条目总数」计算；活跃条目单独与上限比较，因此归档条目
// 只在总数超限时被清理，不会挤占活跃条目的配额。
func (r *SQLiteRepository) evictIfNeeded(ctx context.Context, scope memory.Scope) {
	key := scope.Key()
	if _, busy := r.evicting.LoadOrStore(key, true); busy {
		return // 同 scope 已有淘汰在进行，下一次写入会再次检查
	}
	defer r.evicting.Delete(key)

	limit := r.config.MaxEntriesPerScope
	if limit <= 0 {
		return
	}
	base := func() *gorm.DB {
		return r.exactScope(r.db.WithContext(ctx).Model(&dao.EntryModel{}), scope).Where("(source IS NULL OR source <> ?)", DailyDigestSource)
	}

	var total int64
	if err := base().Count(&total).Error; err != nil {
		return // DB 错误，跳过淘汰
	}
	if int(total) <= limit {
		return
	}

	// 1) 已归档条目优先淘汰
	excess := int(total) - limit
	var archivedIDs []string
	if err := base().Where(evictArchivedCond, archivedLike).
		Order(evictOrder).Limit(excess).Pluck("id", &archivedIDs).Error; err != nil {
		return
	}
	r.deleteEvicted(ctx, scope, archivedIDs, "archived")

	// 2) 活跃条目超限：先摘要（压缩），再淘汰
	activeCount := func() (int, error) {
		var n int64
		err := base().Where(evictActiveCond, archivedLike).Count(&n).Error
		return int(n), err
	}
	active, err := activeCount()
	if err != nil || active <= limit {
		return
	}
	if r.compactor != nil {
		if cerr := r.compactor.CompactScope(ctx, scope); cerr != nil {
			r.log().Warnw("memory eviction: compaction failed, keeping unarchived entries over cap",
				"scope", key, "active", active, "cap", limit, "error", cerr)
			return
		}
		// 压缩会归档来源条目；先清理新产生的归档条目
		var n int64
		if err := base().Count(&n).Error; err == nil && int(n) > limit {
			var ids []string
			if base().Where(evictArchivedCond, archivedLike).
				Order(evictOrder).Limit(int(n)-limit).Pluck("id", &ids).Error == nil {
				r.deleteEvicted(ctx, scope, ids, "archived")
			}
		}
		if active, err = activeCount(); err != nil || active <= limit {
			return
		}
	}
	// 压缩后仍超限（或未配置 Compactor）：按最旧删除活跃条目（xch: 镜像优先）
	var oldIDs []string
	if err := base().Where(evictActiveCond, archivedLike).
		Order(evictOrder).Limit(active-limit).Pluck("id", &oldIDs).Error; err != nil {
		return
	}
	r.deleteEvicted(ctx, scope, oldIDs, "active")
}

// deleteEvicted 删除给定 id 并记录 Info 日志（scope、数量、类型、xch 镜像数）。
func (r *SQLiteRepository) deleteEvicted(ctx context.Context, scope memory.Scope, ids []string, kind string) {
	if len(ids) == 0 {
		return
	}
	result := r.exactScope(r.db.WithContext(ctx), scope).Where("id IN ?", ids).Delete(&dao.EntryModel{})
	if result.Error != nil {
		r.log().Warnw("memory eviction: delete failed", "scope", scope.Key(), "kind", kind, "error", result.Error)
		return
	}
	if result.RowsAffected > 0 {
		r.entriesDeleted.Add(result.RowsAffected)
	}
	mirrors := 0
	for _, id := range ids {
		if strings.HasPrefix(id, "xch:") {
			mirrors++
		}
	}
	r.log().Infow("memory eviction",
		"scope", scope.Key(), "kind", kind, "count", result.RowsAffected,
		"xch_mirrors", mirrors, "own", len(ids)-mirrors, "cap", r.config.MaxEntriesPerScope)
}

func (r *SQLiteRepository) log() *zap.SugaredLogger {
	if r.config.Logger != nil {
		return r.config.Logger
	}
	return zap.NewNop().Sugar()
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

// maybeCompact 决定是否对 scope 发起一次（增量）记忆去重。
//
// 触发条件：未经去重检查的条目数（不含 compactor 输出）>= DedupTriggerNew。
// 字符预算不再触发去重（见函数体注释）；全部条目都已检查过时不调用 LLM（此前
// 超预算的 scope 每 5 分钟就把全部条目重新送一遍 LLM）。冷却持久化在 memory_compact_states，重启不重置。
// 非阻塞调用方；内部已做并发重入保护与冷却，压缩失败不影响正常写入。
func (r *SQLiteRepository) maybeCompact(ctx context.Context, scope memory.Scope) {
	if isListenScope(scope) {
		return // 被动收听 scope 按天摘要（DailyDigester），不做滚动去重
	}
	unchecked, err := r.CountUncheckedDedup(ctx, scope)
	if err != nil || unchecked == 0 {
		return
	}
	if r.window != nil && r.charBudget(scope) <= 0 {
		// 注入了 window 却算出 0 预算 = 配置错误，必须告警（见 warnZeroBudget）。
		r.warnZeroBudget(ctx, scope)
	}
	// 只按「新条目数」触发。超出字符预算不再触发：去重后仍超预算的 scope 此前每个
	// 冷却周期都把同一批条目重新合并一遍（合并后长度几乎不变，after/before 中位数
	// 0.994），超预算由渲染层截断 / 条数淘汰兜底。
	if r.config.DedupTriggerNew <= 0 || unchecked < r.config.DedupTriggerNew {
		return
	}

	key := scope.Key()

	// 冷却：避免每轮写入都打 LLM（持久化，重启不重置）
	if t, ok := r.lastDedupRun(ctx, key); ok && time.Since(t) < r.config.DedupCooldown {
		return
	}

	// 并发重入保护：同一 scope 压缩进行中则跳过
	if _, loaded := r.compacting.LoadOrStore(key, true); loaded {
		return
	}
	defer r.compacting.Delete(key)

	r.storeDedupRun(ctx, key, time.Now())

	// 非致命：压缩失败不影响正常写入；渲染层仍有截断兜底
	_ = r.compactor.CompactScope(ctx, scope)
}

// warnZeroBudget 在字符预算为 0 时告警一次。
//
// 预算为 0 意味着自动压缩彻底静默失效：maybeCompact 每次提前返回，
// 记忆只增不减，而日志里看不出任何异常——故障表现为「存储空间缓慢膨胀」
// 而非「报错」，极难定位。
//
// 最常见的成因：Window 的 OutputReserve（默认 128000）+ ReservedTokens
// （默认 2000）之和不小于 MaxContextTokens，使 MemoryBudget() 返回 0。
// 把 MaxContextTokens 配到 13 万以下就会中招；生产默认 1000000 不受影响。
func (r *SQLiteRepository) warnZeroBudget(ctx context.Context, scope memory.Scope) {
	if r.budgetWarned.Swap(true) {
		return
	}
	traceid.L(ctx).Warnw("sqlite_repository: char budget is 0, auto-compaction will never trigger",
		"scope", scope.Key(),
		"memory_budget", r.window.MemoryBudget(),
		"hint", "检查 memory window 配置：OutputReserve + ReservedTokens 必须小于 MaxContextTokens")
}

// totalChars 统计指定 scope 下所有「活跃（未归档）」记忆的字符总数。
// 用于判断是否需要触发压缩。归档条目（archived=true）不计入。
func (r *SQLiteRepository) totalChars(ctx context.Context, scope memory.Scope) (int, error) {
	var models []dao.EntryModel
	if err := r.exactScope(r.db.WithContext(ctx), scope).Where(activeWhere, activeArgs...).
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
	if err := r.exactScope(r.db.WithContext(ctx), scope).
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
		if isEntryCompactSkipped(e.Metadata) {
			// 压缩时确定性失败（如内容安全审核被拒）已标记跳过：不再进入
			// 压缩活跃集，避免 maybeCompact 每轮冷却后重新取出并反复触发 LLM 失败。
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
	if err := r.exactScope(r.db.WithContext(ctx), scope).Where("id = ?", entryID).
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
	if err := r.exactScope(r.db.WithContext(ctx), scope).Model(&dao.EntryModel{}).
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

// isEntryCompactSkipped 检查元数据中是否有 compact_skipped=true 标记。
// 该标记表示此条目在压缩时遭遇确定性失败（如内容安全审核被拒），
// 相同内容重试必再次失败，故已退出压缩活跃集。
func isEntryCompactSkipped(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	skipped, ok := meta["compact_skipped"].(bool)
	return ok && skipped
}

// MarkCompactSkipped 将一批「压缩时确定性失败」的记忆标记，使其退出自动压缩的
// 活跃集，避免反复取出重试形成死循环。失败是确定性的（相同内容重试必再次被拒），
// 标记后 GetAllActive / totalChars 都会排除它们，maybeCompact 不再针对这些条目
// 触发 LLM 调用。返回成功标记的条目数（已标记的按幂等跳过）。
func (r *SQLiteRepository) MarkCompactSkipped(ctx context.Context, scope memory.Scope, entries []memory.Entry, reason string) int {
	count := 0
	for _, e := range entries {
		var model dao.EntryModel
		if err := r.exactScope(r.db.WithContext(ctx), scope).Where("id = ?", e.ID).
			First(&model).Error; err != nil {
			continue
		}
		var meta map[string]any
		if model.MetadataJSON != "" {
			_ = json.Unmarshal([]byte(model.MetadataJSON), &meta)
		}
		if meta == nil {
			meta = make(map[string]any)
		}
		if v, ok := meta["compact_skipped"].(bool); ok && v {
			count++
			continue
		}
		meta["compact_skipped"] = true
		meta["compact_skipped_at"] = time.Now()
		meta["compact_skip_reason"] = reason
		b, err := json.Marshal(meta)
		if err != nil {
			continue
		}
		if err := r.exactScope(r.db.WithContext(ctx), scope).Model(&dao.EntryModel{}).
			Where("id = ?", e.ID).
			Update("metadata_json", string(b)).Error; err != nil {
			continue
		}
		count++
	}
	return count
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

	var checked *time.Time
	if precheckedForDedup(entry) {
		now := time.Now()
		checked = &now
	}
	return dao.EntryModel{
		DedupCheckedAt: checked,
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

// likeContains matches text as a literal substring. % and _ in the caller's
// old_text must not become LIKE wildcards.
func likeContains(text string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
	return "%" + escaped + "%"
}

func modelsToEntries(models []dao.EntryModel) []memory.Entry {
	entries := make([]memory.Entry, len(models))
	for i, m := range models {
		entries[i] = modelToEntry(m)
	}
	return entries
}

// FetchEntries loads memory_entries rows by id for vector fusion.
func (r *SQLiteRepository) FetchEntries(ctx context.Context, ids []string) ([]memory.Entry, error) {
	if r == nil || len(ids) == 0 {
		return nil, nil
	}
	var models []dao.EntryModel
	if err := r.owned(r.db.WithContext(ctx)).Where("id IN ?", ids).Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: fetch by id")
	}
	out := make([]memory.Entry, 0, len(models))
	for _, m := range models {
		out = append(out, modelToEntry(m))
	}
	return out, nil
}

// DefaultMaxEntriesPerScope 每个 scope 默认的记忆条目上限（memory.max_entries_per_scope）。
const DefaultMaxEntriesPerScope = 1000
