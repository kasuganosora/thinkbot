package memory

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// SemanticCompactor — 语义记忆压缩器
//
// 参考 Memoh 的 CompactWithLLM：
//   - LLM 将多条相似记忆聚类合并为信息密集的摘要
//   - 旧记忆归档（archived）而非删除
//   - 保留可追溯的来源 ID 引用
//
// 与 Compressor 的区别：
//   - Compressor 是上下文窗口超限时的临时压缩（生成 CompressedBlock）
//   - SemanticCompactor 是持久化的记忆整理（合并相似条目，减少冗余）
//   - SemanticCompactor 的结果直接替换 L1 中的条目
// ============================================================================

// CompactionConfig 配置语义压缩器。
type CompactionConfig struct {
	// Provider LLM 提供商。
	Provider llm.Provider
	// Model 指定模型（建议轻量模型）。
	Model *llm.Model
	// SystemPrompt 压缩用的系统提示词。
	SystemPrompt string
	// SimilarityThreshold 相似度阈值（0~1，默认 0.6）。
	// 当前由 LLM 自主决定聚类边界，此值作为 LLM prompt 的参考参数。
	// 未来可用于后处理阶段校验 LLM 输出的聚类质量。
	SimilarityThreshold float64
	// MinClusterSize 最小聚类大小（默认 2，至少 2 条才合并）。
	MinClusterSize int
	// MaxClusterSize 最大聚类大小（默认 10）。
	MaxClusterSize int
	// MaxInputEntries 单次压缩的最大输入条目数（默认 50）。
	MaxInputEntries int
	// Precompress 在调用 LLM 摘要前先做确定性瘦身（JSON 紧凑化、must-keep
	// 保护、回退校验），降低喂给 LLM 的 token 量。默认开启；回退校验保证
	// 不会因压缩失灵而退化。对应 headroom 的传输层压缩前置思路。
	Precompress bool
}

// DefaultCompactionConfig 返回默认配置。
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		SystemPrompt:        defaultCompactionPrompt,
		SimilarityThreshold: 0.6,
		MinClusterSize:      2,
		MaxClusterSize:      10,
		MaxInputEntries:     50,
		Precompress:         true,
	}
}

// ClusterResult 聚类合并结果。
type ClusterResult struct {
	// MergedContent 合并后的内容。
	MergedContent string `json:"merged_content"`
	// Category 合并后的分类。
	Category string `json:"category,omitempty"`
	// Importance 合并后的重要度（取最高）。
	Importance float64 `json:"importance,omitempty"`
	// SourceIDs 合并来源的原始条目 ID 列表。
	SourceIDs []string `json:"source_ids"`
}

// CompactionReport 压缩报告。
type CompactionReport struct {
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	InputCount    int       `json:"input_count"`
	ClustersFound int       `json:"clusters_found"`
	MergedCount   int       `json:"merged_count"`
	ArchivedCount int       `json:"archived_count"`
	ReducedCount  int       `json:"reduced_count"` // 减少的条目数
	// PrecompressedSaved 前置压缩层（LLM 摘要前）节省的估算 token 数。
	PrecompressedSaved int           `json:"precompressed_saved"`
	Duration           time.Duration `json:"-"`
}

