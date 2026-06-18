package memory

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// ============================================================================
// Expander — 记忆展开器
// ============================================================================

// Expander 提供按 Entry ID 回溯加载原始记忆内容的能力。
//
// 当 LLM 在压缩摘要中看到 [ref:ID] 引用，需要详细信息时，
// 可通过 Expander 加载原文到当前上下文。
//
// 典型使用场景：
//   - LLM 发现摘要中的某条记忆需要展开了解细节
//   - Tool call: "expand_memory" → 返回原文
//   - 构建 follow-up prompt 时自动展开引用
//
// Expander 可作为 LLM Tool 暴露，也可被 Stage 直接调用。
type Expander struct {
	retriever Retriever
	builder   *ContextBuilder
	tracer    trace.Tracer
	logger    *zap.SugaredLogger
}

// NewExpander 创建记忆展开器。
func NewExpander(
	retriever Retriever,
	builder *ContextBuilder,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *Expander {
	return &Expander{
		retriever: retriever,
		builder:   builder,
		tracer:    tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/expander"),
		logger:    logger.With("component", "memory_expander"),
	}
}

// ExpandResult 是展开操作的结果。
type ExpandResult struct {
	// Entries 找到的原始记忆条目。
	Entries []Entry `json:"entries"`
	// Formatted 格式化后的展开内容（供 LLM 消费）。
	Formatted string `json:"formatted"`
	// NotFound 未找到的 Entry ID 列表。
	NotFound []string `json:"not_found,omitempty"`
	// TokenCount 展开内容的估算 token 数。
	TokenCount int `json:"token_count"`
}

// Expand 根据 Entry ID 列表加载原始记忆内容。
// 返回格式化后的文本，可直接拼入 LLM 的上下文。
//
// 参数：
//   - ids: 要展开的 Entry ID 列表（来自 CompressedBlock.EntryIDs 或 [ref:ID] 引用）
//   - scopes: 搜索范围（为空时搜索所有 scope）
func (e *Expander) Expand(ctx context.Context, ids []string, scopes ...Scope) (*ExpandResult, error) {
	if len(ids) == 0 {
		return &ExpandResult{}, nil
	}

	ctx, span := e.tracer.Start(ctx, "memory.expand",
		trace.WithAttributes(
			attribute.Int("requested_ids", len(ids)),
		))
	defer span.End()

	// 查找所有请求的 Entry
	found, notFound := e.findEntries(ctx, ids, scopes)

	span.SetAttributes(
		attribute.Int("found", len(found)),
		attribute.Int("not_found", len(notFound)),
	)

	if len(found) == 0 {
		e.logger.Debugw("expand: no entries found",
			"requested", len(ids),
			"not_found", notFound)
		return &ExpandResult{
			NotFound: notFound,
		}, nil
	}

	// 格式化展开内容
	formatted := e.formatExpanded(found)
	tokenCount := estimateTokens(formatted)

	e.logger.Debugw("memory expanded",
		"requested", len(ids),
		"found", len(found),
		"not_found", len(notFound),
		"tokens", tokenCount)

	return &ExpandResult{
		Entries:    found,
		Formatted:  formatted,
		NotFound:   notFound,
		TokenCount: tokenCount,
	}, nil
}

// ExpandFromRefs 解析文本中的 [ref:ID] 引用并展开。
// 从压缩摘要中提取所有引用 ID 并批量加载。
func (e *Expander) ExpandFromRefs(ctx context.Context, text string, scopes ...Scope) (*ExpandResult, error) {
	ids := ExtractRefIDs(text)
	if len(ids) == 0 {
		return &ExpandResult{}, nil
	}
	return e.Expand(ctx, ids, scopes...)
}

// findEntries 在 Retriever 中查找指定 ID 的 Entry。
func (e *Expander) findEntries(ctx context.Context, ids []string, scopes []Scope) (found []Entry, notFound []string) {
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}

	// 从 Retriever 获取所有候选记忆
	// 由于当前 Retriever 接口不支持按 ID 批量查询，
	// 我们使用 Recent 获取大范围数据然后过滤。
	// 未来可扩展 Retriever 接口增加 GetByIDs 方法。
	var candidates []Entry

	if len(scopes) == 0 {
		// 搜索所有 scope — 使用空 Query
		all, err := e.retriever.Retrieve(ctx, Query{Limit: 10000})
		if err != nil {
			e.logger.Warnw("expand: retrieval failed", "err", err)
			notFound = ids
			return nil, notFound
		}
		candidates = all
	} else {
		for _, scope := range scopes {
			entries, err := e.retriever.Recent(ctx, scope, 10000)
			if err != nil {
				e.logger.Warnw("expand: scope retrieval failed",
					"scope", scope.Key(), "err", err)
				continue
			}
			candidates = append(candidates, entries...)
		}
	}

	// 过滤匹配 ID
	foundSet := make(map[string]struct{})
	for _, entry := range candidates {
		if _, want := idSet[entry.ID]; want {
			found = append(found, entry)
			foundSet[entry.ID] = struct{}{}
		}
	}

	// 计算未找到的 ID
	for _, id := range ids {
		if _, ok := foundSet[id]; !ok {
			notFound = append(notFound, id)
		}
	}

	return found, notFound
}

// formatExpanded 格式化展开的记忆内容。
func (e *Expander) formatExpanded(entries []Entry) string {
	var sb strings.Builder
	sb.WriteString("[Expanded Memory Details]\n")

	for _, entry := range entries {
		sb.WriteString(fmt.Sprintf("[%s] ", entry.ID))
		if entry.Category != "" {
			sb.WriteString(fmt.Sprintf("(%s) ", entry.Category))
		}
		sb.WriteString(entry.Content)
		sb.WriteByte('\n')
	}

	sb.WriteString("[End Expanded Memory]")
	return sb.String()
}

// ============================================================================
// Ref ID 解析工具
// ============================================================================

// ExtractRefIDs 从文本中提取所有 [ref:ID] 格式的记忆引用 ID。
// 支持格式：[ref:mem-abc123]、[ref:mem-abc123, ref:mem-def456]
func ExtractRefIDs(text string) []string {
	var ids []string
	seen := make(map[string]struct{})

	// 查找所有 [ref:xxx] 模式
	remaining := text
	for {
		idx := strings.Index(remaining, "[ref:")
		if idx < 0 {
			break
		}
		remaining = remaining[idx+5:] // 跳过 "[ref:"

		// 找到结束位置（] 或 , 或空格）
		end := strings.IndexAny(remaining, "],; ")
		if end < 0 {
			end = len(remaining)
		}

		id := strings.TrimSpace(remaining[:end])
		if id != "" {
			if _, ok := seen[id]; !ok {
				ids = append(ids, id)
				seen[id] = struct{}{}
			}
		}

		if end < len(remaining) {
			remaining = remaining[end:]
		} else {
			break
		}
	}

	return ids
}

// ============================================================================
// Retriever 扩展接口（按 ID 批量查询）
// ============================================================================

// IDRetriever 是 Retriever 的可选扩展，支持按 ID 批量查询。
// 如果底层实现支持此接口，Expander 会优先使用它（更高效）。
type IDRetriever interface {
	// GetByIDs 根据 ID 列表批量获取记忆条目。
	// 返回找到的条目列表（可能少于请求数量）。
	GetByIDs(ctx context.Context, ids []string) ([]Entry, error)
}
