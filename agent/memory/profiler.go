package memory

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// Profiler — L2→L3 用户画像提取器
//
// 参考 Memoh 的 Profile Extraction 和 TencentDB-Agent-Memory 的 L3 Runner。
//
// Profiler 从 L1（长期记忆）和 L2（场景记忆）中蒸馏出稳定的用户画像，
// 包括人格特征（traits）、稳定事实（facts）和沟通偏好（preferences）。
//
// 画像记忆作为 system prompt 的持久部分注入，利于 LLM prompt cache。
// ============================================================================

// Profiler 定义用户画像提取能力。
type Profiler interface {
	// ExtractProfile 从长期和场景记忆中提取用户画像。
	// 返回提取出的画像条目内容列表。
	ExtractProfile(ctx context.Context, l1Entries, l2Entries []TieredEntry, existing []TieredEntry) ([]ProfileItem, error)
}

// ProfileItem 是提取出的单条画像元素。
type ProfileItem struct {
	// Type 画像元素类型。
	Type string `json:"type"` // "trait" / "fact" / "preference"
	// Content 画像内容。
	Content string `json:"content"`
	// Confidence 置信度（0.0~1.0）。
	Confidence float64 `json:"confidence"`
}

// ============================================================================
// LLMProfiler — 基于 LLM 的画像提取器
// ============================================================================

// LLMProfilerConfig 配置 LLM 画像提取器。
type LLMProfilerConfig struct {
	Provider     llm.Provider
	Model        *llm.Model
	SystemPrompt string
}

// DefaultLLMProfilerConfig 返回默认配置。
func DefaultLLMProfilerConfig() LLMProfilerConfig {
	return LLMProfilerConfig{}
}

// LLMProfiler 使用 LLM 从记忆中提取用户画像。
type LLMProfiler struct {
	config LLMProfilerConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewLLMProfiler 创建 LLM 画像提取器。
func NewLLMProfiler(config LLMProfilerConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *LLMProfiler {
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaultProfilePrompt
	}
	return &LLMProfiler{
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/profiler"),
		logger: logger.With("component", "memory_profiler"),
	}
}

// ExtractProfile 从记忆中提取用户画像。
func (p *LLMProfiler) ExtractProfile(ctx context.Context, l1Entries, l2Entries []TieredEntry, existing []TieredEntry) ([]ProfileItem, error) {
	if len(l1Entries) == 0 && len(l2Entries) == 0 {
		return nil, nil
	}

	ctx, span := p.tracer.Start(ctx, "memory.profile.extract",
		trace.WithAttributes(
			attribute.Int("l1_count", len(l1Entries)),
			attribute.Int("l2_count", len(l2Entries)),
			attribute.Int("existing_count", len(existing)),
		))
	defer span.End()

	prompt := p.buildPrompt(l1Entries, l2Entries, existing)

	result, err := p.config.Provider.DoGenerate(ctx, llm.GenerateParams{
		Model:    p.config.Model,
		System:   p.config.SystemPrompt,
		Messages: []llm.Message{llm.UserMessage(prompt)},
	})
	if err != nil {
		span.RecordError(err)
		return nil, errs.Wrap(err, "profiler: LLM call failed")
	}

	items := p.parseResult(result.Text)
	span.SetAttributes(attribute.Int("profile_items", len(items)))
	p.logger.Debugw("profile extraction complete",
		"l1_input", len(l1Entries),
		"l2_input", len(l2Entries),
		"profile_items", len(items))

	return items, nil
}

func (p *LLMProfiler) buildPrompt(l1, l2, existing []TieredEntry) string {
	var sb strings.Builder

	sb.WriteString("## 长期记忆（L1）\n\n")
	for _, e := range l1 {
		sb.WriteString(fmt.Sprintf("- (%s) %s\n", e.Category, e.Content))
	}

	if len(l2) > 0 {
		sb.WriteString("\n## 场景记忆（L2）\n\n")
		for _, e := range l2 {
			sb.WriteString(fmt.Sprintf("- %s\n", e.Content))
		}
	}

	if len(existing) > 0 {
		sb.WriteString("\n## 已有画像（供参考，不要重复）\n\n")
		for _, e := range existing {
			sb.WriteString(fmt.Sprintf("- %s\n", e.Content))
		}
	}

	sb.WriteString("\n## 任务\n")
	sb.WriteString("从以上记忆中提取稳定的用户画像。输出 JSON 数组：\n")
	sb.WriteString(`[{"type":"trait","content":"...","confidence":0.9}]`)
	sb.WriteString("\ntype: trait(性格特征) / fact(稳定事实) / preference(沟通偏好)\n")
	sb.WriteString("只提取高置信度的稳定特征，忽略一次性行为。\n")

	return sb.String()
}

func (p *LLMProfiler) parseResult(text string) []ProfileItem {
	var items []ProfileItem
	if err := strutil.ExtractJSON(text, &items); err != nil {
		p.logger.Warnw("profiler: failed to parse LLM JSON",
			"err", err,
			"text_preview", strutil.Truncate(text, 200))
		return nil
	}
	return items
}

const defaultProfilePrompt = `你是一个用户画像分析助手。从 AI 助手的长期记忆中提取用户的稳定特征。

规则：
1. 只提取跨多次对话一致的稳定特征
2. 忽略一次性行为和临时状态
3. 评估置信度：多次确认的特征 > 0.8，单次观察 < 0.5
4. 输出纯 JSON 数组`

// ============================================================================
// ProfileBuilder — 将 ProfileItem 组装为 system prompt 片段
// ============================================================================

// BuildProfilePrompt 将用户画像条目组装为可注入 system prompt 的文本。
func BuildProfilePrompt(profileEntries []TieredEntry) string {
	if len(profileEntries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[User Profile]\n")

	for _, e := range profileEntries {
		sb.WriteString("- ")
		if e.Category != "" {
			sb.WriteString(fmt.Sprintf("(%s) ", e.Category))
		}
		sb.WriteString(e.Content)
		sb.WriteByte('\n')
	}

	sb.WriteString("[End Profile]")
	return sb.String()
}
