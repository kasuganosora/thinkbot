package memory

import (
	"context"
	"fmt"
	"sort"
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

// DreamPromotionReason 记录一条记忆被提升为 L1 的理由与依据。
//
// 持久化在晋升后 L1 条目的 metadata["dream_reason"] 中（嵌套 JSON 对象），
// 既供前端「最近晋升的记忆」面板逐条展示，也便于审计「为什么这条被记住了」。
//
// 设计要点（勿回退）：理由必须可解释。早期实现只存 dream_score 等标量，
// 排查时无法判断「达标的究竟是哪些信号、引用了哪些原条目」，形成黑盒。
// 这里把评分明细、通过的门控、REM 主题、引用的原 L0 条目 ID 一起固化，
// 使每一次提升都自带证据链。
type DreamPromotionReason struct {
	// Summary 一句话中文理由（人读）。
	Summary string `json:"summary"`
	// Score 最终混合分（启发式与 LLM 重要性混合后的总分）。
	Score float64 `json:"score"`
	// MinScore 通过晋升所依据的阈值。
	MinScore float64 `json:"min_score"`
	// PassedGates 通过的全部门控名（如 "score"/"rem"/"recall"/"queries"）。
	PassedGates []string `json:"passed_gates"`
	// Breakdown 各评分信号子分数。
	Breakdown ScoreBreakdown `json:"breakdown"`
	// Heuristic 纯启发式总分（未与 LLM 重要性混合前）。
	Heuristic float64 `json:"heuristic"`
	// LLMImportance LLM 评估的重要性（未使用 LLM 时为 0，omitempty 省略）。
	LLMImportance float64 `json:"llm_importance,omitempty"`
	// LightHits / REMHits 关键信号快照。
	LightHits int `json:"light_hits"`
	REMHits   int `json:"rem_hits"`
	// Theme 命中到的 REM 主题（空表示未聚类）。
	Theme string `json:"theme,omitempty"`
	// SourcePreview 引用的原 L0 条目原文预览（截断，最多 3 条）。
	// 让晋升理由在「不展开面板」的主视图即自带证据原文，自我解释，
	// 不必点开才能看到「凭什么原文提拔升」。与 source_entries 全量快照互补：
	// 此处是供人速读的摘要，source_entries 是供审计展开的全文。
	SourcePreview string `json:"source_preview,omitempty"`
}

// DreamSourceEntry 晋升时快照的「被引用的原始 L0 工作记忆条目」。
//
// 固化进 L1 条目 metadata["source_entries"]，使前端「最近晋升的记忆」面板可展开
// 查看原内容，而不只是 ID。之所以要快照而非查询时反查：
//   - L0 工作记忆 TTL=14 天，查询时原条目可能已过期删除，反查会得到空；
//   - 晋升发生在 Deep 相位、当夜 L0 必然还在，此刻抓取最可靠。
//
// 注意：content 来自 L0 原文，可能含 bot 自身回复（speaker="assistant"），
// 展示层应据 Speaker 标注来源，避免误读为用户事实。
type DreamSourceEntry struct {
	// ID 原 L0 条目 ID（"引用的原来的条目"）。
	ID string `json:"id"`
	// Content 原 L0 条目内容（快照，永久可读）。
	Content string `json:"content"`
	// Scope 原 L0 条目所属作用域（Scope.Key()）。
	Scope string `json:"scope"`
	// Speaker 说话人标签："user"=用户原话，"assistant"=bot 回复，"observer"=公开时间线观察，""=未知。
	Speaker string `json:"speaker,omitempty"`
}

// DreamPromotionRecord 一次梦境运行中单条晋升的结构化记录。
// 直接挂在 DreamReport.Promotions 上，使 trigger 响应可即时回显本轮晋升明细，
// 无需再单独查询存储。
type DreamPromotionRecord struct {
	ID       string               `json:"id"`
	Content  string               `json:"content"`
	Category string               `json:"category"`
	Scope    string               `json:"scope"`
	Score    float64              `json:"score"`
	Reason   DreamPromotionReason `json:"reason"`
	// SourceIDs 引用的原始 L0 工作记忆条目 ID（"引用的原来的条目"）。
	SourceIDs []string `json:"source_ids"`
	// SourceEntries 引用的原始 L0 工作记忆条目快照（内容 + 说话人），供面板展开原内容。
	SourceEntries []DreamSourceEntry `json:"source_entries,omitempty"`
	// PromotedAt 晋升时间。
	PromotedAt time.Time `json:"promoted_at"`
}

// Scoring weights (合计 = 1.0)
// 评分权重（合计 = 1.0）。
// 设计修正：Relevance/Diversity 依赖白天的召回信号（RecallCount/UniqueQueries），
// 而该信号仅在候选已被晋升到 L1 后、被检索系统召回时才会累积——
// 但晋升门控又要求这些信号，形成死锁（见 DefaultDreamConfig 注释）。
// 因此权重向「梦境管线自身可产出的信号」倾斜：Frequency(LightHits) /
// Recency / Consolidation(REMHits) / Richness，使有价值的近期事实能被晋升。
const (
	WeightRelevance     = 0.10
	WeightFrequency     = 0.30
	WeightDiversity     = 0.05
	WeightRecency       = 0.25
	WeightConsolidation = 0.20
	WeightRichness      = 0.10
	LightEnhanceCap     = 0.05
	REMEnhanceCap       = 0.08
)

// DreamReport 一次梦境运行的完整报告。
type DreamReport struct {
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    time.Time  `json:"finished_at"`
	Phase         DreamPhase `json:"phase"`
	LightIngested int        `json:"light_ingested"`
	LightDeduped  int        `json:"light_deduped"`
	LightDropped  int        `json:"light_dropped"`
	REMThemes     int        `json:"rem_themes"`
	REMCandidates int        `json:"rem_candidates"`
	DeepScored    int        `json:"deep_scored"`
	DeepPassed    int        `json:"deep_passed"`
	DeepPromoted  int        `json:"deep_promoted"`
	// Promotions 本轮 Deep 相位实际晋升的明细（含理由与原条目引用）。
	// 供 trigger 接口即时回显，也用于前端「最近晋升的记忆」面板。
	Promotions      []DreamPromotionRecord `json:"promotions,omitempty"`
	SkippedInactive int                    `json:"skipped_inactive"`
	UserProfiles    int                    `json:"user_profiles,omitempty"`
	BotProfiles     int                    `json:"bot_profiles,omitempty"`
	// Backfill 标记本轮是「补偿积压」运行（忽略活跃度门槛与 Light 的回溯窗口）。
	// 与常规运行共用同一条管线，仅放宽两道门槛，故必须能在报告里区分，
	// 否则排查时无法判断某次晋升来自增量还是补偿。
	Backfill bool `json:"backfill,omitempty"`
	// BackfillScanned 补偿模式下扫描到的 TTL 内未处理 L0 条目数（常规运行为 0）。
	BackfillScanned int    `json:"backfill_scanned,omitempty"`
	Error           string `json:"error,omitempty"`
}

// DreamRunOptions 一次梦境运行的选项。
type DreamRunOptions struct {
	// Backfill 为 true 时进入「补偿积压」模式：
	//   - 跳过 ActiveThresholdHours 活跃度门槛（停摆 N 天后所有 scope 都会被判为不活跃，
	//     常规运行会整轮空转，积压的 L0 永远等不到处理）；
	//   - Light 阶段不再按 LookbackDays（默认 2 天）截断，改为处理 TTL 内全部
	//     未标记的 L0（IsExpired 仍然生效——过期的 L0 不该被救活）。
	//
	// 仍然生效的两道保险：L0 TTL（过期不处理）与 dream_processed 标记（幂等，不重复提取）。
	Backfill bool
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
	MinScore         float64
	MinRecallCount   int
	MinUniqueQueries int
	// MinREMHits：候选需至少命中 N 个 REM 主题（跨候选反复出现）才晋升。
	// 0 = 不要求（默认），仅作为可选的强约束门控。
	MinREMHits          int
	MaxPromotions       int
	RecencyHalfLifeDays int
	MaxAgeDays          int
	// UseLLMImportance：用 LLM 评估「重要性」作为主导分数，替代纯启发式噪声
	// （Recency 线性衰减 + Richness 字数阈值对单条记忆几乎恒定满分，导致分数全挤在 0.5~0.7）。
	// 开启后：最终分 = LLM分*权重 + 启发式*(1-权重)。LLM 不可用时自动回退纯启发式。
	UseLLMImportance bool
	// LLMImportanceWeight：LLM 分占比（0~1）。0=未配置，runDeep 回退 0.55。
	LLMImportanceWeight float64
}

// DefaultLLMImportanceWeight 是 LLM 重要性占比的回退默认值（当配置为 0 时）。
const DefaultLLMImportanceWeight = 0.55

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
			// MinScore：基于「近期 + 丰富 + 反复出现」信号的晋升阈值。
			// 旧值 0.8 要求 Relevance(召回) 与 Diversity(查询) 信号，但白天的召回信号
			// 从未被写入生产代码（RecordRecall 无调用方），导致永远无法晋升。
			// 0.45 使「近期、内容丰富的事实」稳定跨越阈值，同时过滤闲聊/临时调试。
			MinScore: 0.45,
			// 召回/查询门控默认关闭（0）。这些信号只有候选晋升后召回才会累积，
			// 作为硬门控会造成死锁；保留配置项以便将来接入召回追踪后按需启用。
			MinRecallCount:   0,
			MinUniqueQueries: 0,
			MinREMHits:       0,
			MaxPromotions:    10,
			// RecencyHalfLifeDays=14：约两周半衰期，超过 ~4 周的内容评分趋近 0。
			RecencyHalfLifeDays: 14,
			MaxAgeDays:          30,
			// UseLLMImportance=true：用 LLM 直接评估每条记忆的重要性，替代纯启发式
			// 噪声（Recency 线性衰减/Richness 字数阈值对单条记忆几乎恒定满分）。
			// LLM 分占 0.55，启发式占 0.45；LLM 模型未配置或调用失败时回退纯启发式。
			UseLLMImportance:    true,
			LLMImportanceWeight: DefaultLLMImportanceWeight,
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

	// onRunComplete 回调：每次 Run 结束（成功或失败）后触发（可选）。
	// 用于把「最近一次运行」持久化，使运行状态页在刷新/重启后仍可展示。
	// 手动触发与 cron 定时触发都走 Run()，因此两条路径都会被记录。
	onRunComplete func(report *DreamReport)
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

// SetUserProfiler 注入用户画像提取器（对 user:* scope 写 L3）。
func (d *DreamManager) SetUserProfiler(p Profiler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.manager != nil {
		d.manager.profiler = p
	}
}

// State 返回当前状态。
func (d *DreamManager) State() DreamState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// SetOnRunComplete 注入「运行完成」回调（每次 Run 结束调用一次，含失败路径）。
func (d *DreamManager) SetOnRunComplete(cb func(report *DreamReport)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onRunComplete = cb
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

// Run 执行完整梦境管线（Light → REM → Deep），即常规增量运行。
func (d *DreamManager) Run(ctx context.Context) (*DreamReport, error) {
	return d.RunWithOptions(ctx, DreamRunOptions{})
}

// RunWithOptions 执行完整梦境管线，可指定运行选项（见 DreamRunOptions）。
//
// Backfill 模式用于补偿停摆期积压：放宽活跃度门槛与 Light 回溯窗口，
// 让「还活着但已经错过窗口」的 L0 重新有机会被巩固。
func (d *DreamManager) RunWithOptions(ctx context.Context, opts DreamRunOptions) (*DreamReport, error) {
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

	// 无论成功或失败都落库「最近一次运行」记录（运行状态页的唯一数据源）。
	// 先把回调取出再释放锁，避免在持锁状态下做文件 IO。
	defer func() {
		d.mu.Lock()
		cb := d.onRunComplete
		d.mu.Unlock()
		if cb != nil {
			cb(report)
		}
	}()

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

	report.Backfill = opts.Backfill

	// 活跃度过滤：跳过指定时间内无记忆写入的僵尸 scope
	// ActiveThresholdHours=0 时跳过过滤；补偿模式（Backfill）也整体跳过——
	// 停摆数天后所有 scope 必然不活跃，不跳过就一个 scope 都进不来。
	threshold := d.config.ActiveThresholdHours
	var activeScopes []Scope
	skipped := 0
	if threshold > 0 && !opts.Backfill {
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
	lightRes, err := d.runLight(ctx, activeScopes, opts)
	if err != nil {
		report.FinishedAt = time.Now()
		report.Error = fmt.Sprintf("light: %v", err)
		span.RecordError(err)
		return report, errs.Wrap(err, "dreaming light")
	}
	report.LightIngested = lightRes.ingested
	report.LightDeduped = lightRes.deduped
	report.LightDropped = lightRes.dropped
	if opts.Backfill {
		// 补偿模式下 ingested 即「TTL 内、未处理过的 L0 条数」——
		// 这是判断积压还剩多少的直接读数，常规运行下该口径无意义。
		report.BackfillScanned = lightRes.ingested
	}

	if lightRes.deduped == 0 {
		// 检查是否有已分期的候选（来自之前的 Run）
		// 即使本轮 Light 没有新候选，可能有待 REM 聚类和 Deep 评分的旧候选
		d.mu.Lock()
		stagedCount := len(d.candidates)
		d.mu.Unlock()
		if stagedCount == 0 {
			// 没有新候选仍可能有既有 L1，继续抽用户/Bot 画像。
			d.extractProfiles(ctx, activeScopes, report)
			report.FinishedAt = time.Now()
			report.Phase = PhaseDeep
			d.logger.Info("dreaming: no candidates, skipping REM/Deep")
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
	report.Promotions = deepRes.promotions
	report.FinishedAt = time.Now()
	report.Phase = PhaseDeep

	d.extractProfiles(ctx, activeScopes, report)

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

func (d *DreamManager) extractProfiles(ctx context.Context, activeScopes []Scope, report *DreamReport) {
	if d.botProfiler != nil {
		n := d.extractBotProfiles(ctx, activeScopes)
		if report != nil {
			report.BotProfiles = n
		}
	}
	if d.manager != nil && d.manager.profiler != nil {
		n := d.extractUserProfiles(ctx, activeScopes)
		if report != nil {
			report.UserProfiles = n
		}
	}
}

// extractUserProfiles 对活跃 user:* scope 蒸馏 L3 用户画像。
func (d *DreamManager) extractUserProfiles(ctx context.Context, activeScopes []Scope) int {
	ctx, span := d.tracer.Start(ctx, "memory.dreaming.user_profile")
	defer span.End()
	logger := traceid.WithLoggerFrom(ctx, d.logger)

	type rankedUser struct {
		scope Scope
		at    time.Time
	}
	ranked := make([]rankedUser, 0, len(activeScopes))
	for _, scope := range activeScopes {
		if scope.Kind == ScopeUser && scope.ID != "" {
			ranked = append(ranked, rankedUser{scope: scope, at: d.manager.store.LatestActivity(ctx, scope)})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].at.Equal(ranked[j].at) {
			return ranked[i].scope.ID < ranked[j].scope.ID
		}
		return ranked[i].at.After(ranked[j].at)
	})
	users := make([]Scope, len(ranked))
	for i := range ranked {
		users[i] = ranked[i].scope
	}
	if len(users) > MaxUserProfilesPerDream {
		logger.Infow("dreaming: user profile cap reached",
			"cap", MaxUserProfilesPerDream, "candidates", len(users))
		users = users[:MaxUserProfilesPerDream]
	}

	var attempted, writtenUsers int
	for _, scope := range users {
		attempted++
		written, err := d.manager.ExtractProfile(ctx, scope)
		if err != nil {
			logger.Warnw("dreaming: user profile extraction failed", "user", scope.ID, "err", err)
			continue
		}
		if written > 0 {
			writtenUsers++
			logger.Infow("dreaming: user profile written", "user", scope.ID, "items", written)
		}
	}
	span.SetAttributes(
		attribute.Int("user_profiles_attempted", attempted),
		attribute.Int("user_profiles_extracted", writtenUsers),
	)
	return writtenUsers
}

// extractBotProfiles 对活跃 scope 中的 BotScope 执行自我画像提取。
func (d *DreamManager) extractBotProfiles(ctx context.Context, activeScopes []Scope) int {
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
		if profile.Confidence < MinProfileWriteConfidence {
			logger.Infow("dreaming: skip low-confidence bot profile (keep SOUL seed)",
				"bot_id", botID, "confidence", profile.Confidence)
			continue
		}

		// 将画像写入 L3（BotScope）
		d.persistBotProfile(ctx, scope, profile)

		extractedCount++

		// 回调通知（加锁拷贝函数指针，避免与 SetOnBotProfileUpdated 并发写竞争）
		d.mu.Lock()
		cb := d.onBotProfileUpdated
		d.mu.Unlock()
		if cb != nil {
			cb(botID, profile)
		}
	}
	return extractedCount
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
	if report.UserProfiles > 0 || report.BotProfiles > 0 {
		entry += fmt.Sprintf("- Profiles: user=%d, bot=%d\n", report.UserProfiles, report.BotProfiles)
	}
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
