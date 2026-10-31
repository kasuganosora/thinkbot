package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

const (
	// compactLLMTimeout 单次压缩批调用 GLM 聚类合并的独立超时（兜底上限）。
	//
	// 为什么是 10 分钟而不是 120s：
	// 压缩是「尽力而为」的后台维护，大 prompt 下 GLM（尤其 glm-5.3）首 token 延迟
	// 可能超过 2 分钟。历史上此处设为 120s，导致 compactBatch 用
	// context.WithoutCancel(ctx) 剥离调用方 deadline 后只给了 120s，GLM 还在
	// 生成就被 context deadline exceeded 杀掉——无论自动路径（maybeCompact）还是
	// 手动路径（/compact）的 LLM 聚类合并全部静默失败，只有本地 pre-LLM 预压缩
	// 真正生效（见日志 "LLM cluster+merge failed, skipping batch"）。
	//
	// 这里给每个 batch 一个扁平的 10 分钟上限（与底层 HTTP 客户端 20min 超时留有
	// 余量），既让慢 GLM 有充足时间返回，又通过 compactBatch 的逐批跳过逻辑保证
	// 单批失败不影响其他批次、且不会无限挂起。HTTP 客户端超时为 20min（见
	// agent/bot/llm_factory.go 的 llmClientTimeout），本值必须 < 它。
	compactLLMTimeout = 10 * time.Minute
)

// ============================================================================
// SQLiteCompactor — 生产路径的语义记忆压缩器
//
// 与 memory.SemanticCompactor（面向 TieredStore）并列：生产路径的存储是
// SQLiteRepository，故单独实现一份针对它的压缩器。二者复用 memory.ClusterMerge
// 的 LLM 聚类合并逻辑，仅在「读取来源 / 写回合并 / 归档来源」三步上适配各自的存储。
//
// 行为：
//  1. 读取该 scope 全部活跃（未归档）记忆
//  2. 调用 LLM 聚类 + 合并相似条目
//  3. 写入合并后的新条目（source=compactor）
//  4. 将来源条目标记为 archived（而非删除），保持可追溯
//
// 触发方为 SQLiteRepository.Append：当某 scope 字符数超过 window 派生的预算阈值时，
// 异步调用 CompactScope，实现「压缩后入库」而非截断。
// ============================================================================

// SQLiteCompactorConfig 配置 SQLite 记忆压缩器。
type SQLiteCompactorConfig struct {
	// Provider LLM 提供商（必填）。
	Provider llm.Provider
	// Model 指定模型（建议轻量模型）。
	Model *llm.Model
	// SystemPrompt 压缩用的系统提示词；留空使用 memory 默认压缩提示词。
	SystemPrompt string
	// SimilarityThreshold 仅作参考（LLM 自主决定聚类边界），保留以对齐 SemanticsCompactor。
	SimilarityThreshold float64
	// MinClusterSize 最小聚类大小（默认 2，至少 2 条才合并）。
	MinClusterSize int
	// MaxClusterSize 最大聚类大小（默认 10）。
	MaxClusterSize int
	// MaxInputEntries 单次压缩的最大输入条目数（默认 50）。
	MaxInputEntries int
	// MaxTokens 聚类合并调用的输出上限（取模型配置的 maxTokens；0 = 默认兜底）。
	MaxTokens int
	// Policy 内部调用策略（memory_dedup 的 reasoning_effort / 输出封顶；nil = 不发 reasoning_effort）。
	Policy *llm.InternalPolicy
	// NeighbourTopK 每个未检查条目从 memory_vec 取的近邻数（默认 DefaultDedupNeighbourTopK）。
	NeighbourTopK int
	// NeighbourMinSimilarity 近邻入组的最低相似度（memory_vec 同款哈希向量余弦，
	// 默认 DefaultDedupMinSimilarity）。
	NeighbourMinSimilarity float64
	// MaxSeedsPerRun 单次去重最多处理的未检查条目数（默认 DefaultDedupMaxSeeds），
	// 存量未检查条目分多轮消化，单轮 LLM 开销有上限。
	MaxSeedsPerRun int
	// FallbackRecent memory_vec 不可用时每组附带的最近已检查条目数（默认 3）。
	FallbackRecent int
	// MaxMergedChars 合并结果的长度上限（字符，默认 DefaultDedupMaxMergedChars=800）。
	// 只作用于「正常大小」条目的合并：长于该值的来源条目不参与合并（原样保留，
	// 仅在注入 prompt 时截断），绝不把存量超长条目压到 800；合并结果超过上限则放弃
	// 该次合并（来源保留），而不是截断——09-27 的细节丢失正是过度压缩造成的。
	MaxMergedChars int
	// NoopSimilarity 合并结果与任一来源的相似度 >= 该值时视为无效合并（不写入，
	// 来源标记已检查；默认 0.9）。
	NoopSimilarity float64
}

