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
// Profiler 从 L1（长期记忆）和 L2（场景记忆）中蒸馏出稳定的用户画像，
// 包括人格特征（traits）、稳定事实（facts）、沟通偏好（preferences）
// 和交互行为模式（behavior）。
//
// 提取流程：先对 L1 记忆做 TF-IDF 聚类，然后按聚类分组提取画像（避免主题混叠），
// 最后对提取结果做语义一致性验证（基于 embedding 或 Jaccard 相似度），
// 过滤掉与源记忆不一致的低质量画像。
//
// 画像记忆作为 system prompt 的持久部分注入，利于 LLM prompt cache。
// ============================================================================

// 画像元素类型常量。
const (
	ProfileTypeTrait      = "trait"      // 性格特征（如 "用户性格偏理性"）
	ProfileTypeFact       = "fact"       // 稳定事实（如 "用户使用 Go 语言"）
	ProfileTypePreference = "preference" // 沟通偏好（如 "偏好简洁的回复风格"）
	ProfileTypeBehavior   = "behavior"   // 交互行为模式（如 "倾向先问可行性再要方案"）
)

// Profiler 定义用户画像提取能力。
type Profiler interface {
	// ExtractProfile 从长期和场景记忆中提取用户画像。
	// 返回提取出的画像条目内容列表。
	ExtractProfile(ctx context.Context, l1Entries, l2Entries []TieredEntry, existing []TieredEntry) ([]ProfileItem, error)
}

// ProfileItem 是提取出的单条画像元素。
type ProfileItem struct {
	// Type 画像元素类型。
	Type string `json:"type"` // "trait" / "fact" / "preference" / "behavior"
	// Content 画像内容。
	Content string `json:"content"`
	// Confidence 置信度（0.0~1.0）。
	Confidence float64 `json:"confidence"`
	// Validated 是否通过语义一致性验证。
	// 当为 true 时表示此画像已通过 cosine/Jaccard 相似度验证。
	Validated bool `json:"validated,omitempty"`
	// ValidationScore 验证分数（0.0~1.0），仅当 Validated=true 时有意义。
	// 表示画像与源记忆的语义相似度减去与其他 scope 记忆的平均相似度。
	ValidationScore float64 `json:"validation_score,omitempty"`
}

// ============================================================================
// LLMProfiler — 基于 LLM 的画像提取器
// ============================================================================

// LLMProfilerConfig 配置 LLM 画像提取器。
type LLMProfilerConfig struct {
	Provider     llm.Provider
	Model        *llm.Model
	SystemPrompt string
	// EmbeddingProvider 可选的 embedding 后端（用于语义验证）。
	// 为 nil 时降级使用 Jaccard 相似度验证。
	EmbeddingProvider llm.EmbeddingProvider
	// EmbeddingModel embedding 模型（可选）。
	EmbeddingModel *llm.Model
	// EnableValidation 是否启用画像验证（默认 true）。
	// 关闭后跳过语义一致性检查，所有画像直接通过。
	EnableValidation bool
	// EnableClustering 是否启用聚类辅助提取（默认 true）。
	// 开启后先对 L1 做 TF-IDF 聚类再按组提取，避免主题混叠。
	EnableClustering bool
	// ClusterCount 目标聚类数（0 表示自动 = sqrt(N)，上限 8）。
	ClusterCount int
	// MinValidationScore 最低验证分数（默认 0.15）。
	// 低于此分数的画像会被丢弃。
	MinValidationScore float64
}

// DefaultLLMProfilerConfig 返回默认配置。
func DefaultLLMProfilerConfig() LLMProfilerConfig {
	return LLMProfilerConfig{
		EnableValidation:   true,
		EnableClustering:   true,
		MinValidationScore: 0.15,
	}
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
// 流程：聚类辅助分组提取 → 语义一致性验证 → 过滤低质量画像。
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

	// 选择提取策略：聚类辅助 vs 直接全量
	var items []ProfileItem
	var err error
	if p.config.EnableClustering && len(l1Entries) >= 6 {
		items, err = p.extractWithClustering(ctx, l1Entries, l2Entries, existing)
	} else {
		items, err = p.extractDirect(ctx, l1Entries, l2Entries, existing)
	}
	if err != nil {
		return nil, err
	}

	span.SetAttributes(attribute.Int("profile_items_raw", len(items)))

	// 语义一致性验证
	if p.config.EnableValidation && len(items) > 0 {
		sourceEntries := make([]TieredEntry, 0, len(l1Entries)+len(l2Entries))
		sourceEntries = append(sourceEntries, l1Entries...)
		sourceEntries = append(sourceEntries, l2Entries...)
		items = p.validateItems(ctx, items, sourceEntries)
	}

	span.SetAttributes(attribute.Int("profile_items_final", len(items)))
	p.logger.Debugw("profile extraction complete",
		"l1_input", len(l1Entries),
		"l2_input", len(l2Entries),
		"profile_items", len(items))

	return items, nil
}

// extractDirect 直接将所有记忆一次性交给 LLM 提取（适用于记忆较少的场景）。
func (p *LLMProfiler) extractDirect(ctx context.Context, l1, l2, existing []TieredEntry) ([]ProfileItem, error) {
	prompt := p.buildPrompt(l1, l2, existing)
	return p.callLLM(ctx, prompt)
}

