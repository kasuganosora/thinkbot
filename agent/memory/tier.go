package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// MemoryTier — 记忆分层定义
//
// 参考 Memoh 和 TencentDB-Agent-Memory 的分层架构，将记忆按"蒸馏程度"分为
// 四个层级，每个层级有不同的生命周期、存储策略和检索优先级。
//
// 数据流：
//
//	对话/观察 → L0(原始) → [Consolidator] → L1(事实) → [Aggregator] → L2(场景) → [Profiler] → L3(画像)
//	             ↑                           ↑                         ↑                        ↑
//	         自动过期                   去重+重要度              定期聚类 L1           定期提取 L2
//
// 设计原则：
//   - 下层保留原始证据，上层保留结构化摘要
//   - 层级越高，数据越精炼、越稳定
//   - 检索时从上到下逐层降级（L3 → L2 → L1 → L0）
// ============================================================================

// MemoryTier 标识记忆所处的层级。
type MemoryTier int

const (
	// Tier0Working 工作记忆（最短期）。
	// 存储原始对话轮次和即时观察，自动过期（TTL 默认 30 分钟）。
	// 高吞吐写入，无 LLM 处理开销。
	Tier0Working MemoryTier = 0

	// Tier1LongTerm 长期记忆。
	// 通过 Consolidation Pipeline 从 L0 提取的结构化事实（fact/preference/event）。
	// 经过去重、重要度评估后持久化。
	Tier1LongTerm MemoryTier = 1

	// Tier2Episodic 场景记忆。
	// 将相关的 L1 记忆聚类为主题场景，提供紧凑的导航摘要。
	// 定期通过 LLM 聚合生成。
	Tier2Episodic MemoryTier = 2

	// Tier3Profile 用户画像（最长期）。
	// 从 L2 场景中蒸馏出的稳定人格特征和偏好。
	// 作为 system prompt 的持久部分注入，利于 prompt cache。
	Tier3Profile MemoryTier = 3
)

// String 返回层级名称。
func (t MemoryTier) String() string {
	switch t {
	case Tier0Working:
		return "L0_working"
	case Tier1LongTerm:
		return "L1_longterm"
	case Tier2Episodic:
		return "L2_episodic"
	case Tier3Profile:
		return "L3_profile"
	default:
		return "unknown"
	}
}

// ============================================================================
// TieredEntry — 带层级的记忆条目
// ============================================================================

// TieredEntry 带有层级元数据的记忆条目。
// 通过嵌入 Entry 保持与现有接口的兼容性。
type TieredEntry struct {
	Entry
	// Tier 记忆层级。
	Tier MemoryTier `json:"tier"`
	// ExpiresAt 过期时间（仅对 Tier0 有意义，零值表示不过期）。
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// PromotedFrom 标记此条目是从哪个层级提升而来。
	// 例如 L1 条目 PromotedFrom=Tier0Working 表示从 L0 提取。
	PromotedFrom MemoryTier `json:"promoted_from,omitempty"`
}

// IsExpired 判断 L0 条目是否已过期。
func (te *TieredEntry) IsExpired(now time.Time) bool {
	if te.Tier != Tier0Working {
		return false
	}
	return !te.ExpiresAt.IsZero() && now.After(te.ExpiresAt)
}

// ============================================================================
// TierConfig — 各层级配置
// ============================================================================

// TierConfig 定义单个记忆层级的配置。
type TierConfig struct {
	// MaxEntries 该层级的最大条目数（per scope）。
	// 超出时按 LRU 淘汰。0 表示无限制。
	MaxEntries int

	// TTL 条目的生存时间。
	// 过期后由 TieredManager 的 GC 自动清理。
	// 仅对 Tier0 有意义。零值表示不过期。
	TTL time.Duration

	// ConsolidateThreshold 触发提升（L0→L1）的阈值。
	// 当未处理的 L0 条目达到此数量时触发 Consolidator。
	// 仅对 Tier0 有意义。
	ConsolidateThreshold int

	// AggregateInterval 触发聚合（L1→L2）的时间间隔。
	// 仅对 Tier1 有意义。零值表示禁用自动聚合。
	AggregateInterval time.Duration
}

