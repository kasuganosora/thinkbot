package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Compressor — 上下文压缩器
// ============================================================================

// Compressor 定义上下文压缩能力。
// 当记忆内容超过窗口限制时，Compressor 将溢出部分进行总结压缩。
//
// 压缩产生 CompressedBlock：
//   - Summary: 压缩后的摘要文本
//   - EntryIDs: 被压缩的原始 Entry ID 列表（用于回溯原文）
//
// LLM 在需要详细信息时，可通过 EntryIDs 调用 Expander 加载原文。
type Compressor interface {
	// Compress 将一组记忆条目压缩为摘要。
	// 输入为超出窗口限制的记忆条目列表。
	// 输出为压缩后的块（含摘要 + 原始 ID 引用）。
	Compress(ctx context.Context, entries []Entry) (*CompressedBlock, error)
}

// CompressedBlock 表示一次压缩的结果。
// 它是 Memory 系统的核心产出物之一，连接"摘要"和"原文"。
type CompressedBlock struct {
	// ID 压缩块唯一标识。
	ID string `json:"id"`
	// Summary 压缩后的摘要文本（供 LLM 直接消费）。
	Summary string `json:"summary"`
	// EntryIDs 被压缩的原始 Entry ID 列表。
	// LLM 可通过这些 ID 回溯加载原文到上下文。
	EntryIDs []string `json:"entry_ids"`
	// TokenCount 摘要的估算 token 数。
	TokenCount int `json:"token_count"`
	// OriginalTokenCount 压缩前原始内容的估算 token 数。
	OriginalTokenCount int `json:"original_token_count"`
	// CompressionRatio 压缩比（Summary tokens / Original tokens）。
	CompressionRatio float64 `json:"compression_ratio"`
	// CreatedAt 压缩时间。
	CreatedAt time.Time `json:"created_at"`
	// Metadata 扩展元数据。
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ============================================================================
// LLMCompressor — 基于 LLM 的压缩器实现
// ============================================================================

// LLMCompressorConfig 配置 LLM 压缩器。
type LLMCompressorConfig struct {
	// Provider LLM 提供商（用于生成摘要）。
	// 建议使用轻量级/低成本模型做压缩。
	Provider llm.Provider
	// Model 指定用于压缩的模型（可选，为空则使用 Provider 默认）。
	Model *llm.Model
	// SystemPrompt 压缩用的系统提示词。
	// 为空时使用内置默认 prompt。
	SystemPrompt string
	// MaxSummaryTokens 摘要的最大 token 数。
	// 默认 500。
	MaxSummaryTokens int
	// TargetRatio 目标压缩比（默认 0.3，即压缩为原文的 30%）。
	TargetRatio float64
}

// DefaultLLMCompressorConfig 返回默认配置（需调用方注入 Provider）。
func DefaultLLMCompressorConfig() LLMCompressorConfig {
	return LLMCompressorConfig{
		MaxSummaryTokens: 500,
		TargetRatio:      0.3,
	}
}

// LLMCompressor 使用 LLM 对记忆进行摘要压缩。
// 它在超出窗口时被 ContextManager 调用，产出 CompressedBlock。
//
// 工作流程：
//  1. 收到待压缩的 Entry 列表
//  2. 构建压缩 prompt（包含所有 Entry 内容 + 它们的 ID）
//  3. 调用 LLM 生成摘要
//  4. 返回 CompressedBlock（摘要 + ID 引用列表）
type LLMCompressor struct {
	config LLMCompressorConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewLLMCompressor 创建 LLM 压缩器。
func NewLLMCompressor(config LLMCompressorConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *LLMCompressor {
	if config.MaxSummaryTokens <= 0 {
		config.MaxSummaryTokens = 500
	}
	if config.TargetRatio <= 0 || config.TargetRatio > 1.0 {
		config.TargetRatio = 0.3
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaultCompressPrompt
	}

	return &LLMCompressor{
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/compressor"),
		logger: logger.With("component", "memory_compressor"),
	}
}

// Compress 将一组记忆条目压缩为摘要。
func (c *LLMCompressor) Compress(ctx context.Context, entries []Entry) (*CompressedBlock, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("compressor: nothing to compress")
	}

	ctx, span := c.tracer.Start(ctx, "memory.compress",
		trace.WithAttributes(
			attribute.Int("entries_count", len(entries)),
		))
	defer span.End()

	// 构建压缩 prompt
	content, entryIDs, originalTokens := c.buildCompressContent(entries)

	c.logger.Debugw("compressing memories",
		"entries", len(entries),
		"original_tokens_est", originalTokens)

	// 调用 LLM 生成摘要
	maxTokens := c.config.MaxSummaryTokens
	params := llm.GenerateParams{
		Model:     c.config.Model,
		System:    c.config.SystemPrompt,
		Messages:  []llm.Message{llm.UserMessage(content)},
		MaxTokens: &maxTokens,
	}

	result, err := c.config.Provider.DoGenerate(ctx, params)
	if err != nil {
		span.RecordError(err)
		c.logger.Errorw("compression LLM call failed", "err", err)
		return nil, errs.Wrap(err, "compressor: LLM generation failed")
	}

	summary := strings.TrimSpace(result.Text)
	summaryTokens := estimateTokens(summary)
	ratio := float64(summaryTokens) / float64(max(originalTokens, 1))

	span.SetAttributes(
		attribute.Int("summary_tokens", summaryTokens),
		attribute.Int("original_tokens", originalTokens),
		attribute.Float64("compression_ratio", ratio),
		attribute.Int("llm_input_tokens", result.Usage.InputTokens),
		attribute.Int("llm_output_tokens", result.Usage.OutputTokens),
	)

	block := &CompressedBlock{
		ID:                 idgen.New("mem"), // 复用统一的 ID 生成
		Summary:            summary,
		EntryIDs:           entryIDs,
		TokenCount:         summaryTokens,
		OriginalTokenCount: originalTokens,
		CompressionRatio:   ratio,
		CreatedAt:          time.Now(),
		Metadata: map[string]any{
			"llm_provider":     c.config.Provider.Name(),
			"llm_input_tokens": result.Usage.InputTokens,
			"llm_output_tokens": result.Usage.OutputTokens,
		},
	}

	c.logger.Infow("compression complete",
		"entries", len(entries),
		"original_tokens", originalTokens,
		"summary_tokens", summaryTokens,
		"ratio", fmt.Sprintf("%.2f", ratio))

	return block, nil
}

// buildCompressContent 构建发送给 LLM 的压缩请求内容。
// 包含每条 Entry 的 ID 和内容，让 LLM 在摘要中保留 ID 引用。
func (c *LLMCompressor) buildCompressContent(entries []Entry) (content string, entryIDs []string, tokenEstimate int) {
	var sb strings.Builder
	entryIDs = make([]string, 0, len(entries))

	sb.WriteString("以下是需要压缩的记忆条目，每条以 [ID] 标识：\n\n")

	for _, entry := range entries {
		entryIDs = append(entryIDs, entry.ID)
		line := fmt.Sprintf("[%s] (%s) %s\n", entry.ID, entry.Category, entry.Content)
		sb.WriteString(line)
		tokenEstimate += estimateTokens(entry.Content)
	}

	sb.WriteString("\n请生成一份结构化摘要。在摘要中，对每个要点标注其来源 Entry ID（格式：`[ref:ID]`），")
	sb.WriteString("以便后续需要详细信息时可按 ID 回溯原文。")

	return sb.String(), entryIDs, tokenEstimate
}

// estimateTokens 粗略估算文本的 token 数。
// 中文约 1 字 = 1.5 token，英文约 1 word = 1.3 token。
// 这里使用简单的 字符数/3 作为估算（偏保守）。
func estimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	// 中英混合场景，使用 rune 计数更准确
	runeCount := len([]rune(text))
	// 粗略：每 3 个字符 ≈ 1 token（中文偏高，英文偏低，取中间值）
	tokens := (runeCount + 2) / 3
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// ============================================================================
// NoopCompressor — 空实现（禁用压缩时使用）
// ============================================================================

// NoopCompressor 不执行任何压缩操作，直接截断超限内容。
// 用于不配置 LLM Provider 做压缩的场景。
type NoopCompressor struct{}

// Compress 截断内容而非压缩。
// 返回一个简单的 "内容已截断" 摘要。
func (n *NoopCompressor) Compress(_ context.Context, entries []Entry) (*CompressedBlock, error) {
	entryIDs := make([]string, len(entries))
	var totalTokens int
	for i, e := range entries {
		entryIDs[i] = e.ID
		totalTokens += estimateTokens(e.Content)
	}

	return &CompressedBlock{
		ID:                 idgen.New("mem"),
		Summary:            fmt.Sprintf("(已省略 %d 条较早的记忆，可通过 ID 加载原文)", len(entries)),
		EntryIDs:           entryIDs,
		TokenCount:         estimateTokens(fmt.Sprintf("(已省略 %d 条较早的记忆)", len(entries))),
		OriginalTokenCount: totalTokens,
		CompressionRatio:   0,
		CreatedAt:          time.Now(),
	}, nil
}

// ============================================================================
// Default compress prompt
// ============================================================================

const defaultCompressPrompt = `你是一个记忆压缩助手。你的任务是将多条记忆条目压缩为一份简洁的结构化摘要。

规则：
1. 保留关键事实、决策和偏好，删除冗余和过时信息
2. 对每个要点标注来源 Entry ID，格式为 [ref:ID]
3. 按主题分组组织摘要
4. 使用简练的表述，避免冗余
5. 重要度高的内容优先保留
6. 输出纯文本，不要使用 markdown 标题

输出格式示例：
- 用户偏好Go语言开发，使用Gin框架 [ref:mem-abc123]
- 项目使用Bazel构建系统，proto生成Go代码 [ref:mem-def456]
- 已完成用户注册接口的软删除逻辑修复 [ref:mem-ghi789] [ref:mem-jkl012]`