// extractWithClustering 先对 L1 做 TF-IDF 聚类，然后按聚类分组提取画像。
// 避免 LLM 在大量混合主题记忆中产生主题混叠问题。
func (p *LLMProfiler) extractWithClustering(ctx context.Context, l1, l2, existing []TieredEntry) ([]ProfileItem, error) {
	// 1. 聚类 L1 记忆
	k := p.config.ClusterCount
	if k <= 0 {
		k = clusterCountSuggestion(len(l1))
	}
	clusters := clusterEntries(l1, k)

	// 2. 对每个聚类提取画像
	var allItems []ProfileItem
	for _, cluster := range clusters {
		if len(cluster.entries) == 0 {
			continue
		}
		prompt := p.buildClusterPrompt(cluster, l2, existing)
		items, err := p.callLLM(ctx, prompt)
		if err != nil {
			p.logger.Warnw("profiler: cluster extraction failed",
				"cluster_keyword", cluster.keyword,
				"err", err)
			continue
		}
		allItems = append(allItems, items...)
	}

	// 3. 对聚类间结果做去重
	allItems = dedupProfileItems(allItems)

	return allItems, nil
}

func (p *LLMProfiler) buildPrompt(l1, l2, existing []TieredEntry) string {
	var sb strings.Builder

	sb.WriteString("## 长期记忆（L1）\n\n")
	for _, e := range l1 {
		fmt.Fprintf(&sb, "- (%s) %s\n", e.Category, e.Content)
	}

	if len(l2) > 0 {
		sb.WriteString("\n## 场景记忆（L2）\n\n")
		for _, e := range l2 {
			fmt.Fprintf(&sb, "- %s\n", e.Content)
		}
	}

	if len(existing) > 0 {
		sb.WriteString("\n## 已有画像（供参考，不要重复）\n\n")
		for _, e := range existing {
			fmt.Fprintf(&sb, "- %s\n", e.Content)
		}
	}

	sb.WriteString("\n## 任务\n")
	sb.WriteString("从以上记忆中提取稳定的用户画像。输出 JSON 数组：\n")
	sb.WriteString(`[{"type":"trait","content":"...","confidence":0.9}]`)
	sb.WriteString("\ntype: trait(性格特征) / fact(稳定事实) / preference(沟通偏好) / behavior(交互行为模式)\n")
	sb.WriteString("只提取高置信度的稳定特征，忽略一次性行为。\n")

	return sb.String()
}

// buildClusterPrompt 为单个聚类构建提取 prompt。
func (p *LLMProfiler) buildClusterPrompt(cluster profileCluster, l2, existing []TieredEntry) string {
	var sb strings.Builder

	sb.WriteString("## 相关记忆集群")
	if cluster.keyword != "" {
		fmt.Fprintf(&sb, "（主题: %s）", cluster.keyword)
	}
	sb.WriteString("\n\n")
	for _, e := range cluster.entries {
		fmt.Fprintf(&sb, "- (%s) %s\n", e.Category, e.Content)
	}

	if len(l2) > 0 {
		sb.WriteString("\n## 场景记忆（L2，上下文参考）\n\n")
		for _, e := range l2 {
			fmt.Fprintf(&sb, "- %s\n", e.Content)
		}
	}

	if len(existing) > 0 {
		sb.WriteString("\n## 已有画像（供参考，不要重复）\n\n")
		for _, e := range existing {
			fmt.Fprintf(&sb, "- %s\n", e.Content)
		}
	}

	sb.WriteString("\n## 任务\n")
	sb.WriteString("从以上记忆集群中提取该主题相关的稳定用户画像。输出 JSON 数组：\n")
	sb.WriteString(`[{"type":"trait","content":"...","confidence":0.9}]`)
	sb.WriteString("\ntype: trait(性格特征) / fact(稳定事实) / preference(沟通偏好) / behavior(交互行为模式)\n")
	sb.WriteString("只提取与该集群主题强相关的稳定特征。\n")

	return sb.String()
}

// callLLM 调用 LLM 并解析结果。
func (p *LLMProfiler) callLLM(ctx context.Context, prompt string) ([]ProfileItem, error) {
	result, err := p.config.Provider.DoGenerate(ctx, llm.GenerateParams{
		Model:    p.config.Model,
		System:   p.config.SystemPrompt,
		Messages: []llm.Message{llm.UserMessage(prompt)},
	})
	if err != nil {
		return nil, errs.Wrap(err, "profiler: LLM call failed")
	}
	return p.parseResult(result.Text), nil
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
4. 输出纯 JSON 数组

画像类型：
- trait: 性格特征（"用户性格偏理性"、"用户喜欢深入分析"）
- fact: 稳定事实（"用户使用 Go 语言"、"用户在做区块链项目"）
- preference: 沟通偏好（"用户偏好简洁回复"、"用户喜欢有代码示例"）
- behavior: 交互行为模式（"用户倾向先问可行性再要方案"、"用户经常切换话题"、"用户习惯在深夜活跃"）`

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
			fmt.Fprintf(&sb, "(%s) ", e.Category)
		}
		sb.WriteString(e.Content)
		sb.WriteByte('\n')
	}

	sb.WriteString("[End Profile]")
	return sb.String()
}