// DefaultTierConfigs 返回各层级的默认配置。
func DefaultTierConfigs() map[MemoryTier]TierConfig {
	return map[MemoryTier]TierConfig{
		Tier0Working: {
			MaxEntries:           200,
			TTL:                  30 * time.Minute,
			ConsolidateThreshold: 20,
		},
		Tier1LongTerm: {
			MaxEntries:           500,
			AggregateInterval:    2 * time.Hour,
			ConsolidateThreshold: 0,
		},
		Tier2Episodic: {
			MaxEntries: 50,
		},
		Tier3Profile: {
			MaxEntries: 20,
		},
	}
}

// ============================================================================
// TieredStore — 分层记忆存储（线程安全）
// ============================================================================

// TieredStore 按 tier 和 scope 分桶存储 TieredEntry。
// 它是 TieredMemoryManager 的存储后端。
//
// 线程安全：所有操作通过 sync.RWMutex 保护。
type TieredStore struct {
	mu sync.RWMutex
	// key = tier:scope.Key() -> []TieredEntry
	buckets map[string][]TieredEntry
	configs map[MemoryTier]TierConfig
}

// NewTieredStore 创建分层存储。
// configs 为 nil 时使用 DefaultTierConfigs()。
func NewTieredStore(configs map[MemoryTier]TierConfig) *TieredStore {
	if configs == nil {
		configs = DefaultTierConfigs()
	}
	return &TieredStore{
		buckets: make(map[string][]TieredEntry),
		configs: configs,
	}
}