// Defaults of the incremental dedup selection.
const (
	DefaultDedupNeighbourTopK  = 5
	DefaultDedupMinSimilarity  = 0.50
	DefaultDedupMaxSeeds       = 30
	DefaultDedupFallbackRecent = 3
	DefaultDedupMaxMergedChars = 800
	DefaultDedupNoopSimilarity = 0.9
)

// SQLiteCompactor 对 SQLiteRepository 中的记忆执行语义压缩。
type SQLiteCompactor struct {
	repo   *SQLiteRepository
	config memory.CompactionConfig
	sel    dedupSelectConfig
	logger *zap.SugaredLogger
	// sf 按 scope.Key() 对压缩做单飞（singleflight）：同一 scope 同一时刻
	// 只跑一个压缩，避免手动 /compact 与自动 maybeCompact 并发跑同一 scope
	// 导致双倍 GLM 聚类合并消耗与重复归档。不同 scope 之间不受影响。
	sf singleflight.Group
}

// NewSQLiteCompactor 创建 SQLite 记忆压缩器。
// repo 在 SetRepository 中注入，以打破 NewSQLiteRepository ↔ NewSQLiteCompactor
// 的构造循环依赖（两者互相引用）。
func NewSQLiteCompactor(cfg SQLiteCompactorConfig, logger *zap.SugaredLogger) *SQLiteCompactor {
	mc := memory.CompactionConfig{
		Provider:    cfg.Provider,
		Model:       cfg.Model,
		Precompress: true, // LLM 摘要前先做确定性瘦身（JSON 紧凑化+must-keep+回退校验）
		MaxTokens:   cfg.MaxTokens,
		Policy:      cfg.Policy,
	}
	if cfg.SystemPrompt != "" {
		mc.SystemPrompt = cfg.SystemPrompt
	} else {
		mc.SystemPrompt = memory.DefaultCompactionConfig().SystemPrompt
	}
	if cfg.SimilarityThreshold > 0 {
		mc.SimilarityThreshold = cfg.SimilarityThreshold
	} else {
		mc.SimilarityThreshold = 0.6
	}
	if cfg.MinClusterSize >= 2 {
		mc.MinClusterSize = cfg.MinClusterSize
	} else {
		mc.MinClusterSize = 2
	}
	if cfg.MaxClusterSize == 0 {
		mc.MaxClusterSize = 10 // documented default (was silently MinClusterSize)
	} else if cfg.MaxClusterSize >= mc.MinClusterSize {
		mc.MaxClusterSize = cfg.MaxClusterSize
	} else {
		mc.MaxClusterSize = mc.MinClusterSize
	}
	if cfg.MaxInputEntries > 0 {
		mc.MaxInputEntries = cfg.MaxInputEntries
	} else {
		mc.MaxInputEntries = 50
	}
	sel := dedupSelectConfig{
		topK: cfg.NeighbourTopK, minSim: cfg.NeighbourMinSimilarity,
		maxSeeds: cfg.MaxSeedsPerRun, fallbackRecent: cfg.FallbackRecent, maxGroup: mc.MaxClusterSize,
	}
	if sel.topK <= 0 {
		sel.topK = DefaultDedupNeighbourTopK
	}
	if sel.minSim <= 0 {
		sel.minSim = DefaultDedupMinSimilarity
	}
	if sel.maxSeeds <= 0 {
		sel.maxSeeds = DefaultDedupMaxSeeds
	}
	if sel.fallbackRecent <= 0 {
		sel.fallbackRecent = DefaultDedupFallbackRecent
	}
	if sel.maxGroup < 2 {
		sel.maxGroup = 10
	}
	sel.maxMergedChars = cfg.MaxMergedChars
	if sel.maxMergedChars <= 0 {
		sel.maxMergedChars = DefaultDedupMaxMergedChars
	}
	sel.noopSim = cfg.NoopSimilarity
	if sel.noopSim <= 0 {
		sel.noopSim = DefaultDedupNoopSimilarity
	}
	mc.SystemPrompt += fmt.Sprintf("\n\nLength: keep each merged_content concise, at most %d characters. "+
		"Concise means removing repetition, never dropping details. "+
		"If a cluster cannot fit in %d characters with every detail kept, do not merge it.", sel.maxMergedChars, sel.maxMergedChars)
	return &SQLiteCompactor{
		sel:    sel,
		config: mc,
		logger: logger.With("component", "sqlite_compactor"),
	}
}

