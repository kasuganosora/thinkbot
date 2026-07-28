package memory

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/log"
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
//
// 持久化：若 db 非 nil，则内存 map 仍为检索/TTL/淘汰的工作副本（逻辑不变），
// 同时每个写操作 write-through 到 SQLite（表 tiered_memories），并在初始化时
// 从库加载未过期的条目。这样进程重启后分层记忆可恢复，避免“重启即失”。
type TieredStore struct {
	mu sync.RWMutex
	// key = tier:scope.Key() -> []TieredEntry
	buckets map[string][]TieredEntry
	configs map[MemoryTier]TierConfig

	// db 可选的持久化后端。nil 表示纯内存模式（兼容测试/旧行为）。
	db *gorm.DB

	// logger 用于持久化层的可观测性（DB 失败、加载统计）。
	// nil 时退化为 no-op，避免未初始化日志时 panic。
	logger *zap.SugaredLogger
}

// NewTieredStore 创建分层存储（纯内存模式）。
// configs 为 nil 时使用 DefaultTierConfigs()。
func NewTieredStore(configs map[MemoryTier]TierConfig) *TieredStore {
	return NewTieredStoreWithDB(configs, nil)
}

// NewTieredStoreWithDB 创建带 SQLite 持久化的分层存储。
// db 非 nil 时，初始化会从库加载未过期的条目，之后所有写操作 write-through。
func NewTieredStoreWithDB(configs map[MemoryTier]TierConfig, db *gorm.DB) *TieredStore {
	if configs == nil {
		configs = DefaultTierConfigs()
	}
	s := &TieredStore{
		buckets: make(map[string][]TieredEntry),
		configs: configs,
		db:      db,
		logger:  log.Logger,
	}
	if db != nil {
		s.loadFromDB()
	}
	return s
}

