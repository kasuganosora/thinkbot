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
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Dreaming — 后台记忆巩固系统（三相位管线）
//
// 受认知科学睡眠周期启发的异步记忆整理机制。
// L0 工作记忆通过 Light → REM → Deep 三阶段处理转化为持久 L1 长期记忆。
//
// 与 Consolidator 的区别：
//   - Consolidator 实时触发，小批量快速
//   - Dreaming 定时调度，大批量深度分析，证据驱动评分门控
//   - 仅 Deep 相位写入 L1，严格隔离噪声
// ============================================================================

// DreamPhase 标识梦境阶段。
type DreamPhase string

const (
	PhaseLight DreamPhase = "light"
	PhaseREM   DreamPhase = "rem"
	PhaseDeep  DreamPhase = "deep"
)

// DreamState 梦境系统运行状态。
type DreamState string

const (
	DreamIdle     DreamState = "idle"
	DreamRunning  DreamState = "running"
	DreamDisabled DreamState = "disabled"
)

// DreamCandidate 记忆候选（跨相位累积信号）。
type DreamCandidate struct {
	Key           string    `json:"key"`
	Content       string    `json:"content"`
	SourceIDs     []string  `json:"source_ids"`
	Scope         Scope     `json:"scope"`
	Category      string    `json:"category,omitempty"`
	LightHits     int       `json:"light_hits,omitempty"`
	Theme         string    `json:"theme,omitempty"`
	REMHits       int       `json:"rem_hits,omitempty"`
	RecallCount   int       `json:"recall_count,omitempty"`
	UniqueQueries int       `json:"unique_queries,omitempty"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	Score         float64   `json:"score,omitempty"`
	Promoted      bool      `json:"promoted,omitempty"`

	// seenQueries 追踪已记录的查询（用于 UniqueQueries 去重）。
	// 不序列化，运行时状态。
	seenQueries map[string]struct{} `json:"-"`
}

// ScoreBreakdown 各评分信号子分数。
type ScoreBreakdown struct {
	Relevance     float64 `json:"relevance"`
	Frequency     float64 `json:"frequency"`
	Diversity     float64 `json:"diversity"`
	Recency       float64 `json:"recency"`
	Consolidation float64 `json:"consolidation"`
	Richness      float64 `json:"richness"`
}

// Scoring weights (合计 = 1.0)
const (
	WeightRelevance     = 0.30
	WeightFrequency     = 0.24
	WeightDiversity     = 0.15
	WeightRecency       = 0.15
	WeightConsolidation = 0.10
	WeightRichness      = 0.06
	LightEnhanceCap     = 0.05
	REMEnhanceCap       = 0.08
)

// DreamReport 一次梦境运行的完整报告。
type DreamReport struct {
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      time.Time  `json:"finished_at"`
	Phase           DreamPhase `json:"phase"`
	LightIngested   int        `json:"light_ingested"`
	LightDeduped    int        `json:"light_deduped"`
	LightDropped    int        `json:"light_dropped"`
	REMThemes       int        `json:"rem_themes"`
	REMCandidates   int        `json:"rem_candidates"`
	DeepScored      int        `json:"deep_scored"`
	DeepPassed      int        `json:"deep_passed"`
	DeepPromoted    int        `json:"deep_promoted"`
	SkippedInactive int        `json:"skipped_inactive"`
	Error           string     `json:"error,omitempty"`
}

// Duration 返回本次梦境耗时。
func (r *DreamReport) Duration() time.Duration {
	if r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// ============================================================================
// DreamConfig
// ============================================================================

// DreamConfig 配置梦境系统。
type DreamConfig struct {
	Enabled              bool
	Schedule             string  // cron 表达式，默认 "0 3 * * *"
	Model                string  // LLM 模型名（从 bot 主模型/经济模型读取）
	ActiveThresholdHours float64 // 活跃度阈值：仅处理过去 N 小时内有 L0 写入的 scope，0=不过滤。默认 24。
	Scopes               []Scope
	Light                LightPhaseConfig
	REM                  REMPhaseConfig
	Deep                 DeepPhaseConfig
	JaccardThreshold     float64
	MaxDreamTokens       int
	VerboseLogging       bool
}

// LightPhaseConfig 浅睡眠阶段配置。
type LightPhaseConfig struct {
	LookbackDays  int
	MaxCandidates int
}

// REMPhaseConfig REM 阶段配置。
type REMPhaseConfig struct {
	LookbackDays       int
	MaxThemes          int
	MinPatternStrength float64
}

// DeepPhaseConfig 深睡眠阶段配置。
type DeepPhaseConfig struct {
	MinScore            float64
	MinRecallCount      int
	MinUniqueQueries    int
	MaxPromotions       int
	RecencyHalfLifeDays int
	MaxAgeDays          int
}

// DefaultDreamConfig 返回默认配置。
func DefaultDreamConfig() DreamConfig {
	return DreamConfig{
		Enabled:          false,
		Schedule:         "0 3 * * *",
		JaccardThreshold: 0.9,
		MaxDreamTokens:   10000,
		Light: LightPhaseConfig{
			LookbackDays:  2,
			MaxCandidates: 100,
		},
		REM: REMPhaseConfig{
			LookbackDays:       7,
			MaxThemes:          10,
			MinPatternStrength: 0.75,
		},
		Deep: DeepPhaseConfig{
			MinScore:            0.8,
			MinRecallCount:      3,
			MinUniqueQueries:    3,
			MaxPromotions:       10,
			RecencyHalfLifeDays: 14,
			MaxAgeDays:          30,
		},
	}
}

// ============================================================================
// DreamManager
// ============================================================================

// DreamManager 协调三相位梦境管线。
type DreamManager struct {
	config     DreamConfig
	manager    *TieredManager
	provider   llm.Provider
	model      string
	tracer     trace.Tracer
	logger     *zap.SugaredLogger
	mu         sync.Mutex
	state      DreamState
	report     *DreamReport
	candidates map[string]*DreamCandidate
	dreamDiary []string

	// botProfiler Bot 自我画像提取器（可选）。
	// 注入后，梦境管线会在 Deep 相位后对 BotScope 执行画像提取。
	botProfiler *BotProfileProfiler

	// onBotProfileUpdated 回调：Bot 画像更新后触发（可选）。
	// 调用方可在此通知 AdaptiveEngagementSyncer 刷新参数。
	onBotProfileUpdated func(botID string, result *BotProfileResult)
}

// NewDreamManager 创建梦境管理器。
// model 从 bot 配置中的主模型/经济模型读取，用于 Light/REM 相位的 LLM 调用。
func NewDreamManager(
	config DreamConfig,
	manager *TieredManager,
	provider llm.Provider,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *DreamManager {
	if config.Schedule == "" {
		config.Schedule = "0 3 * * *"
	}
	if config.JaccardThreshold <= 0 {
		config.JaccardThreshold = 0.9
	}
	state := DreamIdle
	if !config.Enabled {
		state = DreamDisabled
	}
	return &DreamManager{
		config:     config,
		manager:    manager,
		provider:   provider,
		model:      config.Model,
		tracer:     tp.Tracer("github.com/kasuganosora/thinkbot/agent/memory/dreaming"),
		logger:     logger.With("component", "dreaming"),
		state:      state,
		candidates: make(map[string]*DreamCandidate),
	}
}

// SetBotProfiler 注入 Bot 自我画像提取器。
func (d *DreamManager) SetBotProfiler(profiler *BotProfileProfiler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.botProfiler = profiler
}

// SetOnBotProfileUpdated 设置 Bot 画像更新回调。
func (d *DreamManager) SetOnBotProfileUpdated(cb func(botID string, result *BotProfileResult)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onBotProfileUpdated = cb
}

// State 返回当前状态。
func (d *DreamManager) State() DreamState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// LastReport 返回最近一次运行报告。
func (d *DreamManager) LastReport() *DreamReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.report
}

// DreamDiary 返回梦境日记。
func (d *DreamManager) DreamDiary() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.dreamDiary))
	copy(out, d.dreamDiary)
	return out
}

// Enable 启用梦境系统。
func (d *DreamManager) Enable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.Enabled = true
	if d.state == DreamDisabled {
		d.state = DreamIdle
	}
}

// Disable 禁用梦境系统。
func (d *DreamManager) Disable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.Enabled = false
	d.state = DreamDisabled
}

// StagedCandidates 返回当前 staged candidates 快照（用于调试/测试）。
func (d *DreamManager) StagedCandidates() []DreamCandidate {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DreamCandidate, 0, len(d.candidates))
	for _, c := range d.candidates {
		out = append(out, *c)
	}
	return out
}

// RecordRecall 记录一次候选被召回（外部检索系统调用）。
func (d *DreamManager) RecordRecall(key, query string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.candidates[key]; ok {
		c.RecallCount++
		if c.seenQueries == nil {
			c.seenQueries = make(map[string]struct{})
		}
		queryKey := strings.TrimSpace(strings.ToLower(query))
		if _, exists := c.seenQueries[queryKey]; !exists {
			c.seenQueries[queryKey] = struct{}{}
			c.UniqueQueries++
		}
	}
}

// Run 执行完整梦境管线（Light → REM → Deep）。
func (d *DreamManager) Run(ctx context.Context) (*DreamReport, error) {
	d.mu.Lock()
	if d.state == DreamDisabled {
		d.mu.Unlock()
		return nil, fmt.Errorf("dreaming: system is disabled")
	}
	if d.state == DreamRunning {
		d.mu.Unlock()
		return nil, fmt.Errorf("dreaming: already running")
	}
	d.state = DreamRunning
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.state = DreamIdle
		d.mu.Unlock()
	}()

	report := &DreamReport{StartedAt: time.Now()}
	d.mu.Lock()
	d.report = report
	d.mu.Unlock()

	// 无论成功或失败都记录梦境日记
	defer d.appendDreamDiary(report)

	ctx, span := d.tracer.Start(ctx, "memory.dreaming.run")
	defer span.End()

	d.logger.Info("dreaming pipeline started")

	scopes := d.config.Scopes
	if len(scopes) == 0 {
		scopes = d.discoverScopes(ctx)
	}
	if len(scopes) == 0 {
		report.FinishedAt = time.Now()
		report.Error = "no scopes to process"
		return report, nil
	}

	// 活跃度过滤：跳过指定时间内无记忆写入的僵尸 scope
	// ActiveThresholdHours=0 时跳过过滤
	threshold := d.config.ActiveThresholdHours
	var activeScopes []Scope
	skipped := 0
	if threshold > 0 {
		activeScopes = make([]Scope, 0, len(scopes))
		for _, s := range scopes {
			if d.manager.store.HasRecentActivity(ctx, s, threshold) {
				activeScopes = append(activeScopes, s)
			}
		}
		skipped = len(scopes) - len(activeScopes)
	} else {
		activeScopes = scopes
	}
	report.SkippedInactive = skipped
	if skipped > 0 {
		d.logger.Infow("dreaming: skipped inactive scopes",
			"total", len(scopes), "active", len(activeScopes), "skipped", skipped)
	}
	if len(activeScopes) == 0 {
		report.FinishedAt = time.Now()
		report.Phase = PhaseDeep
		d.logger.Info("dreaming: no active scopes, skipping")
		return report, nil
	}

	// Phase 1: Light
	lightRes, err := d.runLight(ctx, activeScopes)
	if err != nil {
		report.FinishedAt = time.Now()
		report.Error = fmt.Sprintf("light: %v", err)
		span.RecordError(err)
		return report, errs.Wrap(err, "dreaming light")
	}
	report.LightIngested = lightRes.ingested
	report.LightDeduped = lightRes.deduped
	report.LightDropped = lightRes.dropped

	if lightRes.deduped == 0 {
		// 检查是否有已分期的候选（来自之前的 Run）
		// 即使本轮 Light 没有新候选，可能有待 REM 聚类和 Deep 评分的旧候选
		d.mu.Lock()
		stagedCount := len(d.candidates)
		d.mu.Unlock()
		if stagedCount == 0 {
			report.FinishedAt = time.Now()
			report.Phase = PhaseDeep
			d.logger.Info("dreaming: no candidates, skipping")
			return report, nil
		}
		// 有已分期的候选，继续执行 REM + Deep
		d.logger.Infow("dreaming: reuse staged candidates",
			"count", stagedCount)
	}

	// Phase 2: REM
	remRes, err := d.runREM(ctx)
	if err != nil {
		report.FinishedAt = time.Now()
		report.Error = fmt.Sprintf("rem: %v", err)
		span.RecordError(err)
		return report, errs.Wrap(err, "dreaming REM")
	}
	report.REMThemes = remRes.themes
	report.REMCandidates = remRes.candidates

	// Phase 3: Deep
	deepRes, err := d.runDeep(ctx)
	if err != nil {
		report.FinishedAt = time.Now()
		report.Error = fmt.Sprintf("deep: %v", err)
		span.RecordError(err)
		return report, errs.Wrap(err, "dreaming deep")
	}
	report.DeepScored = deepRes.scored
	report.DeepPassed = deepRes.passed
	report.DeepPromoted = deepRes.promoted
	report.FinishedAt = time.Now()
	report.Phase = PhaseDeep

	// Phase 4 (Optional): Bot 自我画像提取
	// 对 BotScope 的 L1+L2 记忆执行画像蒸馏，更新 Bot 的 L3 自我认知。
	if d.botProfiler != nil {
		d.extractBotProfiles(ctx, activeScopes)
	}

	span.SetAttributes(
		attribute.Int("ingested", report.LightIngested),
		attribute.Int("promoted", report.DeepPromoted),
	)

	d.logger.Infow("dreaming pipeline complete",
		"duration", report.Duration(),
		"promoted", report.DeepPromoted)

	return report, nil
}

// discoverScopes 从 TieredStore 快照中发现所有 scope。
func (d *DreamManager) discoverScopes(_ context.Context) []Scope {
	snap := d.manager.store.Snapshot()
	seen := make(map[string]bool)
	var scopes []Scope
	for _, scopeMap := range snap {
		for k := range scopeMap {
			s := parseScopeFromKey(k)
			if s.Kind != "" && !seen[s.Key()] {
				seen[s.Key()] = true
				scopes = append(scopes, s)
			}
		}
	}
	return scopes
}

// parseScopeFromKey 从 "L0_working|channel:xxx" 中提取 scope。
func parseScopeFromKey(key string) Scope {
	pipe := -1
	for i, c := range key {
		if c == '|' {
			pipe = i
			break
		}
	}
	if pipe < 0 || pipe+1 >= len(key) {
		return Scope{}
	}
	rest := key[pipe+1:]
	colon := -1
	for i, c := range rest {
		if c == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return Scope{Kind: ScopeKind(rest)}
	}
	return Scope{Kind: ScopeKind(rest[:colon]), ID: rest[colon+1:]}
}

// extractBotProfiles 对活跃 scope 中的 BotScope 执行自我画像提取。
func (d *DreamManager) extractBotProfiles(ctx context.Context, activeScopes []Scope) {
	ctx, span := d.tracer.Start(ctx, "memory.dreaming.bot_profile",
		trace.WithAttributes(
			attribute.Int("active_scopes", len(activeScopes)),
		))
	defer span.End()
	logger := traceid.WithLoggerFrom(ctx, d.logger)

	var extractedCount int
	defer func() {
		span.SetAttributes(attribute.Int("bot_profiles_extracted", extractedCount))
	}()

	for _, scope := range activeScopes {
		if scope.Kind != ScopeBot {
			continue
		}

		botID := scope.ID
		if botID == "" {
			continue
		}

		logger.Debugw("dreaming: extracting bot profile", "bot_id", botID)

		// 获取 BotScope 的 L1 和 L2 记忆
		l1Entries, err := d.manager.store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, 50)
		if err != nil {
			logger.Warnw("dreaming: failed to get bot L1 entries",
				"bot_id", botID, "err", err)
			continue
		}
		l2Entries, err := d.manager.store.Retrieve(ctx, Tier2Episodic, []Scope{scope}, 20)
		if err != nil {
			logger.Warnw("dreaming: failed to get bot L2 entries",
				"bot_id", botID, "err", err)
			l2Entries = nil
		}

		if len(l1Entries) == 0 && len(l2Entries) == 0 {
			continue
		}

		// 获取已有 L3 画像（供参考）
		existing, err := d.manager.store.Retrieve(ctx, Tier3Profile, []Scope{scope}, 10)
		if err != nil {
			logger.Warnw("dreaming: failed to get existing bot L3",
				"bot_id", botID, "err", err)
			existing = nil
		}

		// 调用 BotProfileProfiler
		profile, err := d.botProfiler.ExtractProfile(ctx, l1Entries, l2Entries, existing)
		if err != nil {
			logger.Warnw("dreaming: bot profile extraction failed",
				"bot_id", botID, "err", err)
			continue
		}
		if profile == nil {
			continue
		}

		// 将画像写入 L3（BotScope）
		d.persistBotProfile(ctx, scope, profile)

		extractedCount++

		// 回调通知
		if d.onBotProfileUpdated != nil {
			d.onBotProfileUpdated(botID, profile)
		}
	}
}

// persistBotProfile 将 Bot 自我画像写入 L3。
func (d *DreamManager) persistBotProfile(ctx context.Context, scope Scope, profile *BotProfileResult) {
	logger := traceid.WithLoggerFrom(ctx, d.logger)

	entry := Entry{
		Scope:      scope,
		Content:    profile.Personality,
		Category:   "bot_personality",
		Source:     "bot_profiler",
		Importance: profile.Confidence,
		Metadata: map[string]any{
			"energy_level":     profile.EnergyLevel,
			"patience":         profile.Patience,
			"verbosity":        profile.Verbosity,
			"preferred_topics": profile.PreferredTopics,
			"extracted_at":     time.Now(),
		},
	}

	if err := d.manager.WriteProfile(ctx, entry); err != nil {
		logger.Warnw("dreaming: failed to write bot profile", "err", err)
		return
	}

	logger.Infow("dreaming: bot profile written to L3",
		"bot_id", scope.ID,
		"personality", profile.Personality,
		"energy", profile.EnergyLevel,
		"patience", profile.Patience,
		"confidence", profile.Confidence)
}

// appendDreamDiary 追加一条梦境日记。
func (d *DreamManager) appendDreamDiary(report *DreamReport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry := fmt.Sprintf("## Dream — %s\n"+
		"- Duration: %v\n"+
		"- Light: ingested=%d, deduped=%d, dropped=%d\n"+
		"- REM: themes=%d, candidates=%d\n"+
		"- Deep: scored=%d, passed=%d, promoted=%d\n",
		report.StartedAt.Format("2006-01-02 15:04:05"),
		report.Duration(),
		report.LightIngested, report.LightDeduped, report.LightDropped,
		report.REMThemes, report.REMCandidates,
		report.DeepScored, report.DeepPassed, report.DeepPromoted)
	if report.SkippedInactive > 0 {
		entry += fmt.Sprintf("- Skipped (inactive): %d scopes\n", report.SkippedInactive)
	}
	if report.Error != "" {
		entry += fmt.Sprintf("- Error: %s\n", report.Error)
	}
	d.dreamDiary = append(d.dreamDiary, entry)
	// 限制日记长度
	if len(d.dreamDiary) > 100 {
		d.dreamDiary = d.dreamDiary[len(d.dreamDiary)-100:]
	}
}
