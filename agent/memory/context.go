package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// ContextBuilder — 上下文组装器
// ============================================================================

// ContextBuilder 负责将检索到的记忆格式化为 LLM 可消费的上下文文本。
// 它是 Memory → LLM 的桥梁。
//
// 典型使用流程：
//  1. MemoryStage 从 Retriever 检索相关记忆
//  2. ContextBuilder 将记忆格式化为 context string
//  3. 格式化后的 context 注入到 Envelope KV 中（key: "memory.context"）
//  4. 下游 Stage（如 ReplyStage）在构建 LLM messages 时使用此 context
//
// ContextBuilder 是无状态的，可安全并发使用。
type ContextBuilder struct {
	config ContextBuilderConfig
}

// ContextBuilderConfig 配置上下文组装器。
type ContextBuilderConfig struct {
	// MaxTokenEstimate 上下文最大 token 估算值（默认 2000）。
	// 超过此值时截断。使用简单的 字符数/4 估算。
	MaxTokenEstimate int
	// Header 上下文块的头部标记（用于 LLM 识别）。
	Header string
	// Footer 上下文块的尾部标记。
	Footer string
	// EntryFormat 单条记忆的格式化模板。
	// 支持占位符：{content}, {category}, {time}, {scope}
	// 为空时使用默认格式。
	EntryFormat string
	// IncludeMetadata 是否在格式化中包含 metadata。
	IncludeMetadata bool
	// TimestampFormat 时间戳格式。为空时使用相对时间（"2 hours ago"）。
	TimestampFormat string
}

// DefaultContextBuilderConfig 返回默认配置。
func DefaultContextBuilderConfig() ContextBuilderConfig {
	return ContextBuilderConfig{
		MaxTokenEstimate: 2000,
		Header:           "[Memory Context]",
		Footer:           "[End Memory Context]",
	}
}

// NewContextBuilder 创建上下文组装器。
func NewContextBuilder(opts ...ContextBuilderConfig) *ContextBuilder {
	cfg := DefaultContextBuilderConfig()
	if len(opts) > 0 {
		if opts[0].MaxTokenEstimate > 0 {
			cfg.MaxTokenEstimate = opts[0].MaxTokenEstimate
		}
		if opts[0].Header != "" {
			cfg.Header = opts[0].Header
		}
		if opts[0].Footer != "" {
			cfg.Footer = opts[0].Footer
		}
		if opts[0].EntryFormat != "" {
			cfg.EntryFormat = opts[0].EntryFormat
		}
		cfg.IncludeMetadata = opts[0].IncludeMetadata
		if opts[0].TimestampFormat != "" {
			cfg.TimestampFormat = opts[0].TimestampFormat
		}
	}
	return &ContextBuilder{config: cfg}
}

// Build 将记忆条目组装为 LLM 上下文文本。
// 返回格式化后的文本，可直接拼入 system prompt 或作为 context message。
func (b *ContextBuilder) Build(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(b.config.Header)
	sb.WriteByte('\n')

	maxChars := b.config.MaxTokenEstimate * 4 // 粗略估算：1 token ≈ 4 chars
	currentLen := len(b.config.Header) + len(b.config.Footer) + 2

	for _, entry := range entries {
		line := b.formatEntry(entry)
		lineLen := len(line) + 1 // +1 for newline

		if currentLen+lineLen > maxChars {
			sb.WriteString("... (more memories truncated)\n")
			break
		}

		sb.WriteString(line)
		sb.WriteByte('\n')
		currentLen += lineLen
	}

	sb.WriteString(b.config.Footer)
	return sb.String()
}

// formatEntry 格式化单条记忆条目。
func (b *ContextBuilder) formatEntry(entry Entry) string {
	if b.config.EntryFormat != "" {
		return b.applyTemplate(entry)
	}

	// 默认格式
	var sb strings.Builder
	sb.WriteString("- ")

	// 时间标记
	timeStr := b.formatTime(entry.CreatedAt)
	if timeStr != "" {
		sb.WriteString("[")
		sb.WriteString(timeStr)
		sb.WriteString("] ")
	}

	// 分类标签
	if entry.Category != "" {
		sb.WriteString("(")
		sb.WriteString(entry.Category)
		sb.WriteString(") ")
	}

	// 内容
	sb.WriteString(entry.Content)

	return sb.String()
}

// applyTemplate 应用自定义模板。
func (b *ContextBuilder) applyTemplate(entry Entry) string {
	s := b.config.EntryFormat
	s = strings.ReplaceAll(s, "{content}", entry.Content)
	s = strings.ReplaceAll(s, "{category}", entry.Category)
	s = strings.ReplaceAll(s, "{time}", b.formatTime(entry.CreatedAt))
	s = strings.ReplaceAll(s, "{scope}", entry.Scope.Key())
	return s
}

