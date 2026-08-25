package storage

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
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
}

// SQLiteCompactor 对 SQLiteRepository 中的记忆执行语义压缩。
type SQLiteCompactor struct {
	repo   *SQLiteRepository
	config memory.CompactionConfig
	logger *zap.SugaredLogger
}

// NewSQLiteCompactor 创建 SQLite 记忆压缩器。
// repo 在 SetRepository 中注入，以打破 NewSQLiteRepository ↔ NewSQLiteCompactor
// 的构造循环依赖（两者互相引用）。
func NewSQLiteCompactor(cfg SQLiteCompactorConfig, logger *zap.SugaredLogger) *SQLiteCompactor {
	mc := memory.CompactionConfig{
		Provider:    cfg.Provider,
		Model:       cfg.Model,
		Precompress: true, // LLM 摘要前先做确定性瘦身（JSON 紧凑化+must-keep+回退校验）
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
	if cfg.MaxClusterSize >= mc.MinClusterSize {
		mc.MaxClusterSize = cfg.MaxClusterSize
	} else {
		mc.MaxClusterSize = mc.MinClusterSize
	}
	if cfg.MaxInputEntries > 0 {
		mc.MaxInputEntries = cfg.MaxInputEntries
	} else {
		mc.MaxInputEntries = 50
	}
	return &SQLiteCompactor{
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
func (c *SQLiteCompactor) CompactScope(ctx context.Context, scope memory.Scope) error {
	if c.repo == nil {
		return nil
	}
	start := time.Now()

	// 1. 获取该 scope 所有活跃（未归档）记忆
	entries, err := c.repo.GetAllActive(ctx, scope)
	if err != nil {
		return errs.Wrap(err, "sqlite_compactor: get entries")
	}
	if len(entries) < c.config.MinClusterSize {
		return nil
	}

	// 2. 分批循环压缩：无论活跃条目多少都全部处理。
	//    原来超额时只取前 MaxInputEntries 条、其余本轮直接丢弃，导致
	//    GetAllActive 按 created_at ASC 排序下，较新的记忆永远轮不到压缩、
	//    活跃记忆只增不减（见日志 sqlite_compactor exceeds MaxInputEntries）。
	//    现改为按 MaxInputEntries 切块循环处理，单批内独立聚类合并。
	batchSize := c.config.MaxInputEntries
	totalBatches := (len(entries) + batchSize - 1) / batchSize
	if totalBatches > 1 {
		c.logger.Infow("sqlite_compactor: input exceeds MaxInputEntries, processing in batches",
			"total", len(entries), "max", batchSize, "batches", totalBatches)
	}

	mergedTotal, archivedTotal, savedTotal := 0, 0, 0
	for i := 0; i < len(entries); i += batchSize {
		end := i + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		m, a, s := c.compactBatch(ctx, scope, entries[i:end])
		mergedTotal += m
		archivedTotal += a
		savedTotal += s
	}

	c.logger.Infow("sqlite semantic compaction complete",
		"scope", scope.Key(),
		"input", len(entries),
		"batches", totalBatches,
		"merged", mergedTotal,
		"archived", archivedTotal,
		"precompressed_saved", savedTotal,
		"duration", time.Since(start))

	return nil
}

// compactBatch 对一批活跃记忆执行 LLM 聚类合并 + 归档来源。
// 单批失败（如 LLM 抖动）不影响其他批次，返回已合并/已归档计数。
func (c *SQLiteCompactor) compactBatch(ctx context.Context, scope memory.Scope, entries []memory.Entry) (merged, archived, saved int) {
	if len(entries) < c.config.MinClusterSize {
		return 0, 0, 0
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
	clusters, err := memory.ClusterMerge(batchCtx, c.config.Provider, c.config.Model, c.config.SystemPrompt, inputs)
	if err != nil {
		c.logger.Warnw("sqlite_compactor: LLM cluster+merge failed, skipping batch",
			"err", err, "batch_size", len(entries))
		return 0, 0, 0
	}
	if len(clusters) == 0 {
		return 0, 0, 0 // 无可合并项
	}

	mergedIDs := make(map[string]bool)
	mergedCount, archivedCount := 0, 0
	for _, cl := range clusters {
		meta := map[string]any{
			"compacted_at": time.Now(),
			"source_ids":   cl.SourceIDs,
			"source_count": len(cl.SourceIDs),
		}
		merged := memory.Entry{
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
	}
	for _, e := range entries {
		if !mergedIDs[e.ID] {
			continue
		}
		if c.repo.ArchiveByID(batchCtx, scope, e.ID) {
			archivedCount++
		}
	}

	return mergedCount, archivedCount, saved
}
