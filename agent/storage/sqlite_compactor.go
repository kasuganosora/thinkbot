package storage

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
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
		Provider: cfg.Provider,
		Model:    cfg.Model,
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
	if len(entries) > c.config.MaxInputEntries {
		c.logger.Warnw("sqlite_compactor: input exceeds MaxInputEntries, excess entries will not be processed this run",
			"total", len(entries), "max", c.config.MaxInputEntries)
		entries = entries[:c.config.MaxInputEntries]
	}

	// 2. LLM 聚类合并
	inputs := make([]memory.ClusterInput, 0, len(entries))
	for _, e := range entries {
		inputs = append(inputs, memory.ClusterInput{
			ID:       e.ID,
			Category: e.Category,
			Content:  e.Content,
		})
	}
	clusters, err := memory.ClusterMerge(ctx, c.config.Provider, c.config.Model, c.config.SystemPrompt, inputs)
	if err != nil {
		return errs.Wrap(err, "sqlite_compactor: LLM cluster+merge")
	}
	if len(clusters) == 0 {
		return nil // 无可合并项
	}

	// 3. 写入合并条目 + 归档原始来源
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
		if err := c.repo.Append(ctx, merged); err != nil {
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
		if c.repo.ArchiveByID(ctx, scope, e.ID) {
			archivedCount++
		}
	}

	c.logger.Infow("sqlite semantic compaction complete",
		"scope", scope.Key(),
		"input", len(entries),
		"clusters", len(clusters),
		"merged", mergedCount,
		"archived", archivedCount,
		"duration", time.Since(start))

	return nil
}
