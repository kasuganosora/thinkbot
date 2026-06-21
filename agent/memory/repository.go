package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// MemoryRepository — 内存实现（开发/测试用）
// ============================================================================

// MemoryRepositoryConfig 配置内存记忆仓储。
type MemoryRepositoryConfig struct {
	// MaxEntriesPerScope 每个 scope 的最大记忆条目数（默认 1000）。
	// 超过时按 LRU 或最旧淘汰。
	MaxEntriesPerScope int
	// DefaultLimit 检索时的默认返回条数（默认 10）。
	DefaultLimit int
}

// DefaultMemoryRepositoryConfig 返回默认配置。
func DefaultMemoryRepositoryConfig() MemoryRepositoryConfig {
	return MemoryRepositoryConfig{
		MaxEntriesPerScope: 1000,
		DefaultLimit:       10,
	}
}

// MemoryRepository 将记忆存储在内存中。
// 使用 map[string][]Entry 按 scope key 分桶存储。
// 实现 Repository 接口（Store + Retriever）。
//
// 线程安全：所有操作通过 sync.RWMutex 保护。
// 适用于开发/测试和单机小规模部署。
type MemoryRepository struct {
	config MemoryRepositoryConfig

	mu      sync.RWMutex
	buckets map[string][]Entry // scope.Key() -> entries

	// metrics
	entriesAppended atomic.Int64
	entriesDeleted  atomic.Int64
	retrievals      atomic.Int64
}

// NewMemoryRepository 创建内存记忆仓储。
func NewMemoryRepository(opts ...MemoryRepositoryConfig) *MemoryRepository {
	cfg := DefaultMemoryRepositoryConfig()
	if len(opts) > 0 {
		if opts[0].MaxEntriesPerScope > 0 {
			cfg.MaxEntriesPerScope = opts[0].MaxEntriesPerScope
		}
		if opts[0].DefaultLimit > 0 {
			cfg.DefaultLimit = opts[0].DefaultLimit
		}
	}
	return &MemoryRepository{
		config:  cfg,
		buckets: make(map[string][]Entry),
	}
}

// ============================================================================
// Store 实现（写入侧）
// ============================================================================

// Append 追加一条记忆到指定 scope。
func (r *MemoryRepository) Append(_ context.Context, entry Entry) error {
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

	key := entry.Scope.Key()

	r.mu.Lock()
	defer r.mu.Unlock()

	bucket := r.buckets[key]
	bucket = append(bucket, entry)

	// 容量限制：淘汰最旧条目
	// 使用 copy 释放底层 array 前段，避免内存泄漏
	if len(bucket) > r.config.MaxEntriesPerScope {
		excess := len(bucket) - r.config.MaxEntriesPerScope
		newBucket := make([]Entry, r.config.MaxEntriesPerScope)
		copy(newBucket, bucket[excess:])
		bucket = newBucket
	}

	r.buckets[key] = bucket
	r.entriesAppended.Add(1)

	return nil
}

// Delete 按 ID 删除指定 scope 下的一条记忆。
func (r *MemoryRepository) Delete(_ context.Context, scope Scope, entryID string) error {
	key := scope.Key()

	r.mu.Lock()
	defer r.mu.Unlock()

	bucket := r.buckets[key]
	for i, e := range bucket {
		if e.ID == entryID {
			r.buckets[key] = append(bucket[:i], bucket[i+1:]...)
			r.entriesDeleted.Add(1)
			return nil
		}
	}

	return nil // 不存在时静默返回
}

// Replace 原子性地删除旧条目（按 ID）并追加新条目到指定 scope。
// 操作在单个写锁内完成，避免 Delete+Append 分离时的中间状态
// （如 Append 失败但旧条目已被删除导致数据丢失）。
// 如果 deleteID 为空或不存在，则仅追加新条目。
func (r *MemoryRepository) Replace(_ context.Context, scope Scope, deleteID string, newEntry Entry) error {
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

	key := scope.Key()

	r.mu.Lock()
	defer r.mu.Unlock()

	bucket := r.buckets[key]

	// 删除旧条目
	if deleteID != "" {
		for i, e := range bucket {
			if e.ID == deleteID {
				bucket = append(bucket[:i], bucket[i+1:]...)
				r.entriesDeleted.Add(1)
				break
			}
		}
	}

	// 追加新条目
	bucket = append(bucket, newEntry)

	// 容量限制
	if len(bucket) > r.config.MaxEntriesPerScope {
		excess := len(bucket) - r.config.MaxEntriesPerScope
		newBucket := make([]Entry, r.config.MaxEntriesPerScope)
		copy(newBucket, bucket[excess:])
		bucket = newBucket
	}

	r.buckets[key] = bucket
	r.entriesAppended.Add(1)
	return nil
}

