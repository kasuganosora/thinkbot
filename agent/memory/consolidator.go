package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// Consolidator — L0→L1 巩固管道
//
// 参考 Memoh 的 Formation Pipeline（Extract → Decide → Store）和
// TencentDB-Agent-Memory 的 L1 Runner（LLM 提取 → 去重 → 存储）。
//
// Consolidator 从 Tier0（工作记忆）中提取结构化事实，
// 经过去重和重要度评估后写入 Tier1（长期记忆）。
//
// 提取决策类型：
//   - ADD:     新事实，写入 L1
//   - UPDATE:  已有事实的更新（替换旧版本）
//   - MERGE:   与已有事实合并（补充信息）
//   - SKIP:    无价值内容（闲聊、已存在等）
// ============================================================================

// ConsolidateDecision 标识 LLM 对一条候选记忆的处置决策。
type ConsolidateDecision string

const (
	DecisionAdd    ConsolidateDecision = "ADD"
	DecisionUpdate ConsolidateDecision = "UPDATE"
	DecisionMerge  ConsolidateDecision = "MERGE"
	DecisionSkip   ConsolidateDecision = "SKIP"
)

// ConsolidateResult 是单条记忆的提取结果。
type ConsolidateResult struct {
	// SourceID 原始 L0 条目 ID。
	SourceID string `json:"source_id"`
	// Decision 处置决策。
	Decision ConsolidateDecision `json:"decision"`
	// Category 提取的分类（fact/preference/event/observation）。
	Category string `json:"category,omitempty"`
	// Content 提取后的内容（Decision=SKIP 时为空）。
	Content string `json:"content,omitempty"`
	// Importance LLM 评估的重要度（0.0~1.0）。
	Importance float64 `json:"importance,omitempty"`
	// TargetID 当 Decision=UPDATE/MERGE 时，指向要修改的 L1 条目 ID。
	TargetID string `json:"target_id,omitempty"`
	// Reason 决策原因（用于调试）。
	Reason string `json:"reason,omitempty"`
}

// Consolidator 定义 L0→L1 巩固能力。
//
// 实现方可以使用 LLM（LLMConsolidator）或基于规则（RuleConsolidator）。
type Consolidator interface {
	// Consolidate 从一批 L0 条目中提取结构化记忆。
	// existing 为该 scope 下已有的 L1 记忆（供去重和 UPDATE 决策参考）。
	Consolidate(ctx context.Context, l0Entries []TieredEntry, existing []TieredEntry) ([]ConsolidateResult, error)
}

// ============================================================================
// LLMConsolidator — 基于 LLM 的巩固器
// ============================================================================

// LLMConsolidatorConfig 配置 LLM 巩固器。
type LLMConsolidatorConfig struct {
	// Provider LLM 提供商。
	Provider llm.Provider
	// Model 指定模型（可选）。
	Model *llm.Model
	// SystemPrompt 系统提示词（为空使用默认）。
	SystemPrompt string
	// MaxInputEntries 单次最多处理的 L0 条目数（默认 30）。
	MaxInputEntries int
}

// DefaultLLMConsolidatorConfig 返回默认配置（需调用方注入 Provider）。
func DefaultLLMConsolidatorConfig() LLMConsolidatorConfig {
	return LLMConsolidatorConfig{
		MaxInputEntries: 30,
	}
}

