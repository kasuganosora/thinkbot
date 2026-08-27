package memory

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// FormationPipeline — 对话后即时记忆提取管线
//
// 参考 Memoh 的 OnAfterChat Formation Pipeline：
//   Extract（LLM 提取事实）→ Gather（搜索现有记忆做候选）→ Decide（ADD/UPDATE/SKIP）→ Apply
//
// 与 Consolidator 的区别：
//   - Consolidator 是批量巩固（L0 积累到阈值后才触发）
//   - FormationPipeline 是即时提取（每轮对话后立即执行）
//   - FormationPipeline 可以直接写入 L1，也可以交给 TieredManager 管理
//
// 工作流程：
//  1. 对话结束 → ProcessTurn()
//  2. LLM 从本轮 user+assistant 中提取事实候选
//  3. 对每个候选搜索现有 L1 记忆做去重
//  4. LLM 决策 ADD/UPDATE/SKIP
//  5. 应用结果（写入 L1 或更新已有 L1）
// ============================================================================

// FormationConfig 配置即时记忆提取管线。
type FormationConfig struct {
	// Provider LLM 提供商。
	Provider llm.Provider
	// Model 指定模型（可选，建议用轻量模型）。
	Model *llm.Model
	// SystemPrompt 提取用的系统提示词（为空使用默认）。
	SystemPrompt string
	// MaxFactsPerTurn 单轮最多提取的事实数（默认 5）。
	MaxFactsPerTurn int
	// MinContentLen 触发提取的最小对话内容长度（短于此的跳过）。
	MinContentLen int
	// DedupScopeLimit 去重检索时从 L1 获取的候选条数（默认 20）。
	DedupScopeLimit int
}

// DefaultFormationConfig 返回默认配置。
func DefaultFormationConfig() FormationConfig {
	return FormationConfig{
		MaxFactsPerTurn: 5,
		MinContentLen:   20,
		DedupScopeLimit: 20,
		SystemPrompt:    defaultFormationPrompt,
	}
}

// FormationResult 单轮提取结果。
type FormationResult struct {
	Extracted int           `json:"extracted"`
	Added     int           `json:"added"`
	Updated   int           `json:"updated"`
	Skipped   int           `json:"skipped"`
	Duration  time.Duration `json:"-"`
}

// FactItem LLM 提取的单条事实。
type FactItem struct {
	Content    string  `json:"content"`
	Category   string  `json:"category,omitempty"`
	Importance float64 `json:"importance,omitempty"`
}

// FactDecision LLM 对单条事实的去重决策。
type FactDecision struct {
	Fact     FactItem            `json:"fact"`
	Action   ConsolidateDecision `json:"action"` // ADD / UPDATE / SKIP
	TargetID string              `json:"target_id,omitempty"`
	Reason   string              `json:"reason,omitempty"`
}