// SetRepository 注入目标仓储（构造后调用）。
func (c *SQLiteCompactor) SetRepository(repo *SQLiteRepository) {
	c.repo = repo
}

// CompactScope 对该 scope 执行语义压缩（实现 storage.MemoryCompactor 接口）。
// 返回 error 仅用于上层感知；压缩失败不影响正常写入流程。
//
// 同一 scope 通过 singleflight 串行化：手动 /compact（注入式 API / web 命令）
// 与自动 maybeCompact 可能并发触发同一 scope，串行化可避免双倍 GLM 聚类合并
// 消耗与重复归档。不同 scope 之间互不影响。
func (c *SQLiteCompactor) CompactScope(ctx context.Context, scope memory.Scope) error {
	if c.repo == nil {
		return nil
	}
	_, err, _ := c.sf.Do(scope.Key(), func() (interface{}, error) {
		return nil, c.compactScope(ctx, scope)
	})
	return err
}

// compactScope 对单 scope 做增量去重（由 CompactScope 经 singleflight 调用）。
//
// 只处理未经检查的条目（dedup_checked_at IS NULL）：每个未检查条目取
// memory_vec 中相似度达标的近邻组成 2-10 条的小组送 ClusterMerge；没有近邻的
// 条目直接标记已检查、不调用 LLM。未被合并的组内条目标记已检查，下次不再重送。
// memory_vec 不可用时退化为「未检查条目分块 + 少量最近已检查条目」。
func (c *SQLiteCompactor) compactScope(ctx context.Context, scope memory.Scope) error {
	start := time.Now()
	if isListenScope(scope) {
		return nil // digested once per day (DailyDigester), not deduplicated
	}

	all, err := c.repo.UncheckedDedup(ctx, scope, c.sel.maxSeeds)
	if err != nil {
		return errs.Wrap(err, "sqlite_compactor: get unchecked entries")
	}
	if len(all) == 0 {
		return nil
	}
	// Oversized entries (> MaxMergedChars) never take part in a merge: they
	// stay as they are and are marked checked.
	var seeds []memory.Entry
	var oversized []string
	for _, e := range all {
		if c.oversized(e) {
			oversized = append(oversized, e.ID)
			continue
		}
		seeds = append(seeds, e)
	}

	var groups [][]memory.Entry
	var lonely []string
	vec := c.repo.Vec()
	mode := "vec"
	if vec.Enabled() {
		groups, lonely = buildDedupGroups(seeds, func(seed memory.Entry) []memory.Entry {
			return c.vecNeighbours(ctx, scope, vec, seed)
		}, c.sel.maxGroup)
	} else {
		mode = "recent"
		recent, _ := c.repo.RecentCheckedDedup(ctx, scope, c.sel.fallbackRecent*3)
		var small []memory.Entry
		for _, e := range recent {
			if !c.oversized(e) && len(small) < c.sel.fallbackRecent {
				small = append(small, e)
			}
		}
		groups, lonely = buildFallbackGroups(seeds, small, c.sel.maxGroup)
	}
	marked := c.repo.MarkDedupChecked(ctx, scope, append(lonely, oversized...))

	mergedTotal, archivedTotal, savedTotal, calls := 0, 0, 0, 0
	for _, g := range groups {
		m, a, s, done := c.compactBatch(ctx, scope, g)
		calls++
		mergedTotal += m
		archivedTotal += a
		savedTotal += s
		if done != nil {
			marked += c.repo.MarkDedupChecked(ctx, scope, done)
		}
	}

	c.logger.Infow("sqlite semantic compaction complete",
		"scope", scope.Key(),
		"mode", mode,
		"unchecked_seeds", len(seeds),
		"groups", len(groups),
		"llm_groups", calls,
		"no_neighbour", len(lonely),
		"oversized_skipped", len(oversized),
		"marked_checked", marked,
		"merged", mergedTotal,
		"archived", archivedTotal,
		"precompressed_saved", savedTotal,
		"duration", time.Since(start))
	logDedupStats(c.logger)

	return nil
}

