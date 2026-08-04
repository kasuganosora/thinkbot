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
	result, err := p.config.Provider.DoGenerate(llm.WithStatsFeature(ctx, "bot_profiler"), llm.GenerateParams{
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

	sb.WriteString("## The bot's own behavioral history (L1 long-term memory)\n\n")
	for _, e := range l1 {
		fmt.Fprintf(&sb, "- (%s) %s\n", e.Category, StripThinking(e.Content))
	}

	if len(l2) > 0 {
		sb.WriteString("\n## The bot's episodic interaction memory (L2)\n\n")
		for _, e := range l2 {
			fmt.Fprintf(&sb, "- %s\n", StripThinking(e.Content))
		}
	}

	if len(existing) > 0 {
		sb.WriteString("\n## Existing profile (for reference — do NOT contradict it)\n\n")
		for _, e := range existing {
			fmt.Fprintf(&sb, "- %s\n", e.Content)
		}
	}

	sb.WriteString("\n## Task\n")
	sb.WriteString("Derive the bot's self-profile from its own behavioral history.\n")
	sb.WriteString("Output raw JSON matching this schema exactly:\n")
	sb.WriteString(`{
  "energy_level": 0.0-1.0,
  "patience": 0.0-1.0,
  "preferred_topics": ["话题1", "话题2"],
  "verbosity": 0.0-1.0,
  "personality": "人格标签",
  "confidence": 0.0-1.0
}`)
	sb.WriteString("\n\nField definitions:\n")
	sb.WriteString("- energy_level: how actively the bot engages in discussion\n")
	sb.WriteString("- patience: how well the bot tolerates repetitive or pointless questions\n")
	sb.WriteString("- preferred_topics: the topics the bot engages with most often\n")
	sb.WriteString("- verbosity: the bot's reply-length preference (0 = terse, 1 = long-winded)\n")
	sb.WriteString("- personality: one or two sentences describing the bot's behavioral style\n")
	sb.WriteString("- confidence: how trustworthy this profile is given the memory sample size\n")
	sb.WriteString("\nWrite preferred_topics and personality in Chinese (中文).\n")

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

const defaultBotProfilePrompt = `You are a bot self-profiler, an analysis component that derives a bot's stable personality traits from its own behavioral history.

Rules:
1. Infer the self-profile from how the bot actually interacts with users, not from how it describes itself.
2. energy_level: how often and how eagerly the bot joins discussions.
3. patience: how much the bot tolerates repetitive questions and unreasonable requests.
4. preferred_topics: the topic areas the bot engages with most.
5. verbosity: how detailed the bot's replies tend to be.
6. personality: one or two sentences of stylized description.
7. Output raw JSON and nothing else — no prose, no code fences.
8. Write preferred_topics and personality in Chinese (中文).`