// formatTime 格式化时间。
func (b *ContextBuilder) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if b.config.TimestampFormat != "" {
		return t.Format(b.config.TimestampFormat)
	}
	// 默认使用相对时间
	return relativeTime(t)
}

// relativeTime 返回相对时间描述。
func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd ago", days)
	default:
		return t.Format("2006-01-02")
	}
}

// ============================================================================
// ContextManager — 上下文管理器（协调 Retriever + ContextBuilder）
// ============================================================================

// ContextManager 协调记忆检索和上下文组装的完整流程。
// 它是 Memory 模块对外的高层 API，封装了：
//   - 根据当前消息确定检索 scope
//   - 调用 Retriever 获取相关记忆
//   - 通过 ContextBuilder 格式化为 LLM context
//
// ContextManager 是线程安全的。
type ContextManager struct {
	retriever Retriever
	builder   *ContextBuilder
	config    ContextManagerConfig
}

// ContextManagerConfig 配置上下文管理器。
type ContextManagerConfig struct {
	// RecentLimit 检索最近对话记忆的条数（默认 5）。
	RecentLimit int
	// RelevantLimit 检索相关记忆的条数（默认 5）。
	RelevantLimit int
	// Scopes 检索时使用的 scope 策略。
	// 为空时默认使用 [ChannelScope(msg.Channel), UserScope(msg.UserID)]。
	Scopes []ScopeKind
}

// DefaultContextManagerConfig 返回默认配置。
func DefaultContextManagerConfig() ContextManagerConfig {
	return ContextManagerConfig{
		RecentLimit:   5,
		RelevantLimit: 5,
		Scopes:        []ScopeKind{ScopeChannel, ScopeUser},
	}
}

// NewContextManager 创建上下文管理器。
func NewContextManager(retriever Retriever, builder *ContextBuilder, opts ...ContextManagerConfig) *ContextManager {
	cfg := DefaultContextManagerConfig()
	if len(opts) > 0 {
		if opts[0].RecentLimit > 0 {
			cfg.RecentLimit = opts[0].RecentLimit
		}
		if opts[0].RelevantLimit > 0 {
			cfg.RelevantLimit = opts[0].RelevantLimit
		}
		if len(opts[0].Scopes) > 0 {
			cfg.Scopes = opts[0].Scopes
		}
	}
	return &ContextManager{
		retriever: retriever,
		builder:   builder,
		config:    cfg,
	}
}

// AssembleContext 根据消息上下文组装 LLM 记忆上下文。
// 参数：
//   - channelID: 当前会话 channel
//   - userID: 当前用户
//   - text: 当前消息文本（用于相关性检索）
//
// 返回格式化后的 context 文本（可直接拼入 prompt）。
// 无记忆时返回空字符串。
func (m *ContextManager) AssembleContext(ctx context.Context, channelID, userID, text string) (string, error) {
	// 1. 确定检索 scope
	scopes := m.resolveScopes(channelID, userID)

	// 2. 获取最近对话记忆
	var allEntries []Entry

	for _, scope := range scopes {
		recent, err := m.retriever.Recent(ctx, scope, m.config.RecentLimit)
		if err != nil {
			return "", fmt.Errorf("memory context: recent retrieval failed for scope %s: %w", scope.Key(), err)
		}
		allEntries = append(allEntries, recent...)
	}

	// 3. 如果有消息文本，做相关性检索（与 recent 去重）
	if text != "" {
		relevant, err := m.retriever.Retrieve(ctx, Query{
			Scopes: scopes,
			Text:   text,
			Limit:  m.config.RelevantLimit,
		})
		if err != nil {
			return "", fmt.Errorf("memory context: relevant retrieval failed: %w", err)
		}
		allEntries = dedup(allEntries, relevant)
	}

	if len(allEntries) == 0 {
		return "", nil
	}

	// 4. 格式化为 LLM context
	return m.builder.Build(allEntries), nil
}

// resolveScopes 根据 channelID 和 userID 确定检索 scope。
func (m *ContextManager) resolveScopes(channelID, userID string) []Scope {
	var scopes []Scope
	for _, kind := range m.config.Scopes {
		switch kind {
		case ScopeChannel:
			if channelID != "" {
				scopes = append(scopes, ChannelScope(channelID))
			}
		case ScopeUser:
			if userID != "" {
				scopes = append(scopes, UserScope(userID))
			}
		case ScopeBot:
			// Bot scope 需要 botID，在 MemoryStage 中传入
			// 这里先跳过，由调用方自行添加
		case ScopeGlobal:
			scopes = append(scopes, GlobalScope())
		}
	}
	return scopes
}

// dedup 合并 existing 和 additional，去除重复 ID。
func dedup(existing, additional []Entry) []Entry {
	seen := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		seen[e.ID] = struct{}{}
	}
	result := existing
	for _, e := range additional {
		if _, ok := seen[e.ID]; !ok {
			result = append(result, e)
			seen[e.ID] = struct{}{}
		}
	}
	return result
}