// logDedupStats logs the cumulative dedup call counters (grep keyword
// memory_dedup_stats; cut-off ratio = truncated_total / calls_total).
func logDedupStats(logger *zap.SugaredLogger) {
	calls, trunc := memory.DedupCallStats()
	ratio := 0.0
	if calls > 0 {
		ratio = float64(trunc) / float64(calls)
	}
	logger.Infow("memory_dedup_stats", "calls_total", calls, "truncated_total", trunc, "truncated_ratio", ratio)
}

// vecNeighbours returns the active entries most similar to seed (memory_vec
// top-k, filtered by NeighbourMinSimilarity, best first). Mirror copies are
// excluded.
func (c *SQLiteCompactor) vecNeighbours(ctx context.Context, scope memory.Scope, vec *memory.VecIndex, seed memory.Entry) []memory.Entry {
	hits := vec.Search(memory.VectorScopeKey(seed.Scope, seed.Metadata), seed.Content, c.sel.topK+1)
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.EntryID != seed.ID && !memory.IsMirrorID(h.EntryID) {
			ids = append(ids, h.EntryID)
		}
	}
	cands, err := c.repo.ActiveByIDs(ctx, scope, ids)
	if err != nil {
		return nil
	}
	small := cands[:0]
	for _, e := range cands {
		if !c.oversized(e) {
			small = append(small, e)
		}
	}
	return filterNeighbours(seed, small, c.sel.minSim, c.sel.topK)
}

// oversized reports whether an entry is too long to take part in a merge.
func (c *SQLiteCompactor) oversized(e memory.Entry) bool {
	return len([]rune(e.Content)) > c.sel.maxMergedChars
}

// mergeRejectReason returns why a merge result must not be written ("" = ok):
// longer than MaxMergedChars, or nearly identical to one of its sources (the
// merge would only replace a source by a copy of itself).
func (c *SQLiteCompactor) mergeRejectReason(cl memory.ClusterResult, byID map[string]memory.Entry) string {
	if len([]rune(cl.MergedContent)) > c.sel.maxMergedChars {
		return "too_long"
	}
	for _, id := range cl.SourceIDs {
		if src, ok := byID[id]; ok && memory.EmbedSimilarity(cl.MergedContent, src.Content) >= c.sel.noopSim {
			return "noop_similar_to_source"
		}
	}
	return ""
}