// SemanticCompactor 语义记忆压缩器。
type SemanticCompactor struct {
	config CompactionConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewSemanticCompactor 创建语义压缩器。
func NewSemanticCompactor(config CompactionConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *SemanticCompactor {
	if config.Provider == nil {
		panic("compactor: config.Provider must not be nil")
	}
	if config.SimilarityThreshold <= 0 {
		config.SimilarityThreshold = 0.6
	}
	if config.MinClusterSize < 2 {
		config.MinClusterSize = 2
	}
	if config.MaxClusterSize < config.MinClusterSize {
		config.MaxClusterSize = config.MinClusterSize
	}
	if config.MaxInputEntries <= 0 {
		config.MaxInputEntries = 50
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaultCompactionPrompt
	}
	return &SemanticCompactor{
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/semantic_compactor"),
		logger: logger.With("component", "semantic_compactor"),
	}
}

// Compact 对指定 scope 的 L1 记忆执行语义压缩。
//
// 流程：
//  1. 获取该 scope 的所有 L1 记忆
//  2. LLM 聚类 + 合并相似条目
//  3. 写入合并后的新条目
//  4. 将原始条目标记为 archived（而非删除）
//
// 参数：
//   - store: 分层存储
//   - scope: 目标作用域
func (c *SemanticCompactor) Compact(
	ctx context.Context,
	store *TieredStore,
	scope Scope,
) (*CompactionReport, error) {
	start := time.Now()
	report := &CompactionReport{StartedAt: start}

	ctx, span := c.tracer.Start(ctx, "memory.semantic_compact",
		trace.WithAttributes(attribute.String("scope", scope.Key())))
	defer span.End()

	// 1. 获取 L1 记忆
	entries, err := store.GetAll(ctx, Tier1LongTerm, scope)
	if err != nil {
		return report, errs.Wrap(err, "compactor: get L1 entries")
	}

	// 过滤已归档的
	var active []TieredEntry
	for _, e := range entries {
		if isArchived(e.Metadata) {
			continue
		}
		active = append(active, e)
	}

	report.InputCount = len(active)
	if len(active) < c.config.MinClusterSize {
		report.FinishedAt = time.Now()
		report.Duration = time.Since(start)
		return report, nil
	}

	// 分批循环压缩：超 MaxInputEntries 时切块，避免超额条目被丢弃导致记忆只增不减。
	// （与 SQLiteCompactor 同源的缺陷：原实现超额即截断、其余本轮不处理。）
	batchSize := c.config.MaxInputEntries
	if batchSize < c.config.MinClusterSize {
		batchSize = c.config.MinClusterSize
	}
	totalBatches := (len(active) + batchSize - 1) / batchSize
	if totalBatches > 1 {
		c.logger.Infow("compactor: input exceeds MaxInputEntries, processing in batches",
			"total", len(active), "max", c.config.MaxInputEntries, "batches", totalBatches)
	}

	for i := 0; i < len(active); i += batchSize {
		end := i + batchSize
		if end > len(active) {
			end = len(active)
		}
		batch := active[i:end]

		// LLM 摘要前的确定性前置压缩：JSON 紧凑化 + must-keep 保护 + 回退校验。
		// 让 GLM 吃更少、更聚焦的输入，降低 token 成本与延迟。
		pre := c.preprocessBatch(batch)
		for j := range batch {
			if j < len(pre) {
				report.PrecompressedSaved += llm.EstimateTokens(batch[j].Content) - llm.EstimateTokens(pre[j].Content)
			}
		}

		// LLM 聚类合并
		clusters, err := c.clusterAndMerge(ctx, pre)
		if err != nil {
			span.RecordError(err)
			c.logger.Warnw("compactor: LLM cluster+merge failed, skipping batch",
				"err", err, "batch_size", len(batch))
			continue
		}
		report.ClustersFound += len(clusters)

		// 应用合并结果
		mergedIDs := make(map[string]bool)
		for _, cluster := range clusters {
			// 写入合并后的新条目（写入失败则跳过该 cluster，不归档源条目）
			meta := map[string]any{
				"compacted_at": time.Now(),
				"source_ids":   cluster.SourceIDs,
				"source_count": len(cluster.SourceIDs),
			}
			if err := store.Append(ctx, TieredEntry{
				Entry: Entry{
					Scope:      scope,
					Content:    cluster.MergedContent,
					Category:   cluster.Category,
					Source:     "compactor",
					Importance: cluster.Importance,
					Metadata:   meta,
				},
				Tier:         Tier1LongTerm,
				PromotedFrom: Tier1LongTerm,
			}); err != nil {
				c.logger.Warnw("compactor: failed to write merged entry, skipping cluster",
					"err", err, "source_ids", cluster.SourceIDs)
				continue
			}

			// 标记原始条目为待归档
			for _, id := range cluster.SourceIDs {
				mergedIDs[id] = true
			}
			report.MergedCount++
		}

		// 归档被合并的原始条目（标记 archived 而非删除）
		for _, e := range batch {
			if !mergedIDs[e.ID] {
				continue
			}
			if c.archiveEntry(ctx, store, scope, e) {
				report.ArchivedCount++
			}
		}
	}

	// 计算净减少量
	report.ReducedCount = report.ArchivedCount - report.MergedCount
	report.FinishedAt = time.Now()
	report.Duration = time.Since(start)

	span.SetAttributes(
		attribute.Int("input", report.InputCount),
		attribute.Int("clusters", report.ClustersFound),
		attribute.Int("precompressed_saved", report.PrecompressedSaved),
		attribute.Int("archived", report.ArchivedCount),
		attribute.Int("reduced", report.ReducedCount),
	)

	c.logger.Infow("semantic compaction complete",
		"scope", scope.Key(),
		"input", report.InputCount,
		"batches", totalBatches,
		"clusters", report.ClustersFound,
		"archived", report.ArchivedCount,
		"reduced", report.ReducedCount,
		"precompressed_saved", report.PrecompressedSaved,
		"duration", report.Duration)

	return report, nil
}

// clusterAndMerge 调用 LLM 对记忆进行聚类合并。
// 复用包级 ClusterMerge，仅在其基础上施加本压缩器的大小约束。
func (c *SemanticCompactor) clusterAndMerge(ctx context.Context, entries []TieredEntry) ([]ClusterResult, error) {
	inputs := make([]ClusterInput, 0, len(entries))
	for _, e := range entries {
		inputs = append(inputs, ClusterInput{
			ID:       e.ID,
			Category: e.Category,
			Content:  e.Content,
		})
	}
	raw, err := ClusterMerge(ctx, c.config.Provider, c.config.Model, c.config.SystemPrompt, inputs)
	if err != nil {
		return nil, err
	}

	// 应用本压缩器的大小约束
	var valid []ClusterResult
	for _, cl := range raw {
		if len(cl.SourceIDs) >= c.config.MinClusterSize {
			if len(cl.SourceIDs) > c.config.MaxClusterSize {
				cl.SourceIDs = cl.SourceIDs[:c.config.MaxClusterSize]
			}
			valid = append(valid, cl)
		}
	}
	return valid, nil
}

// ClusterInput 是语义压缩聚类合并的输入条目。
type ClusterInput struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Content  string `json:"content"`
}

// ClusterMerge 调用 LLM 将相似记忆条目聚类合并为更密集的条目。
// 抽离为包级函数，供 SemanticCompactor（TieredStore）与 SQLiteCompactor
// （SQLiteRepository）复用，避免两套重复的 prompt 构造与 JSON 解析逻辑。
//
// 返回的 cluster 已通过基础合法性校验（至少 2 个来源、合并内容非空、
// 来源数上限 50），调用方可按需再施加自身的大小约束（MinClusterSize/MaxClusterSize）。
// LLM 返回无法解析的 JSON 时返回 (nil, nil)，表示「无可合并项」，不报错。
func ClusterMerge(ctx context.Context, provider llm.Provider, model *llm.Model, systemPrompt string, entries []ClusterInput) ([]ClusterResult, error) {
	var sb strings.Builder
	sb.WriteString("## Long-term memory entries to compact\n\n")
	for _, e := range entries {
		sb.WriteString("[")
		sb.WriteString(e.ID)
		sb.WriteString("] (")
		if e.Category != "" {
			sb.WriteString(e.Category)
		} else {
			sb.WriteString("uncategorized")
		}
		sb.WriteString(") ")
		sb.WriteString(StripThinking(e.Content))
		sb.WriteByte('\n')
	}

	sb.WriteString("\n## Task\n")
	sb.WriteString("Cluster semantically similar entries and merge each cluster into one denser entry.\n")
	sb.WriteString("- Merge near-duplicates into a single entry (several facts about the same topic become one sentence).\n")
	sb.WriteString("- Merge complementary entries (e.g. '用户用Go' + '用户用Gin框架' → '用户使用Go+Gin做后端开发').\n")
	sb.WriteString("- Leave unrelated, standalone entries untouched.\n")
	sb.WriteString("- A cluster MUST contain at least 2 source entries. NEVER emit a cluster of one.\n")
	sb.WriteString("- Write merged_content in the same language as its source entries.\n\n")
	sb.WriteString("Output a JSON array:\n")
	sb.WriteString("```json\n")
	sb.WriteString(`[{"merged_content":"用户使用Go+Gin框架做后端开发","category":"fact","importance":0.8,"source_ids":["mem-1","mem-2"]}]`)
	sb.WriteString("\n```")
	sb.WriteString("\nIf nothing can be merged, output an empty array [] and nothing else.")

	resp, err := provider.DoGenerate(llm.WithStatsFeature(ctx, "memory_dedup"), llm.GenerateParams{
		Model:    model,
		System:   systemPrompt,
		Messages: []llm.Message{llm.UserMessage(sb.String())},
	})
	if err != nil {
		return nil, errs.Wrap(err, "compactor: LLM call")
	}

	var clusters []ClusterResult
	if err := strutil.ExtractJSON(resp.Text, &clusters); err != nil {
		return nil, nil // 解析失败返回空，不报错（与 clusterAndMerge 旧行为一致）
	}

	// 基础合法性校验
	var valid []ClusterResult
	for _, cl := range clusters {
		if len(cl.SourceIDs) >= 2 && strings.TrimSpace(cl.MergedContent) != "" {
			if len(cl.SourceIDs) > 50 {
				cl.SourceIDs = cl.SourceIDs[:50]
			}
			valid = append(valid, cl)
		}
	}

	return valid, nil
}

// isArchived 检查元数据中是否有 archived=true 标记。
func isArchived(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	archived, ok := meta["archived"].(bool)
	return ok && archived
}

// archiveEntry 将一条记忆标记为已归档（而非删除）。返回是否成功归档。
func (c *SemanticCompactor) archiveEntry(ctx context.Context, store *TieredStore, scope Scope, entry TieredEntry) bool {
	// 原子性替换：在单个锁内完成删除旧条目和写入归档后的新条目
	if entry.Metadata == nil {
		entry.Metadata = make(map[string]any)
	}
	entry.Metadata["archived"] = true
	entry.Metadata["archived_at"] = time.Now()
	entry.Metadata["archived_by"] = "compactor"

	if err := store.Replace(ctx, Tier1LongTerm, scope, entry.ID, entry); err != nil {
		c.logger.Warnw("compactor: atomic archive replace failed", "err", err, "id", entry.ID)
		return false
	}
	return true
}

// PurgeArchived 物理删除所有标记为 archived 的记忆。
// 通常在确认压缩结果无误后手动调用。
func (c *SemanticCompactor) PurgeArchived(ctx context.Context, store *TieredStore, scope Scope) (int, error) {
	entries, err := store.GetAll(ctx, Tier1LongTerm, scope)
	if err != nil {
		return 0, errs.Wrap(err, "purge: get entries")
	}

	removed := 0
	for _, e := range entries {
		if !isArchived(e.Metadata) {
			continue
		}
		if err := store.Delete(ctx, Tier1LongTerm, scope, e.ID); err == nil {
			removed++
		}
	}

	c.logger.Infow("purged archived entries",
		"scope", scope.Key(),
		"removed", removed)

	return removed, nil
}

const defaultCompactionPrompt = `You are a memory compactor, a deduplication component that clusters semantically similar memory entries and merges each cluster into a single, denser entry.

Rules:
1. Merge ONLY entries that are genuinely similar. NEVER force unrelated entries together.
2. Preserve every key detail when merging. Losing information is a failure.
3. Each merged_content MUST be one concise, self-contained fact.
4. source_ids MUST list the IDs of every entry folded into that cluster.
5. Output a raw JSON array and nothing else — no prose, no code fences.
6. If nothing can be merged, output an empty array [].
7. Write merged_content in the same language as its source entries.

<example>
Input:
[mem-1] 用户使用 Go 语言
[mem-2] 用户使用 Gin 框架
[mem-3] 用户喜欢猫

Output:
[{"merged_content":"用户使用Go语言和Gin框架进行开发","category":"fact","importance":0.8,"source_ids":["mem-1","mem-2"]}]

mem-3 is unrelated, so it stays standalone and is not emitted.
</example>`