// Append 追加一条 TieredEntry 到对应 tier+scope 桶。
func (s *TieredStore) Append(_ context.Context, entry TieredEntry) error {
	if entry.ID == "" {
		entry.ID = idgen.New("mem")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.LastAccessedAt.IsZero() {
		entry.LastAccessedAt = entry.CreatedAt
	}

	// 自动设置过期时间
	if entry.Tier == Tier0Working && entry.ExpiresAt.IsZero() {
		if cfg, ok := s.configs[Tier0Working]; ok && cfg.TTL > 0 {
			entry.ExpiresAt = entry.CreatedAt.Add(cfg.TTL)
		}
	}

	key := tierScopeKey(entry.Tier, entry.Scope)

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.buckets[key]
	bucket = append(bucket, entry)

	// 容量限制
	if cfg, ok := s.configs[entry.Tier]; ok && cfg.MaxEntries > 0 {
		if len(bucket) > cfg.MaxEntries {
			excess := len(bucket) - cfg.MaxEntries
			newBucket := make([]TieredEntry, cfg.MaxEntries)
			copy(newBucket, bucket[excess:])
			bucket = newBucket
		}
	}

	s.buckets[key] = bucket
	return nil
}

// Retrieve 从指定 tier 和 scope 检索记忆。
func (s *TieredStore) Retrieve(_ context.Context, tier MemoryTier, scopes []Scope, limit int) ([]TieredEntry, error) {
	if limit <= 0 {
		limit = 10
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var candidates []TieredEntry
	if len(scopes) == 0 {
		// 搜索该 tier 下所有 scope
		for k, bucket := range s.buckets {
			if tierOf(k) == tier {
				candidates = append(candidates, bucket...)
			}
		}
	} else {
		for _, scope := range scopes {
			key := tierScopeKey(tier, scope)
			if bucket, ok := s.buckets[key]; ok {
				candidates = append(candidates, bucket...)
			}
		}
	}

	// 过滤过期条目
	now := time.Now()
	var results []TieredEntry
	for _, e := range candidates {
		if e.IsExpired(now) {
			continue
		}
		results = append(results, e)
	}

	// 按时间倒序
	sortTieredEntriesByTimeDesc(results)

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// GetAll 获取指定 tier 下某个 scope 的全部条目（无 limit）。
func (s *TieredStore) GetAll(_ context.Context, tier MemoryTier, scope Scope) ([]TieredEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := tierScopeKey(tier, scope)
	bucket := s.buckets[key]
	if len(bucket) == 0 {
		return nil, nil
	}

	// 返回副本
	out := make([]TieredEntry, len(bucket))
	copy(out, bucket)
	return out, nil
}

// Count 返回指定 tier+scope 的条目数（不含过期）。
func (s *TieredStore) Count(_ context.Context, tier MemoryTier, scope Scope) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := tierScopeKey(tier, scope)
	bucket := s.buckets[key]

	now := time.Now()
	count := 0
	for _, e := range bucket {
		if !e.IsExpired(now) {
			count++
		}
	}
	return count, nil
}

// Delete 按 ID 删除指定 tier+scope 下的一条记忆。
func (s *TieredStore) Delete(_ context.Context, tier MemoryTier, scope Scope, entryID string) error {
	key := tierScopeKey(tier, scope)

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.buckets[key]
	for i, e := range bucket {
		if e.ID == entryID {
			s.buckets[key] = append(bucket[:i], bucket[i+1:]...)
			return nil
		}
	}
	return nil
}

// Replace 原子性地删除旧条目（按 ID）并追加新条目到同一 tier+scope 桶中。
// 操作在单个写锁内完成，避免 Delete+Append 分离时的中间状态
// （如 Append 失败但旧条目已被删除导致数据丢失）。
// 如果 deleteID 为空或不存在，则仅追加新条目。
func (s *TieredStore) Replace(_ context.Context, tier MemoryTier, scope Scope, deleteID string, newEntry TieredEntry) error {
	newEntry.Tier = tier

	if newEntry.ID == "" {
		newEntry.ID = idgen.New("mem")
	}
	if newEntry.CreatedAt.IsZero() {
		newEntry.CreatedAt = time.Now()
	}
	if newEntry.LastAccessedAt.IsZero() {
		newEntry.LastAccessedAt = newEntry.CreatedAt
	}

	// 自动设置过期时间（仅 Tier0）
	if newEntry.Tier == Tier0Working && newEntry.ExpiresAt.IsZero() {
		if cfg, ok := s.configs[Tier0Working]; ok && cfg.TTL > 0 {
			newEntry.ExpiresAt = newEntry.CreatedAt.Add(cfg.TTL)
		}
	}

	key := tierScopeKey(tier, scope)

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.buckets[key]

	// 删除旧条目
	if deleteID != "" {
		for i, e := range bucket {
			if e.ID == deleteID {
				bucket = append(bucket[:i], bucket[i+1:]...)
				break
			}
		}
	}

	// 追加新条目
	bucket = append(bucket, newEntry)

	// 容量限制
	if cfg, ok := s.configs[tier]; ok && cfg.MaxEntries > 0 {
		if len(bucket) > cfg.MaxEntries {
			excess := len(bucket) - cfg.MaxEntries
			newBucket := make([]TieredEntry, cfg.MaxEntries)
			copy(newBucket, bucket[excess:])
			bucket = newBucket
		}
	}

	s.buckets[key] = bucket
	return nil
}

// Clear 清空指定 tier+scope 的所有记忆。
func (s *TieredStore) Clear(_ context.Context, tier MemoryTier, scope Scope) error {
	key := tierScopeKey(tier, scope)
	s.mu.Lock()
	delete(s.buckets, key)
	s.mu.Unlock()
	return nil
}

// ClearTier 清空整个 tier 的所有 scope。
func (s *TieredStore) ClearTier(_ context.Context, tier MemoryTier) error {
	s.mu.Lock()
	for k := range s.buckets {
		if tierOf(k) == tier {
			delete(s.buckets, k)
		}
	}
	s.mu.Unlock()
	return nil
}

// GC 清理所有过期的 Tier0 条目。
// 返回被清理的条目数。
func (s *TieredStore) GC(_ context.Context) int {
	now := time.Now()
	removed := 0

	s.mu.Lock()
	defer s.mu.Unlock()

	for key, bucket := range s.buckets {
		if tierOf(key) != Tier0Working {
			continue
		}
		var kept []TieredEntry
		for _, e := range bucket {
			if e.IsExpired(now) {
				removed++
			} else {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(s.buckets, key)
		} else if len(kept) != len(bucket) {
			s.buckets[key] = kept
		}
	}

	return removed
}

// MarkProcessed 标记 L0 条目为"已处理"（通过设置 Metadata）。
// 用于 Consolidator 跟踪哪些 L0 条目尚未提升到 L1。
func (s *TieredStore) MarkProcessed(_ context.Context, scope Scope, entryIDs []string) error {
	if len(entryIDs) == 0 {
		return nil
	}

	idSet := make(map[string]struct{}, len(entryIDs))
	for _, id := range entryIDs {
		idSet[id] = struct{}{}
	}

	key := tierScopeKey(Tier0Working, scope)

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.buckets[key]
	for i := range bucket {
		if _, ok := idSet[bucket[i].ID]; ok {
			if bucket[i].Metadata == nil {
				bucket[i].Metadata = make(map[string]any)
			}
			bucket[i].Metadata["consolidated"] = true
			bucket[i].Metadata["consolidated_at"] = time.Now()
		}
	}
	return nil
}

// GetUnprocessed 获取尚未被 Consolidator 处理的 L0 条目。
func (s *TieredStore) GetUnprocessed(_ context.Context, scope Scope, limit int) ([]TieredEntry, error) {
	if limit <= 0 {
		limit = 50
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	key := tierScopeKey(Tier0Working, scope)
	bucket := s.buckets[key]

	now := time.Now()
	var results []TieredEntry
	for _, e := range bucket {
		if e.IsExpired(now) {
			continue
		}
		if e.Metadata != nil {
			if _, ok := e.Metadata["consolidated"]; ok {
				continue
			}
		}
		results = append(results, e)
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

// Snapshot 返回各层级各 scope 的条目计数（用于 metrics/调试）。
func (s *TieredStore) Snapshot() map[MemoryTier]map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[MemoryTier]map[string]int)
	for k, bucket := range s.buckets {
		tier := tierOf(k)
		if result[tier] == nil {
			result[tier] = make(map[string]int)
		}
		result[tier][k] = len(bucket)
	}
	return result
}

// ============================================================================
// TieredStore 实现 Store 接口（降级为 Tier0 写入）
// ============================================================================

// AppendLegacy 将普通 Entry 作为 Tier0 条目写入。
// 使 TieredStore 兼容现有 Store 接口。
func (s *TieredStore) AppendLegacy(ctx context.Context, entry Entry) error {
	return s.Append(ctx, TieredEntry{
		Entry: entry,
		Tier:  Tier0Working,
	})
}

// ============================================================================
// Helpers
// ============================================================================

// tierScopeKey 生成 tier:scope 的复合存储键。
func tierScopeKey(tier MemoryTier, scope Scope) string {
	return tier.String() + "|" + scope.Key()
}

// tierOf 从复合键中提取 tier 名称前缀（不含 scope 部分）。
// 注意：这里返回的是 tier 字符串前缀，不是 MemoryTier 枚举。
func tierOf(key string) MemoryTier {
	// 格式: "L0_working|channel:xxx"
	pipe := -1
	for i, c := range key {
		if c == '|' {
			pipe = i
			break
		}
	}
	if pipe < 0 {
		return MemoryTier(-1)
	}
	prefix := key[:pipe]
	switch prefix {
	case "L0_working":
		return Tier0Working
	case "L1_longterm":
		return Tier1LongTerm
	case "L2_episodic":
		return Tier2Episodic
	case "L3_profile":
		return Tier3Profile
	default:
		return MemoryTier(-1)
	}
}

// sortTieredEntriesByTimeDesc 按 CreatedAt 降序排列。
func sortTieredEntriesByTimeDesc(entries []TieredEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})
}
