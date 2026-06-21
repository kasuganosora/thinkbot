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
				f.updateExisting(ctx, store, scope, d)
				result.Updated++
			} else {
				// 有 UPDATE 决策但没 TargetID，降级为 ADD
				_ = store.Append(ctx, TieredEntry{
					Entry: Entry{
						Scope:      scope,
						Content:    d.Fact.Content,
						Category:   d.Fact.Category,
						Source:     "formation",
						Importance: d.Fact.Importance,
					},
					Tier:         Tier1LongTerm,
					PromotedFrom: Tier0Working,
				})
				result.Added++
			}

		case DecisionSkip:
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
	sb.WriteString("## 对话内容\n\n")
	sb.WriteString("用户: ")
	sb.WriteString(StripThinking(userContent))
	sb.WriteString("\n助手: ")
	sb.WriteString(StripThinking(assistantContent))
	sb.WriteString("\n\n## 任务\n")
	sb.WriteString("从上述对话中提取值得长期记忆的事实。只提取有信息量的内容，忽略闲聊/问候/客套。\n")
	sb.WriteString("输出 JSON 数组:\n")
	sb.WriteString("```json\n")
	sb.WriteString(`[{"content":"用户使用 Go 语言","category":"fact","importance":0.8}]`)
	sb.WriteString("\n```")
	sb.WriteString("\ncategory: fact(事实) / preference(偏好) / event(事件) / observation(观察)")
	sb.WriteString("\nimportance: 0.0~1.0，越高越重要。闲聊/问候/客套不输出。\n")
	sb.WriteString("如果对话中没有值得记忆的内容，输出空数组 []")

	resp, err := f.config.Provider.DoGenerate(ctx, llm.GenerateParams{
		Model:    f.config.Model,
		System:   f.config.SystemPrompt,
		Messages: []llm.Message{llm.UserMessage(sb.String())},
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
	sb.WriteString("## 新提取的事实\n\n")
	for i, fact := range facts {
		sb.WriteString(strings.Repeat("-", 20))
		sb.WriteString("\n")
		sb.WriteString(fact.Content)
		sb.WriteString("\n")
		_ = i
	}

	sb.WriteString("\n## 已有的长期记忆（供去重参考）\n\n")
	for _, e := range existing {
		sb.WriteString("[")
		sb.WriteString(e.ID)
		sb.WriteString("] ")
		sb.WriteString(e.Content)
		sb.WriteString("\n")
	}

	sb.WriteString("\n## 任务\n")
	sb.WriteString("对每条新事实做出决策。输出 JSON 数组:\n")
	sb.WriteString("```json\n")
	sb.WriteString(`[{"fact":{"content":"...","category":"fact","importance":0.8},"action":"ADD","reason":"新事实"}]`)
	sb.WriteString("\n```\n")
	sb.WriteString("action: ADD(全新) / UPDATE(更新已有，需提供 target_id) / SKIP(已存在或无价值)\n")
	sb.WriteString("事实顺序与输入顺序一致。")

	resp, err := f.config.Provider.DoGenerate(ctx, llm.GenerateParams{
		Model:    f.config.Model,
		System:   f.config.SystemPrompt,
		Messages: []llm.Message{llm.UserMessage(sb.String())},
	})
	if err != nil {
		return nil, errs.Wrap(err, "formation: LLM decide call")
	}

	var decisions []FactDecision
	if err := strutil.ExtractJSON(resp.Text, &decisions); err != nil {
		return nil, errs.Wrap(err, "formation: parse decide JSON")
	}

	return decisions, nil
}

// updateExisting 更新已有 L1 记忆。
func (f *FormationPipeline) updateExisting(ctx context.Context, store *TieredStore, scope Scope, d FactDecision) {
	all, err := store.GetAll(ctx, Tier1LongTerm, scope)
	if err != nil {
		return
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
		// 删除旧的，写入新的
		_ = store.Delete(ctx, Tier1LongTerm, scope, d.TargetID)
		_ = store.Append(ctx, TieredEntry{
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
		})
		break
	}
}

const defaultFormationPrompt = `你是一个记忆提取助手。你的任务是从对话中提取值得长期保存的事实。

规则：
1. 只提取有信息量的内容（事实、偏好、事件、重要观察）
2. 忽略闲聊、问候、客套、确认等无实质内容的对话
3. 评估每条事实的重要度（0.0~1.0）
4. 输出纯 JSON，不要其他文本

分类：
- fact: 客观事实（"用户使用 Go 语言"）
- preference: 偏好（"用户偏好简洁的回复"）
- event: 事件（"用户完成了部署"）
- observation: 观察（"用户对 Rust 感兴趣"）`
