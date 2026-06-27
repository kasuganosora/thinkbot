package memory

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// BotProfileProfiler — L1+L2 → L3 Bot 自我画像提取器
//
// 与 LLMProfiler（用户画像）对称但专门针对 BotScope。
// 从 BotScope 的 L1（长期记忆）和 L2（场景记忆）中蒸馏出 Bot 的
// 量化人格画像（BotProfileTraits），写入 L3。
//
// 提取维度：energy_level, patience, preferred_topics, verbosity, personality
// ============================================================================

// BotProfileProfilerConfig 配置 Bot 画像提取器。
type BotProfileProfilerConfig struct {
	Provider     llm.Provider
	Model        *llm.Model
	SystemPrompt string
}

// BotProfileProfiler 使用 LLM 提取 Bot 自我画像。
type BotProfileProfiler struct {
	config BotProfileProfilerConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewBotProfileProfiler 创建 Bot 画像提取器。
func NewBotProfileProfiler(config BotProfileProfilerConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *BotProfileProfiler {
	if config.Provider == nil {
		panic("bot_profile_profiler: config.Provider must not be nil")
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaultBotProfilePrompt
	}
	return &BotProfileProfiler{
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/bot_profile_profiler"),
		logger: logger.With("component", "bot_profile_profiler"),
	}
}

// BotProfileResult 是 LLM 提取的 Bot 自我画像结果。
type BotProfileResult struct {
	// EnergyLevel 精力值 0.0~1.0。
	EnergyLevel float64 `json:"energy_level"`
	// Patience 耐心值 0.0~1.0。
	Patience float64 `json:"patience"`
	// PreferredTopics Bot 表现出来的兴趣主题。
	PreferredTopics []string `json:"preferred_topics"`
	// Verbosity 话痨度 0.0~1.0。
	Verbosity float64 `json:"verbosity"`
	// Personality 人格描述标签。
	Personality string `json:"personality"`
	// Confidence 整体可信度 0.0~1.0。
	Confidence float64 `json:"confidence"`
}

// ExtractProfile 从 BotScope 的 L1 和 L2 记忆中提取 Bot 自我画像。
func (p *BotProfileProfiler) ExtractProfile(ctx context.Context, l1Entries, l2Entries []TieredEntry, existing []TieredEntry) (*BotProfileResult, error) {
	if len(l1Entries) == 0 && len(l2Entries) == 0 {
		return nil, nil
	}

	ctx, span := p.tracer.Start(ctx, "memory.bot_profile.extract",
		trace.WithAttributes(
			attribute.Int("l1_count", len(l1Entries)),
			attribute.Int("l2_count", len(l2Entries)),
			attribute.Int("existing_count", len(existing)),
		))
	defer span.End()
	logger := traceid.WithLoggerFrom(ctx, p.logger)

	prompt := p.buildPrompt(l1Entries, l2Entries, existing)
	logger.Debugw("bot_profile_profiler: extracting profile",
		"l1_count", len(l1Entries),
		"l2_count", len(l2Entries),
		"prompt_len", len(prompt))

	maxTokens := 2048
	result, err := p.config.Provider.DoGenerate(ctx, llm.GenerateParams{
		Model:     p.config.Model,
		System:    p.config.SystemPrompt,
		Messages:  []llm.Message{llm.UserMessage(prompt)},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		span.RecordError(err)
		logger.Errorw("bot_profile_profiler: LLM call failed", "err", err)
		return nil, fmt.Errorf("bot_profile_profiler: LLM call failed: %w", err)
	}

	// 记录 LLM 使用量指标
	span.SetAttributes(
		attribute.Int("llm.input_tokens", result.Usage.InputTokens),
		attribute.Int("llm.output_tokens", result.Usage.OutputTokens),
		attribute.Int("llm.total_tokens", result.Usage.TotalTokens),
	)

	profile := p.parseResult(result.Text)
	if profile == nil {
		span.SetAttributes(attribute.Bool("parsed", false))
		return nil, nil
	}

	span.SetAttributes(
		attribute.String("personality", profile.Personality),
		attribute.Float64("confidence", profile.Confidence),
		attribute.Float64("energy_level", profile.EnergyLevel),
		attribute.Float64("patience", profile.Patience),
		attribute.Float64("verbosity", profile.Verbosity),
		attribute.Int("topics_count", len(profile.PreferredTopics)),
	)
	logger.Infow("bot profile extracted",
		"personality", profile.Personality,
		"confidence", profile.Confidence,
		"energy", profile.EnergyLevel,
		"patience", profile.Patience,
		"verbosity", profile.Verbosity,
		"topics", profile.PreferredTopics)

	return profile, nil
}

func (p *BotProfileProfiler) buildPrompt(l1, l2, existing []TieredEntry) string {
	var sb strings.Builder

	sb.WriteString("## Bot 自身的行为历史（L1 长期记忆）\n\n")
	for _, e := range l1 {
		fmt.Fprintf(&sb, "- (%s) %s\n", e.Category, StripThinking(e.Content))
	}

	if len(l2) > 0 {
		sb.WriteString("\n## Bot 的交互场景记忆（L2）\n\n")
		for _, e := range l2 {
			fmt.Fprintf(&sb, "- %s\n", StripThinking(e.Content))
		}
	}

	if len(existing) > 0 {
		sb.WriteString("\n## 已有画像（参考，避免矛盾）\n\n")
		for _, e := range existing {
			fmt.Fprintf(&sb, "- %s\n", e.Content)
		}
	}

	sb.WriteString("\n## 任务\n")
	sb.WriteString("从 Bot 自身的行为历史中提取 Bot 的「自我画像」。\n")
	sb.WriteString("输出纯 JSON：\n")
	sb.WriteString(`{
  "energy_level": 0.0-1.0,
  "patience": 0.0-1.0,
  "preferred_topics": ["话题1", "话题2"],
  "verbosity": 0.0-1.0,
  "personality": "人格标签",
  "confidence": 0.0-1.0
}`)
	sb.WriteString("\n\n规则：\n")
	sb.WriteString("- energy_level: Bot 参与讨论的积极程度\n")
	sb.WriteString("- patience: Bot 面对重复/无意义问题的耐心\n")
	sb.WriteString("- preferred_topics: Bot 频繁参与的话题\n")
	sb.WriteString("- verbosity: Bot 回复长度偏好（0=惜字如金，1=滔滔不绝）\n")
	sb.WriteString("- personality: 一两句话描述 Bot 的行为风格\n")
	sb.WriteString("- confidence: 基于记忆样本量评估画像可信度\n")

	return sb.String()
}

func (p *BotProfileProfiler) parseResult(text string) *BotProfileResult {
	var result BotProfileResult
	if err := strutil.ExtractJSON(text, &result); err != nil {
		p.logger.Warnw("bot_profile_profiler: failed to parse LLM JSON",
			"err", err,
			"text_preview", strutil.Truncate(text, 200))
		return nil
	}
	// 验证范围
	if result.EnergyLevel < 0 {
		result.EnergyLevel = 0
	}
	if result.EnergyLevel > 1 {
		result.EnergyLevel = 1
	}
	if result.Patience < 0 {
		result.Patience = 0
	}
	if result.Patience > 1 {
		result.Patience = 1
	}
	if result.Verbosity < 0 {
		result.Verbosity = 0
	}
	if result.Verbosity > 1 {
		result.Verbosity = 1
	}
	if result.Confidence < 0 {
		result.Confidence = 0
	}
	if result.Confidence > 1 {
		result.Confidence = 1
	}
	return &result
}

const defaultBotProfilePrompt = `你是一个 Bot 自我画像分析助手。从 Bot 自身的行为历史中提取其稳定的人格特征。

规则：
1. 从 Bot 与用户的交互模式中推断自我画像
2. energy_level: Bot 主动参与讨论的频率和积极性
3. patience: Bot 面对重复问题/无理要求时的容忍度
4. preferred_topics: Bot 最常参与讨论的话题领域
5. verbosity: Bot 回复的详细程度偏好
6. personality: 用一两句话的风格化描述
7. 输出纯 JSON，不要其他文本`