// compactBatch 对一批活跃记忆执行 LLM 聚类合并 + 归档来源。
// 单批失败（如 LLM 抖动）不影响其他批次，返回已合并/已归档计数。
//
// done lists the entries that went through the LLM without being merged (to be
// marked dedup-checked); nil after a transient failure so they are retried.
func (c *SQLiteCompactor) compactBatch(ctx context.Context, scope memory.Scope, entries []memory.Entry) (merged, archived, saved int, done []string) {
	if len(entries) < c.config.MinClusterSize {
		return 0, 0, 0, nil
	}

	// 压缩是尽力而为的后台维护：聚类合并要调 GLM，可能耗时数秒到数十秒。
	// 调用方传入的 ctx 通常来自请求链路、带较短 deadline，直接透传会导致
	// "context deadline exceeded"（见日志 sqlite_compactor LLM cluster+merge
	// failed）。这里派生一个「去掉请求 deadline、但保留 traceid 等上下文值」
	// 的 ctx（context.WithoutCancel 仅剥离父 ctx 的 deadline/cancel，保留值），
	// 再包一层独立长超时用于控制。服务关闭时父 ctx 取消防不住本批（best-effort），
	// 但仍受 compactLLMTimeout 上限约束，不会无限挂起。
	batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compactLLMTimeout)
	defer cancel()

	inputs := make([]memory.ClusterInput, 0, len(entries))
	for _, e := range entries {
		pc := memory.PreprocessContent(e.Content, c.config.Precompress)
		if c.config.Precompress {
			saved += llm.EstimateTokens(e.Content) - llm.EstimateTokens(pc)
		}
		inputs = append(inputs, memory.ClusterInput{
			ID:       e.ID,
			Category: e.Category,
			Content:  pc,
		})
	}
	clusters, err := memory.ClusterMerge(batchCtx, c.config.Provider, c.config.Model, c.config.SystemPrompt, inputs, c.config.MaxTokens, c.config.Policy)
	if err != nil {
		if errors.Is(err, memory.ErrDedupTruncated) {
			// 输出被上限截断：同组重试大概率再次截断，标记已检查避免每轮冷却后
			// 反复烧 token；条目保持活跃，新条目到来时仍可作为近邻重新入组。
			c.logger.Warnw("sqlite_compactor: dedup answer cut off by the output limit, marking group checked (raise llm.internal_max_tokens.memory_dedup to retry)",
				"scope", scope.Key(), "entry_ids", entryIDs(entries))
			return 0, 0, saved, entryIDs(entries)
		}
		if isDeterministicLLMReject(err) {
			// 内容安全审核 / 请求参数非法等「确定性失败」：相同内容重试必再次被拒。
			// 标记这批条目跳过压缩，打破「每轮冷却后重新取出 → 再次被拒」的死循环，
			// 避免持续浪费 BigModel 调用并刷 WARN 噪音。可恢复的瞬时限流 / 网络抖动
			// 不标记，仍走冷却后重试路径。
			skipped := c.repo.MarkCompactSkipped(batchCtx, scope, entries, err.Error())
			c.logger.Warnw("sqlite_compactor: LLM cluster+merge rejected (deterministic), marking entries skipped",
				"err", err, "batch_size", len(entries), "skipped", skipped)
		} else {
			c.logger.Warnw("sqlite_compactor: LLM cluster+merge failed, skipping batch",
				"err", err, "batch_size", len(entries))
		}
		return 0, 0, 0, nil
	}
	if len(clusters) == 0 {
		return 0, 0, saved, entryIDs(entries) // 无可合并项：全部标记已检查
	}

	mergedIDs := make(map[string]bool)
	mergedCount, archivedCount := 0, 0
	byID := make(map[string]memory.Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, cl := range clusters {
		if why := c.mergeRejectReason(cl, byID); why != "" {
			c.logger.Infow("memory_dedup: merge rejected, sources kept",
				"scope", scope.Key(), "source_ids", cl.SourceIDs, "reason", why,
				"merged_chars", len([]rune(cl.MergedContent)))
			continue
		}
		meta := map[string]any{
			"compacted_at": time.Now(),
			"source_ids":   cl.SourceIDs,
			"source_count": len(cl.SourceIDs),
		}
		merged := memory.Entry{
			ID:         idgen.New("mem"),
			Scope:      scope,
			Content:    cl.MergedContent,
			Category:   cl.Category,
			Source:     "compactor",
			Importance: cl.Importance,
			Metadata:   meta,
		}
		if err := c.repo.Append(batchCtx, merged); err != nil {
			c.logger.Warnw("sqlite_compactor: failed to write merged entry, skipping cluster",
				"err", err, "source_ids", cl.SourceIDs)
			continue
		}
		for _, id := range cl.SourceIDs {
			mergedIDs[id] = true
		}
		mergedCount++
		// 审计 / 恢复：每次合并都记录来源与结果（来源条目被归档而非删除，可据此回滚）。
		c.logger.Infow("memory_dedup: merged",
			"scope", scope.Key(), "source_ids", cl.SourceIDs, "merged_id", merged.ID)
	}
	for _, e := range entries {
		if !mergedIDs[e.ID] {
			continue
		}
		if c.repo.ArchiveByID(batchCtx, scope, e.ID) {
			archivedCount++
		}
	}

	for _, e := range entries {
		if !mergedIDs[e.ID] {
			done = append(done, e.ID)
		}
	}
	return mergedCount, archivedCount, saved, done
}

func entryIDs(entries []memory.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// isDeterministicLLMReject 判断 LLM 调用是否返回「确定性失败」——即相同输入重试
// 必然再次失败（内容安全审核 / 请求参数非法等），而非可恢复的瞬时限流或网络抖动。
// 用于压缩死循环熔断：确定性失败时标记条目跳过，而非放入「冷却后重试」的死循环。
//
// 采用字符串宽松匹配而非结构化类型：压缩 LLM 调用经 provider 多层包装后错误已退化为
// 纯文本，errors.As 拿不到结构化类型（与 workflow/retry_classify.go 的「Loose」哲学一致）。
// 收录标准严格限定于确定性特征，任何可能表达「瞬时可恢复」的措辞都不得加入，否则会把
// 可恢复故障误判为终态而放弃压缩。
func isDeterministicLLMReject(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	deterministic := []string{
		`"code":"1301"`, `"code": "1301"`, "内容安全审核", "触发平台内容",
		`"code":"1210"`, `"code": "1210"`, "api 调用参数有误",
		`"code":"1214"`, `"code": "1214"`, "messages 参数非法",
		"contentfilter", "content_filter",
	}
	for _, p := range deterministic {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