// FormationPipeline 对话后即时记忆提取。
//
// 每次 ProcessTurn 调用：
//  1. LLM 提取本轮对话中的事实
//  2. 搜索现有 L1 做去重候选
//  3. LLM 决策每条事实的处置
//  4. 写入或更新 L1
type FormationPipeline struct {
	config FormationConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewFormationPipeline 创建即时记忆提取管线。
func NewFormationPipeline(config FormationConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *FormationPipeline {
	if config.Provider == nil {
		panic("formation: config.Provider must not be nil")
	}
	if config.MaxFactsPerTurn <= 0 {
		config.MaxFactsPerTurn = 5
	}
	if config.MinContentLen <= 0 {
		config.MinContentLen = 20
	}
	if config.DedupScopeLimit <= 0 {
		config.DedupScopeLimit = 20
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaultFormationPrompt
	}
	return &FormationPipeline{
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/formation"),
		logger: logger.With("component", "memory_formation"),
	}
}

// ProcessTurn 处理一轮对话，提取记忆并写入。
//
// 参数：
//   - store: 分层存储（用于读取已有 L1 和写入新 L1）
//   - scope: 当前作用域
//   - userContent: 用户消息
//   - assistantContent: 助手回复
func (f *FormationPipeline) ProcessTurn(
	ctx context.Context,
	store *TieredStore,
	scope Scope,
	userContent, assistantContent string,
) (*FormationResult, error) {
	start := time.Now()
	result := &FormationResult{}

	// 1. 预检：内容太短直接跳过
	combined := StripThinking(userContent) + "\n" + StripThinking(assistantContent)
	combined = strings.TrimSpace(combined)
	if len([]rune(combined)) < f.config.MinContentLen {
		result.Duration = time.Since(start)
		return result, nil
	}

	ctx, span := f.tracer.Start(ctx, "memory.formation.process_turn",
		trace.WithAttributes(attribute.String("scope", scope.Key())))
	defer span.End()

	// 2. Extract: LLM 提取事实
	facts, err := f.extractFacts(ctx, userContent, assistantContent)
	if err != nil {
		span.RecordError(err)
		return nil, errs.Wrap(err, "formation: extract facts")
	}
	if len(facts) == 0 {
		result.Duration = time.Since(start)
		return result, nil
	}
	// 限制每轮最多提取数
	if len(facts) > f.config.MaxFactsPerTurn {
		facts = facts[:f.config.MaxFactsPerTurn]
	}
	result.Extracted = len(facts)

	// 3. Gather: 搜索现有 L1 记忆做去重候选
	existing, err := store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, f.config.DedupScopeLimit)
	if err != nil {
		f.logger.Warnw("formation: failed to get existing L1 for dedup", "err", err)
		existing = nil
	}

	// 4. Decide: LLM 对每条事实做去重决策
	decisions, err := f.decideActions(ctx, facts, existing)
	if err != nil {
		f.logger.Warnw("formation: decide failed, falling back to ADD all", "err", err)
		// 降级：全部 ADD
		for _, fact := range facts {
			decisions = append(decisions, FactDecision{
				Fact:   fact,
				Action: DecisionAdd,
			})
		}
	}

	// 5. Apply: 应用决策
	for _, d := range decisions {
		switch d.Action {
		case DecisionAdd:
			err := store.Append(ctx, TieredEntry{
				Entry: Entry{
					Scope:      scope,
					Content:    d.Fact.Content,
					Category:   d.Fact.Category,
					Source:     "formation",
					Importance: d.Fact.Importance,
					Metadata: map[string]any{
						"extracted_at": time.Now(),
					},
				},
				Tier:         Tier1LongTerm,
				PromotedFrom: Tier0Working,
			})
			if err != nil {
				f.logger.Warnw("formation: failed to write L1", "err", err)
				continue
			}
			result.Added++

		case DecisionUpdate:
			if d.TargetID != "" {
				if f.updateExisting(ctx, store, scope, d) {
					result.Updated++
				} else {
					// 更新失败，降级为 ADD 新条目
					f.appendAsNew(ctx, store, scope, d.Fact, result)
				}
			} else {
				// 有 UPDATE 决策但没 TargetID，降级为 ADD
				f.appendAsNew(ctx, store, scope, d.Fact, result)
			}

		case DecisionSkip:
			result.Skipped++

		default:
			// 未知 action（LLM 返回了非 ADD/UPDATE/SKIP 的值）
			f.logger.Warnw("formation: unknown action from LLM, skipping",
				"action", d.Action, "content_preview", strutil.Truncate(d.Fact.Content, 80))
			result.Skipped++
		}
	}

	result.Duration = time.Since(start)
	span.SetAttributes(
		attribute.Int("extracted", result.Extracted),
		attribute.Int("added", result.Added),
		attribute.Int("updated", result.Updated),
		attribute.Int("skipped", result.Skipped),
	)

	f.logger.Debugw("formation complete",
		"scope", scope.Key(),
		"extracted", result.Extracted,
		"added", result.Added,
		"updated", result.Updated,
		"skipped", result.Skipped,
		"duration", result.Duration)

	return result, nil
}

// extractFacts 调用 LLM 从对话中提取事实。
func (f *FormationPipeline) extractFacts(ctx context.Context, userContent, assistantContent string) ([]FactItem, error) {
	var sb strings.Builder
	sb.WriteString("## Conversation\n\n")
	sb.WriteString("User: ")
	sb.WriteString(StripThinking(userContent))
	sb.WriteString("\nAssistant: ")
	sb.WriteString(StripThinking(assistantContent))
	sb.WriteString("\n\n## Task\n")
	sb.WriteString("Extract facts from the conversation above that are worth remembering long term. Keep only informative content; ignore small talk, greetings and pleasantries.\n")
	sb.WriteString("Write each content value in the same language as the conversation.\n")
	sb.WriteString("Output a JSON array:\n")
	sb.WriteString("```json\n")
	sb.WriteString(`[{"content":"用户使用 Go 语言","category":"fact","importance":0.8}]`)
	sb.WriteString("\n```")
	sb.WriteString("\ncategory: fact | preference | event | observation")
	sb.WriteString("\nimportance: 0.0~1.0, higher means more important. NEVER emit small talk, greetings or pleasantries.\n")
	sb.WriteString("If the conversation contains nothing worth remembering, output an empty array [].")

	maxTokens := DefaultGenerationMaxTokens
	resp, err := f.config.Provider.DoGenerate(llm.WithStatsFeature(ctx, "memory_formation"), llm.GenerateParams{
		Model:     f.config.Model,
		System:    f.config.SystemPrompt,
		Messages:  []llm.Message{llm.UserMessage(sb.String())},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		return nil, errs.Wrap(err, "formation: LLM extract call")
	}

	var facts []FactItem
	if err := strutil.ExtractJSON(resp.Text, &facts); err != nil {
		f.logger.Warnw("formation: failed to parse facts JSON",
			"err", err,
			"preview", strutil.Truncate(resp.Text, 200))
		return nil, nil // 解析失败不报错，返回空
	}

	// 过滤空内容
	var filtered []FactItem
	for _, fact := range facts {
		if strings.TrimSpace(fact.Content) != "" {
			filtered = append(filtered, fact)
		}
	}

	return filtered, nil
}

// decideActions 调用 LLM 对每条事实做去重决策。
func (f *FormationPipeline) decideActions(ctx context.Context, facts []FactItem, existing []TieredEntry) ([]FactDecision, error) {
	if len(existing) == 0 {
		// 没有已有记忆，全部 ADD
		var decisions []FactDecision
		for _, fact := range facts {
			decisions = append(decisions, FactDecision{
				Fact:   fact,
				Action: DecisionAdd,
			})
		}
		return decisions, nil
	}

	var sb strings.Builder
	sb.WriteString("## Newly extracted facts\n\n")
	for _, fact := range facts {
		sb.WriteString(strings.Repeat("-", 20))
		sb.WriteString("\n")
		sb.WriteString(fact.Content)
		sb.WriteString("\n")
	}

	sb.WriteString("\n## Existing long-term memory (for deduplication)\n\n")
	for _, e := range existing {
		sb.WriteString("[")
		sb.WriteString(e.ID)
		sb.WriteString("] ")
		sb.WriteString(e.Content)
		sb.WriteString("\n")
	}

	sb.WriteString("\n## Task\n")
	sb.WriteString("Decide what to do with each new fact. Output a JSON array:\n")
	sb.WriteString("```json\n")
	sb.WriteString(`[{"fact":{"content":"...","category":"fact","importance":0.8},"action":"ADD","reason":"new fact"}]`)
	sb.WriteString("\n```\n")
	sb.WriteString("action: ADD (brand new) | UPDATE (supersedes an existing entry, MUST include target_id) | SKIP (already known or worthless)\n")
	sb.WriteString("IMPORTANT: emit one decision per input fact, in the exact same order as the input.")

	maxTokens := DefaultGenerationMaxTokens
	resp, err := f.config.Provider.DoGenerate(llm.WithStatsFeature(ctx, "memory_formation"), llm.GenerateParams{
		Model:     f.config.Model,
		System:    f.config.SystemPrompt,
		Messages:  []llm.Message{llm.UserMessage(sb.String())},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		return nil, errs.Wrap(err, "formation: LLM decide call")
	}

	var decisions []FactDecision
	if err := strutil.ExtractJSON(resp.Text, &decisions); err != nil {
		return nil, errs.Wrap(err, "formation: parse decide JSON")
	}

	// 校验：LLM 返回的决策数应与输入事实数一致
	if len(decisions) < len(facts) {
		f.logger.Warnw("formation: LLM returned fewer decisions than facts, padding with store",
			"facts", len(facts), "decisions", len(decisions))
		// 对缺少决策的 fact 默认保留
		for i := len(decisions); i < len(facts); i++ {
			decisions = append(decisions, FactDecision{
				Fact:   facts[i],
				Action: DecisionAdd,
				Reason: "default: LLM did not provide decision",
			})
		}
	}
	// 截断多余决策（LLM 幻觉）
	if len(decisions) > len(facts) {
		f.logger.Warnw("formation: LLM returned more decisions than facts, truncating",
			"facts", len(facts), "decisions", len(decisions))
		decisions = decisions[:len(facts)]
	}

	return decisions, nil
}

// updateExisting 更新已有 L1 记忆。返回是否成功找到并更新。
func (f *FormationPipeline) updateExisting(ctx context.Context, store *TieredStore, scope Scope, d FactDecision) bool {
	all, err := store.GetAll(ctx, Tier1LongTerm, scope)
	if err != nil {
		f.logger.Warnw("formation: GetAll for update failed", "err", err, "target_id", d.TargetID)
		return false
	}
	for _, e := range all {
		if e.ID != d.TargetID {
			continue
		}
		// 合并内容
		newContent := e.Content
		if !strings.Contains(e.Content, d.Fact.Content) {
			newContent = e.Content + "; " + d.Fact.Content
		}
		// 更新重要度（取较高值）
		importance := e.Importance
		if d.Fact.Importance > importance {
			importance = d.Fact.Importance
		}
		// 删除旧的，写入新的（原子性替换）
		if err := store.Replace(ctx, Tier1LongTerm, scope, d.TargetID, TieredEntry{
			Entry: Entry{
				ID:         d.TargetID,
				Scope:      scope,
				Content:    newContent,
				Category:   e.Category,
				Source:     e.Source,
				Importance: importance,
				Metadata:   e.Metadata,
			},
			Tier:         Tier1LongTerm,
			PromotedFrom: e.PromotedFrom,
		}); err != nil {
			f.logger.Warnw("formation: atomic replace failed", "err", err, "target_id", d.TargetID)
			return false
		}
		return true
	}
	f.logger.Warnw("formation: target entry not found for update", "target_id", d.TargetID)
	return false
}

// appendAsNew 将一条 fact 作为新 L1 记忆写入，成功时自增 result.Added。
func (f *FormationPipeline) appendAsNew(ctx context.Context, store *TieredStore, scope Scope, fact FactItem, result *FormationResult) {
	err := store.Append(ctx, TieredEntry{
		Entry: Entry{
			Scope:      scope,
			Content:    fact.Content,
			Category:   fact.Category,
			Source:     "formation",
			Importance: fact.Importance,
			Metadata: map[string]any{
				"extracted_at": time.Now(),
			},
		},
		Tier:         Tier1LongTerm,
		PromotedFrom: Tier0Working,
	})
	if err != nil {
		f.logger.Warnw("formation: fallback ADD failed", "err", err)
		return
	}
	result.Added++
}

const defaultFormationPrompt = `You are a memory extractor, an analysis component that pulls durable facts out of a conversation.

Rules:
1. Extract only informative content: facts, preferences, events and significant observations.
2. IGNORE small talk, greetings, pleasantries and bare acknowledgements. They carry no substance.
3. Score the importance of each fact from 0.0 to 1.0.
4. Output raw JSON and nothing else — no prose, no code fences.
5. Write each content value in the same language as the conversation.

Categories:
- fact: an objective fact ("用户使用 Go 语言")
- preference: a preference ("用户偏好简洁的回复")
- event: something that happened ("用户完成了部署")
- observation: an inferred observation ("用户对 Rust 感兴趣")`