// Clear 清空指定 scope 的所有记忆。
func (r *MemoryRepository) Clear(_ context.Context, scope Scope) error {
	key := scope.Key()

	r.mu.Lock()
	defer r.mu.Unlock()

	if bucket, ok := r.buckets[key]; ok {
		r.entriesDeleted.Add(int64(len(bucket)))
		delete(r.buckets, key)
	}

	return nil
}

// ============================================================================
// Retriever 实现（查询侧）
// ============================================================================

// Retrieve 根据查询条件检索记忆。
func (r *MemoryRepository) Retrieve(_ context.Context, query Query) ([]Entry, error) {
	r.retrievals.Add(1)

	limit := query.Limit
	if limit <= 0 {
		limit = r.config.DefaultLimit
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// 收集候选条目
	var candidates []Entry

	if len(query.Scopes) == 0 {
		// 无 scope 限制，搜索所有桶
		for _, bucket := range r.buckets {
			candidates = append(candidates, bucket...)
		}
	} else {
		for _, scope := range query.Scopes {
			key := scope.Key()
			if bucket, ok := r.buckets[key]; ok {
				candidates = append(candidates, bucket...)
			}
		}
	}

	// 过滤
	var results []Entry
	for i := range candidates {
		entry := &candidates[i]

		// Category 过滤
		if query.Category != "" && entry.Category != query.Category {
			continue
		}

		// Importance 过滤
		if query.MinImportance > 0 && entry.Importance < query.MinImportance {
			continue
		}

		// 文本关键词匹配（子串，不区分大小写）
		if query.Text != "" {
			if !containsIgnoreCase(entry.Content, query.Text) {
				continue
			}
		}

		results = append(results, *entry)
	}

	// 按时间倒序排列（最新的在前）
	sortByTimeDesc(results)

	// 截断到 limit
	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// Recent 获取指定 scope 的最近 N 条记忆。
func (r *MemoryRepository) Recent(_ context.Context, scope Scope, limit int) ([]Entry, error) {
	r.retrievals.Add(1)

	if limit <= 0 {
		limit = r.config.DefaultLimit
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	key := scope.Key()
	bucket, ok := r.buckets[key]
	if !ok || len(bucket) == 0 {
		return nil, nil
	}

	// 取最后 N 条并倒序返回
	start := len(bucket) - limit
	if start < 0 {
		start = 0
	}

	results := make([]Entry, 0, limit)
	for i := len(bucket) - 1; i >= start; i-- {
		results = append(results, bucket[i])
	}

	return results, nil
}

// Count 返回指定 scope 的记忆总数。
func (r *MemoryRepository) Count(_ context.Context, scope Scope) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := scope.Key()
	return len(r.buckets[key]), nil
}

// ============================================================================
// Metrics
// ============================================================================

// RepositoryMetrics 是仓储的运行指标快照。
type RepositoryMetrics struct {
	TotalScopes     int   `json:"total_scopes"`
	TotalEntries    int   `json:"total_entries"`
	EntriesAppended int64 `json:"entries_appended"`
	EntriesDeleted  int64 `json:"entries_deleted"`
	Retrievals      int64 `json:"retrievals"`
}

// Metrics 返回当前指标快照。
func (r *MemoryRepository) Metrics() RepositoryMetrics {
	r.mu.RLock()
	totalScopes := len(r.buckets)
	totalEntries := 0
	for _, bucket := range r.buckets {
		totalEntries += len(bucket)
	}
	r.mu.RUnlock()

	return RepositoryMetrics{
		TotalScopes:     totalScopes,
		TotalEntries:    totalEntries,
		EntriesAppended: r.entriesAppended.Load(),
		EntriesDeleted:  r.entriesDeleted.Load(),
		Retrievals:      r.retrievals.Load(),
	}
}

// ============================================================================
// Helpers
// ============================================================================

// containsIgnoreCase 判断 s 中是否包含 substr（不区分大小写）。
func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// sortByTimeDesc 按 CreatedAt 降序排列。
func sortByTimeDesc(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})
}