// loadFromDB 从 SQLite 加载未过期的分层记忆到内存 map。
// 仅在 NewTieredStoreWithDB 且 db 非 nil 时调用一次。
func (s *TieredStore) loadFromDB() {
	var models []dao.TieredMemoryModel
	if err := s.db.Order("created_at ASC").Find(&models).Error; err != nil {
		if s.logger != nil {
			s.logger.Errorw("tiered_store: failed to load persisted memories from db", "err", err)
		}
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded := 0
	for _, m := range models {
		// 跳过已过期条目（L0 TTL 已过）
		if m.ExpiresAt.IsZero() == false && now.After(m.ExpiresAt) {
			continue
		}
		te := modelToTiedEntry(m)
		key := tierScopeKey(te.Tier, te.Scope)
		s.buckets[key] = append(s.buckets[key], te)
		loaded++
	}
	if s.logger != nil {
		s.logger.Infow("tiered_store: loaded persisted memories from db",
			"loaded", loaded, "total_rows", len(models))
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
	s.mu.Unlock()

	if s.db != nil {
		s.persistUpsert(context.Background(), entry)
	}
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

// HasRecentActivity 检查 scope 在过去 hours 小时内是否有任何层级的写入。
// 用于 Dreaming 管线的活跃度过滤，避免对僵尸群组做无用 LLM 调用。
// 检查所有层级（L0-L3），因为 L0 条目 TTL 短（30分钟），长期记忆（L1-L3）也能反映活跃度。
func (s *TieredStore) HasRecentActivity(_ context.Context, scope Scope, hours float64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-time.Duration(hours * float64(time.Hour)))
	tiers := []MemoryTier{Tier0Working, Tier1LongTerm, Tier2Episodic, Tier3Profile}

	for _, tier := range tiers {
		key := tierScopeKey(tier, scope)
		for _, e := range s.buckets[key] {
			if e.CreatedAt.After(cutoff) && !e.IsExpired(time.Now()) {
				return true
			}
		}
	}
	return false
}

// Delete 按 ID 删除指定 tier+scope 下的一条记忆。
func (s *TieredStore) Delete(_ context.Context, tier MemoryTier, scope Scope, entryID string) error {
	key := tierScopeKey(tier, scope)

	s.mu.Lock()
	bucket := s.buckets[key]
	for i, e := range bucket {
		if e.ID == entryID {
			s.buckets[key] = append(bucket[:i], bucket[i+1:]...)
			s.mu.Unlock()
			if s.db != nil {
				s.persistDelete(tier, scope, entryID)
			}
			return nil
		}
	}
	s.mu.Unlock()
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
	s.mu.Unlock()

	if s.db != nil {
		if deleteID != "" {
			s.persistDelete(tier, scope, deleteID)
		}
		s.persistUpsert(context.Background(), newEntry)
	}
	return nil
}

// Clear 清空指定 tier+scope 的所有记忆。
func (s *TieredStore) Clear(_ context.Context, tier MemoryTier, scope Scope) error {
	key := tierScopeKey(tier, scope)
	s.mu.Lock()
	delete(s.buckets, key)
	s.mu.Unlock()
	if s.db != nil {
		s.persistClearTierScope(tier, scope)
	}
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
	if s.db != nil {
		s.persistClearTier(tier)
	}
	return nil
}

// GC 清理所有过期的 Tier0 条目。
// 返回被清理的条目数。
func (s *TieredStore) GC(_ context.Context) int {
	now := time.Now()
	removed := 0

	s.mu.Lock()

	// DB 中需删除的过期条目（解锁后统一删除，避免持锁做 I/O）
	var expired []struct {
		tier  MemoryTier
		scope Scope
		id    string
	}

	for key, bucket := range s.buckets {
		if tierOf(key) != Tier0Working {
			continue
		}
		var kept []TieredEntry
		for _, e := range bucket {
			if e.IsExpired(now) {
				removed++
				if s.db != nil {
					expired = append(expired, struct {
						tier  MemoryTier
						scope Scope
						id    string
					}{tier: Tier0Working, scope: e.Scope, id: e.ID})
				}
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

	s.mu.Unlock()

	if s.db != nil {
		for _, e := range expired {
			s.persistDelete(e.tier, e.scope, e.id)
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

	bucket := s.buckets[key]
	var updated []TieredEntry
	for i := range bucket {
		if _, ok := idSet[bucket[i].ID]; ok {
			if bucket[i].Metadata == nil {
				bucket[i].Metadata = make(map[string]any)
			}
			bucket[i].Metadata["consolidated"] = true
			bucket[i].Metadata["consolidated_at"] = time.Now()
			if s.db != nil {
				updated = append(updated, bucket[i])
			}
		}
	}
	s.mu.Unlock()

	if s.db != nil {
		for _, e := range updated {
			s.persistUpsert(context.Background(), e)
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

// ============================================================================
// 持久化辅助（SQLite write-through）
// ============================================================================

// tieredEntryToModel 将 TieredEntry 转换为 DAO 模型。
func tieredEntryToModel(e TieredEntry) dao.TieredMemoryModel {
	metadataJSON := ""
	if e.Metadata != nil {
		if b, err := json.Marshal(e.Metadata); err == nil {
			metadataJSON = string(b)
		}
	}
	return dao.TieredMemoryModel{
		ID:            e.ID,
		Tier:          int(e.Tier),
		ScopeKind:     string(e.Scope.Kind),
		ScopeID:       e.Scope.ID,
		Content:       e.Content,
		Category:      e.Category,
		Source:        e.Source,
		Importance:    e.Importance,
		MetadataJSON:  metadataJSON,
		ExpiresAt:     e.ExpiresAt,
		PromotedFrom:  int(e.PromotedFrom),
		CreatedAt:     e.CreatedAt,
		LastAccessedAt: e.LastAccessedAt,
	}
}

// modelToTiedEntry 将 DAO 模型转换回 TieredEntry。
func modelToTiedEntry(m dao.TieredMemoryModel) TieredEntry {
	var metadata map[string]any
	if m.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(m.MetadataJSON), &metadata)
	}
	return TieredEntry{
		Entry: Entry{
			ID:            m.ID,
			Scope:         Scope{Kind: ScopeKind(m.ScopeKind), ID: m.ScopeID},
			Content:       m.Content,
			Category:      m.Category,
			Source:        m.Source,
			Importance:    m.Importance,
			Metadata:      metadata,
			CreatedAt:     m.CreatedAt,
			LastAccessedAt: m.LastAccessedAt,
		},
		Tier:         MemoryTier(m.Tier),
		ExpiresAt:    m.ExpiresAt,
		PromotedFrom: MemoryTier(m.PromotedFrom),
	}
}

// persistUpsert 将单条记忆 upsert 到 SQLite（按 ID 主键）。
func (s *TieredStore) persistUpsert(ctx context.Context, e TieredEntry) {
	if s.db == nil {
		return
	}
	model := tieredEntryToModel(e)
	if err := s.db.WithContext(ctx).Save(&model).Error; err != nil {
		// 非致命：内存副本已更新，仅持久化同步失败。
		// 避免阻塞记忆写入主流程，但必须上报，否则数据静默丢失不可观测。
		if s.logger != nil {
			s.logger.Errorw("tiered_store: persist upsert failed (in-memory copy kept)",
				"id", e.ID, "tier", int(e.Tier),
				"scope_kind", string(e.Scope.Kind), "scope_id", e.Scope.ID, "err", err)
		}
	}
}

// persistDelete 按 tier+scope+id 从 SQLite 删除一条记忆。
func (s *TieredStore) persistDelete(tier MemoryTier, scope Scope, id string) {
	if s.db == nil {
		return
	}
	if err := s.db.Where("id = ? AND tier = ? AND scope_kind = ? AND scope_id = ?",
		id, int(tier), string(scope.Kind), scope.ID).
		Delete(&dao.TieredMemoryModel{}).Error; err != nil {
		if s.logger != nil {
			s.logger.Errorw("tiered_store: persist delete failed",
				"id", id, "tier", int(tier),
				"scope_kind", string(scope.Kind), "scope_id", scope.ID, "err", err)
		}
	}
}

// persistClearTierScope 清空 SQLite 中指定 tier+scope 的所有记忆。
func (s *TieredStore) persistClearTierScope(tier MemoryTier, scope Scope) {
	if s.db == nil {
		return
	}
	if err := s.db.Where("tier = ? AND scope_kind = ? AND scope_id = ?",
		int(tier), string(scope.Kind), scope.ID).
		Delete(&dao.TieredMemoryModel{}).Error; err != nil {
		if s.logger != nil {
			s.logger.Errorw("tiered_store: persist clear-tier-scope failed",
				"tier", int(tier), "scope_kind", string(scope.Kind), "scope_id", scope.ID, "err", err)
		}
	}
}

// persistClearTier 清空 SQLite 中整个 tier 的所有记忆。
func (s *TieredStore) persistClearTier(tier MemoryTier) {
	if s.db == nil {
		return
	}
	if err := s.db.Where("tier = ?", int(tier)).Delete(&dao.TieredMemoryModel{}).Error; err != nil {
		if s.logger != nil {
			s.logger.Errorw("tiered_store: persist clear-tier failed",
				"tier", int(tier), "err", err)
		}
	}
}