// LLMConsolidator 使用 LLM 从工作记忆中提取长期记忆。
type LLMConsolidator struct {
	config LLMConsolidatorConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewLLMConsolidator 创建 LLM 巩固器。
func NewLLMConsolidator(config LLMConsolidatorConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *LLMConsolidator {
	if config.Provider == nil {
		panic("consolidator: config.Provider must not be nil")
	}
	if config.MaxInputEntries <= 0 {
		config.MaxInputEntries = 30
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaultConsolidatePrompt
	}
	return &LLMConsolidator{
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/consolidator"),
		logger: logger.With("component", "memory_consolidator"),
	}
}

// Consolidate 从 L0 条目中提取结构化记忆。
func (c *LLMConsolidator) Consolidate(ctx context.Context, l0Entries []TieredEntry, existing []TieredEntry) ([]ConsolidateResult, error) {
	if len(l0Entries) == 0 {
		return nil, nil
	}

	// 限制输入数量
	if len(l0Entries) > c.config.MaxInputEntries {
		c.logger.Warnw("consolidator: input exceeds MaxInputEntries, excess entries will not be processed this run",
			"total", len(l0Entries), "max", c.config.MaxInputEntries)
		l0Entries = l0Entries[:c.config.MaxInputEntries]
	}

	ctx, span := c.tracer.Start(ctx, "memory.consolidate",
		trace.WithAttributes(
			attribute.Int("l0_count", len(l0Entries)),
			attribute.Int("existing_l1_count", len(existing)),
		))
	defer span.End()

	// 构建 prompt
	prompt := c.buildPrompt(l0Entries, existing)

	// 调用 LLM
	maxTokens := 8192
	result, err := c.config.Provider.DoGenerate(ctx, llm.GenerateParams{
		Model:     c.config.Model,
		System:    c.config.SystemPrompt,
		Messages:  []llm.Message{llm.UserMessage(prompt)},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		span.RecordError(err)
		return nil, errs.Wrap(err, "consolidator: LLM call failed")
	}

	// 解析结果
	decisions := c.parseResult(result.Text)

	// 校验：LLM 返回的决策数应与输入条目数一致
	if len(decisions) < len(l0Entries) {
		c.logger.Warnw("consolidator: LLM returned fewer decisions than input entries",
			"input", len(l0Entries), "decisions", len(decisions))
	}

	span.SetAttributes(attribute.Int("decisions_count", len(decisions)))
	c.logger.Debugw("consolidation complete",
		"l0_input", len(l0Entries),
		"decisions", len(decisions))

	return decisions, nil
}

// buildPrompt 构建发送给 LLM 的巩固请求。
func (c *LLMConsolidator) buildPrompt(l0Entries []TieredEntry, existing []TieredEntry) string {
	var sb strings.Builder

	sb.WriteString("## 待处理的工作记忆（L0）\n\n")
	for _, e := range l0Entries {
		content := StripThinking(e.Content)
		fmt.Fprintf(&sb, "[%s] %s\n", e.ID, content)
	}

	if len(existing) > 0 {
		sb.WriteString("\n## 已有的长期记忆（L1，供去重参考）\n\n")
		for _, e := range existing {
			fmt.Fprintf(&sb, "[%s] (%s) %s\n", e.ID, e.Category, e.Content)
		}
	}

	sb.WriteString("\n## 任务\n")
	sb.WriteString("对每条 L0 记忆做出决策。输出 JSON 数组，每个元素：\n")
	sb.WriteString("```json\n")
	sb.WriteString(`[{"source_id":"mem-xxx","decision":"ADD","category":"fact","content":"...","importance":0.8,"reason":"..."}]`)
	sb.WriteString("\n```\n")
	sb.WriteString("decision 可选: ADD(新事实) / UPDATE(更新已有) / MERGE(补充已有) / SKIP(无价值)\n")
	sb.WriteString("当 decision=UPDATE/MERGE 时需额外提供 target_id 指向要修改的 L1 条目 ID。\n")

	return sb.String()
}

// parseResult 解析 LLM 返回的决策结果。
func (c *LLMConsolidator) parseResult(text string) []ConsolidateResult {
	var results []ConsolidateResult
	if err := strutil.ExtractJSON(text, &results); err != nil {
		c.logger.Warnw("consolidator: failed to parse LLM JSON",
			"err", err,
			"text_preview", strutil.Truncate(text, 200))
		return nil
	}
	return results
}

const defaultConsolidatePrompt = `你是一个记忆提取助手。你的任务是从 Bot 的工作记忆（原始对话/观察）中提取值得长期保存的结构化事实。

规则：
1. 提取事实、偏好、事件和重要观察
2. 与已有记忆重复时使用 UPDATE 或 MERGE，而非 ADD
3. 闲聊、问候、无实质内容的使用 SKIP
4. 评估每条记忆的重要度（0.0~1.0），越高越重要
5. 输出纯 JSON 数组，不要其他文本

分类建议：
- fact: 客观事实（"用户使用 Go 语言"）
- preference: 偏好（"用户偏好简洁的回复风格"）
- event: 事件（"用户完成了项目部署"）
- observation: 观察（"用户似乎对 Rust 感兴趣"）`

// ============================================================================
// RuleConsolidator — 基于规则的巩固器（无 LLM 依赖，用于测试/降级）
// ============================================================================

// RuleConsolidatorConfig 配置规则巩固器。
type RuleConsolidatorConfig struct {
	// MinContentLength 最小内容长度（短于此的直接 SKIP）。
	MinContentLength int
	// SkipKeywords 匹配这些关键词的条目直接 SKIP。
	SkipKeywords []string
}

// DefaultRuleConsolidatorConfig 返回默认配置。
func DefaultRuleConsolidatorConfig() RuleConsolidatorConfig {
	return RuleConsolidatorConfig{
		MinContentLength: 10,
		SkipKeywords:     []string{"你好", "hello", "hi", "ok", "好的", "谢谢"},
	}
}

// RuleConsolidator 使用简单规则判断记忆价值。
// 无 LLM 依赖，适用于测试或 LLM 不可用时的降级方案。
type RuleConsolidator struct {
	config RuleConsolidatorConfig
}

// NewRuleConsolidator 创建规则巩固器。
func NewRuleConsolidator(config ...RuleConsolidatorConfig) *RuleConsolidator {
	cfg := DefaultRuleConsolidatorConfig()
	if len(config) > 0 {
		cfg = config[0]
	}
	return &RuleConsolidator{config: cfg}
}

// Consolidate 使用规则评估 L0 条目。
func (c *RuleConsolidator) Consolidate(_ context.Context, l0Entries []TieredEntry, _ []TieredEntry) ([]ConsolidateResult, error) {
	results := make([]ConsolidateResult, 0, len(l0Entries))

	for _, e := range l0Entries {
		content := StripThinking(e.Content)
		content = strings.TrimSpace(content)

		result := ConsolidateResult{
			SourceID: e.ID,
		}

		// 太短 → SKIP
		if len([]rune(content)) < c.config.MinContentLength {
			result.Decision = DecisionSkip
			result.Reason = "too short"
			results = append(results, result)
			continue
		}

		// 匹配跳过关键词
		lowerContent := strings.ToLower(content)
		skipped := false
		for _, kw := range c.config.SkipKeywords {
			if strings.Contains(lowerContent, strings.ToLower(kw)) {
				result.Decision = DecisionSkip
				result.Reason = "matches skip keyword"
				skipped = true
				break
			}
		}
		if skipped {
			results = append(results, result)
			continue
		}

		// 通过规则 → ADD
		result.Decision = DecisionAdd
		result.Category = "observation"
		result.Content = content
		result.Importance = 0.5
		results = append(results, result)
	}

	return results, nil
}

// ============================================================================
// Aggregator — L1→L2 场景聚合器
// ============================================================================

// Aggregator 定义 L1→L2 聚合能力。
// 将相关的长期记忆聚类为主题场景。
type Aggregator interface {
	// Aggregate 将一批 L1 记忆聚合为场景摘要。
	Aggregate(ctx context.Context, l1Entries []TieredEntry) ([]TieredEntry, error)
}

// ============================================================================
// TieredManager — 分层记忆管理器（协调各层级）
// ============================================================================

// TieredManagerConfig 配置分层记忆管理器。
type TieredManagerConfig struct {
	// Store 分层存储后端。
	Store *TieredStore
	// Consolidator L0→L1 巩固器（nil 使用 RuleConsolidator）。
	Consolidator Consolidator
	// Aggregator L1→L2 场景聚合器（可选）。
	Aggregator Aggregator
	// Profiler L2→L3 画像提取器（可选）。
	Profiler Profiler
	// EnableAutoConsolidate 是否在写入 L0 后自动触发巩固。
	// 检查未处理 L0 条目数是否达到阈值。
	EnableAutoConsolidate bool
	// ConsolidateDebounce 自动巩固防抖间隔（默认 30 秒）。
	ConsolidateDebounce time.Duration
}

// TieredManager 协调四个记忆层级的写入、检索和巩固。
//
// 它是 memory 模块的全新高层 API，封装了：
//   - TieredStore: 分层存储
//   - Consolidator: L0→L1 提升
//   - Aggregator: L1→L2 场景聚合（可选）
//   - Profiler: L2→L3 画像提取（可选）
//   - 多层检索合并
//   - GC 过期清理
//
// 典型生命周期：
//
//  1. 用户消息 → WriteWorking() → 存入 L0
//  2. L0 达到阈值 → Consolidate() → 提取 L1
//  3. L1 积累到一定量 → Aggregate() → 生成 L2 场景
//  4. L2 到达间隔 → ExtractProfile() → 更新 L3 画像
//  5. 检索时 → RetrieveMerged() → 合并 L3+L2+L1+L0
type TieredManager struct {
	store                *TieredStore
	consolidator         Consolidator
	aggregator           Aggregator
	profiler             Profiler
	autoConsolidate      bool
	consolidateThreshold int
	consolidateDebounce  time.Duration
	tracer               trace.Tracer
	logger               *zap.SugaredLogger

	// 用于 lastConsolidate 去重（避免短时间重复触发）
	mu               sync.Mutex
	lastConsolidated map[string]time.Time // scope.Key() -> last time
}

// NewTieredManager 创建分层记忆管理器。
func NewTieredManager(
	config TieredManagerConfig,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *TieredManager {
	consolidator := config.Consolidator
	if consolidator == nil {
		consolidator = NewRuleConsolidator()
	}

	debounce := config.ConsolidateDebounce
	if debounce <= 0 {
		debounce = 30 * time.Second
	}

	// 从 store 配置中读取巩固阈值
	threshold := 20
	if config.Store != nil {
		if cfg, ok := config.Store.configs[Tier0Working]; ok && cfg.ConsolidateThreshold > 0 {
			threshold = cfg.ConsolidateThreshold
		}
	}

	return &TieredManager{
		store:                config.Store,
		consolidator:         consolidator,
		aggregator:           config.Aggregator,
		profiler:             config.Profiler,
		autoConsolidate:      config.EnableAutoConsolidate,
		consolidateThreshold: threshold,
		consolidateDebounce:  debounce,
		tracer:               tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/tiered_manager"),
		logger:               logger.With("component", "tiered_memory_manager"),
		lastConsolidated:     make(map[string]time.Time),
	}
}

// WriteWorking 写入一条工作记忆（L0）。
// 如果开启自动巩固且未处理条目达到阈值，触发后台巩固。
func (m *TieredManager) WriteWorking(ctx context.Context, scope Scope, content string, source string) error {
	entry := TieredEntry{
		Entry: Entry{
			Scope:   scope,
			Content: StripThinking(content),
			Source:  source,
		},
		Tier: Tier0Working,
	}

	if err := m.store.Append(ctx, entry); err != nil {
		return err
	}

	// 自动巩固检查
	if m.autoConsolidate {
		m.maybeConsolidate(ctx, scope)
	}

	return nil
}

// WriteLongTerm 直接写入一条长期记忆（L1）。
// 通常由 Consolidator 的结果调用，也可手动写入。
func (m *TieredManager) WriteLongTerm(ctx context.Context, entry Entry, promotedFrom MemoryTier) error {
	return m.store.Append(ctx, TieredEntry{
		Entry:        entry,
		Tier:         Tier1LongTerm,
		PromotedFrom: promotedFrom,
	})
}

// WriteEpisodic 写入一条场景记忆（L2）。
func (m *TieredManager) WriteEpisodic(ctx context.Context, entry Entry, entryIDs []string) error {
	te := TieredEntry{
		Entry:        entry,
		Tier:         Tier2Episodic,
		PromotedFrom: Tier1LongTerm,
	}
	if te.Metadata == nil {
		te.Metadata = make(map[string]any)
	}
	te.Metadata["source_entry_ids"] = entryIDs
	return m.store.Append(ctx, te)
}

// WriteProfile 写入一条用户画像记忆（L3）。
func (m *TieredManager) WriteProfile(ctx context.Context, entry Entry) error {
	te := TieredEntry{
		Entry:        entry,
		Tier:         Tier3Profile,
		PromotedFrom: Tier2Episodic,
	}
	te.Category = "profile"
	return m.store.Append(ctx, te)
}

// Consolidate 执行 L0→L1 巩固。
// 从指定 scope 的未处理 L0 条目中提取结构化事实，写入 L1。
// 成功后将已处理的 L0 条目标记为 consolidated。
func (m *TieredManager) Consolidate(ctx context.Context, scope Scope) (int, error) {
	ctx, span := m.tracer.Start(ctx, "memory.tiered.consolidate",
		trace.WithAttributes(attribute.String("scope", scope.Key())))
	defer span.End()

	// 获取未处理的 L0 条目
	l0Entries, err := m.store.GetUnprocessed(ctx, scope, 50)
	if err != nil {
		return 0, errs.Wrap(err, "get unprocessed L0")
	}
	if len(l0Entries) == 0 {
		return 0, nil
	}

	// 获取已有 L1 记忆（供去重参考）
	existing, err := m.store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, 50)
	if err != nil {
		m.logger.Warnw("failed to get existing L1 for consolidation", "err", err)
		existing = nil
	}

	// 调用 Consolidator
	decisions, err := m.consolidator.Consolidate(ctx, l0Entries, existing)
	if err != nil {
		span.RecordError(err)
		return 0, errs.Wrap(err, "consolidator failed")
	}

	// 应用决策
	promoted := 0
	var processedIDs []string

	for _, d := range decisions {
		processedIDs = append(processedIDs, d.SourceID)

		switch d.Decision {
		case DecisionAdd:
			err := m.WriteLongTerm(ctx, Entry{
				Scope:      scope,
				Content:    d.Content,
				Category:   d.Category,
				Source:     "consolidator",
				Importance: d.Importance,
				Metadata: map[string]any{
					"promoted_from_id": d.SourceID,
					"consolidated_at":  time.Now(),
				},
			}, Tier0Working)
			if err != nil {
				m.logger.Warnw("failed to write L1 entry", "err", err)
				continue
			}
			promoted++

		case DecisionUpdate, DecisionMerge:
			// 更新或合并已有 L1 条目
			if d.TargetID != "" {
				if !m.updateL1Entry(ctx, scope, d.TargetID, d.Content, d.Decision) {
					// 更新失败，不计入 promoted
					continue
				}
			}
			promoted++

		case DecisionSkip:
			// 无操作

		default:
			m.logger.Warnw("consolidator: unknown decision from LLM, skipping",
				"decision", d.Decision, "source_id", d.SourceID)
		}
	}

	// 标记已处理
	if len(processedIDs) > 0 {
		_ = m.store.MarkProcessed(ctx, scope, processedIDs)
	}

	span.SetAttributes(attribute.Int("promoted", promoted))
	m.logger.Infow("consolidation complete",
		"scope", scope.Key(),
		"l0_processed", len(processedIDs),
		"l1_promoted", promoted)

	return promoted, nil
}

// updateL1Entry 更新或合并一条 L1 记忆。
// 返回 true 表示更新成功，false 表示失败（条目未找到或写入出错）。
func (m *TieredManager) updateL1Entry(ctx context.Context, scope Scope, targetID, newContent string, decision ConsolidateDecision) bool {
	existing, err := m.store.GetAll(ctx, Tier1LongTerm, scope)
	if err != nil {
		m.logger.Warnw("updateL1Entry: failed to get existing L1 entries",
			"scope", scope.Key(), "target_id", targetID, "err", err)
		return false
	}

	for _, e := range existing {
		if e.ID != targetID {
			continue
		}

		var content string
		if decision == DecisionMerge {
			content = e.Content + "\n" + newContent
		} else {
			content = newContent
		}

		// 原子性替换：在单个锁内完成删除旧条目和写入新条目
		newEntry := TieredEntry{
			Entry: Entry{
				ID:             e.ID,
				Scope:          scope,
				Content:        content,
				Category:       e.Category,
				Source:         e.Source,
				Importance:     e.Importance,
				Metadata:       e.Metadata,
				CreatedAt:      e.CreatedAt,
				LastAccessedAt: e.LastAccessedAt,
			},
			Tier:         Tier1LongTerm,
			PromotedFrom: e.PromotedFrom,
		}

		if err := m.store.Replace(ctx, Tier1LongTerm, scope, targetID, newEntry); err != nil {
			m.logger.Warnw("updateL1Entry: atomic replace failed",
				"scope", scope.Key(), "target_id", targetID, "err", err)
			return false
		}

		return true
	}

	m.logger.Warnw("updateL1Entry: target entry not found",
		"scope", scope.Key(), "target_id", targetID)
	return false
}

// maybeConsolidate 检查是否需要触发巩固（带防抖）。
// 防抖检查和标记更新在同一个临界区内完成，避免竞态条件导致重复触发。
func (m *TieredManager) maybeConsolidate(ctx context.Context, scope Scope) {
	scopeKey := scope.Key()

	m.mu.Lock()
	// 防抖：同一 scope 至少间隔 consolidateDebounce
	if last, ok := m.lastConsolidated[scopeKey]; ok && time.Since(last) < m.consolidateDebounce {
		m.mu.Unlock()
		return
	}

	// 检查阈值（在持锁状态下检查，避免 TOCTOU 竞态）
	count, err := m.store.Count(ctx, Tier0Working, scope)
	if err != nil {
		m.mu.Unlock()
		return
	}

	if count < m.consolidateThreshold {
		m.mu.Unlock()
		return
	}

	// 标记已触发（原子操作：check + update 在同一锁内）
	m.lastConsolidated[scopeKey] = time.Now()
	m.mu.Unlock()

	// 异步执行，不阻塞写入
	go func() {
		bgCtx := context.Background()
		if _, err := m.Consolidate(bgCtx, scope); err != nil {
			m.logger.Warnw("auto consolidation failed", "scope", scopeKey, "err", err)
		}
	}()
}

// RetrieveMerged 从多个层级合并检索记忆。
// 检索顺序：L3(profile) → L2(episodic) → L1(long-term) → L0(working)
// 总条目数不超过 limit。
func (m *TieredManager) RetrieveMerged(ctx context.Context, scopes []Scope, limit int) ([]TieredEntry, error) {
	if limit <= 0 {
		limit = 20
	}

	var results []TieredEntry

	// L3: 画像（最稳定，全量注入）
	for _, scope := range scopes {
		profile, err := m.store.Retrieve(ctx, Tier3Profile, []Scope{scope}, 5)
		if err != nil {
			m.logger.Warnw("RetrieveMerged: failed to retrieve L3 profile", "scope", scope.Key(), "err", err)
		}
		results = append(results, profile...)
	}

	// L2: 场景
	for _, scope := range scopes {
		scenes, err := m.store.Retrieve(ctx, Tier2Episodic, []Scope{scope}, 5)
		if err != nil {
			m.logger.Warnw("RetrieveMerged: failed to retrieve L2 episodic", "scope", scope.Key(), "err", err)
		}
		results = append(results, scenes...)
	}

	// L1: 长期记忆
	l1Quota := limit / 2
	if l1Quota < 1 {
		l1Quota = 1
	}
	for _, scope := range scopes {
		longterm, err := m.store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, l1Quota)
		if err != nil {
			m.logger.Warnw("RetrieveMerged: failed to retrieve L1 longterm", "scope", scope.Key(), "err", err)
		}
		results = append(results, longterm...)
	}

	// L0: 工作记忆（最近，填满剩余配额）
	remaining := limit - len(results)
	if remaining > 0 {
		for _, scope := range scopes {
			working, err := m.store.Retrieve(ctx, Tier0Working, []Scope{scope}, remaining)
			if err != nil {
				m.logger.Warnw("RetrieveMerged: failed to retrieve L0 working", "scope", scope.Key(), "err", err)
			}
			results = append(results, working...)
			remaining -= len(working)
			if remaining <= 0 {
				break
			}
		}
	}

	// 总数限制
	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// RetrieveByTier 从指定层级检索记忆。
func (m *TieredManager) RetrieveByTier(ctx context.Context, tier MemoryTier, scopes []Scope, limit int) ([]TieredEntry, error) {
	return m.store.Retrieve(ctx, tier, scopes, limit)
}

// Aggregate 执行 L1→L2 场景聚合。
// 将指定 scope 的长期记忆聚合为场景摘要，写入 L2。
// 需要在 NewTieredManager 时配置 Aggregator，否则返回错误。
func (m *TieredManager) Aggregate(ctx context.Context, scope Scope) (int, error) {
	if m.aggregator == nil {
		return 0, fmt.Errorf("aggregate: no Aggregator configured")
	}

	l1Entries, err := m.store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, 100)
	if err != nil {
		return 0, errs.Wrap(err, "aggregate: get L1 entries")
	}
	if len(l1Entries) == 0 {
		return 0, nil
	}

	scenes, err := m.aggregator.Aggregate(ctx, l1Entries)
	if err != nil {
		return 0, errs.Wrap(err, "aggregate: aggregator failed")
	}

	for _, scene := range scenes {
		scene.Scope = scope
		if err := m.store.Append(ctx, scene); err != nil {
			m.logger.Warnw("aggregate: failed to write L2 scene", "err", err)
			continue
		}
	}

	m.logger.Infow("aggregation complete",
		"scope", scope.Key(),
		"l1_input", len(l1Entries),
		"l2_scenes", len(scenes))

	return len(scenes), nil
}

// ExtractProfile 执行 L2→L3 画像提取。
// 从长期和场景记忆中蒸馏出稳定的用户画像，写入 L3。
// 需要在 NewTieredManager 时配置 Profiler，否则返回错误。
func (m *TieredManager) ExtractProfile(ctx context.Context, scope Scope) (int, error) {
	if m.profiler == nil {
		return 0, fmt.Errorf("extract profile: no Profiler configured")
	}

	l1Entries, err := m.store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, 50)
	if err != nil {
		return 0, errs.Wrap(err, "extract profile: get L1 entries")
	}
	l2Entries, err := m.store.Retrieve(ctx, Tier2Episodic, []Scope{scope}, 20)
	if err != nil {
		return 0, errs.Wrap(err, "extract profile: get L2 entries")
	}
	if len(l1Entries) == 0 && len(l2Entries) == 0 {
		return 0, nil
	}

	existing, err := m.store.Retrieve(ctx, Tier3Profile, []Scope{scope}, 20)
	if err != nil {
		m.logger.Warnw("extract profile: failed to get existing L3", "err", err)
		existing = nil
	}

	items, err := m.profiler.ExtractProfile(ctx, l1Entries, l2Entries, existing)
	if err != nil {
		return 0, errs.Wrap(err, "extract profile: profiler failed")
	}

	for _, item := range items {
		entry := Entry{
			Scope:      scope,
			Content:    item.Content,
			Category:   item.Type,
			Source:     "profiler",
			Importance: item.Confidence,
		}
		if err := m.WriteProfile(ctx, entry); err != nil {
			m.logger.Warnw("extract profile: failed to write L3 entry", "err", err)
			continue
		}
	}

	m.logger.Infow("profile extraction complete",
		"scope", scope.Key(),
		"profile_items", len(items))

	return len(items), nil
}

// RunGC 执行 GC（清理过期 L0 条目）。
func (m *TieredManager) RunGC(ctx context.Context) int {
	removed := m.store.GC(ctx)
	if removed > 0 {
		m.logger.Debugw("GC cleaned expired L0 entries", "removed", removed)
	}
	return removed
}

// Store 返回底层分层存储（供高级操作）。
func (m *TieredManager) Store() *TieredStore {
	return m.store
}
