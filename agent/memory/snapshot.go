package memory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"go.uber.org/zap"
)

// ============================================================================
// Snapshot — 记忆快照管理器（可配置刷新策略）
//
// 三种刷新模式：
//
//	ModeLive（默认）：每次构建系统提示时重新检索最新记忆。
//	  bot 始终看到最新状态（包括本轮工具写入的记忆）。
//	ModeFrozen：会话启动时冻结，整个会话不变。保护 prefix cache。
//	ModePeriodic：每 N 轮或 T 时间刷新一次，平衡 freshness 和开销。
//
// 数据流：
//
//	会话启动 → Init(retriever, scopes) → 初始快照
//	    ↓
//	每轮开始 → ShouldRefresh() ? → Refresh() → 重新检索 → 更新快照
//	    ↓
//	系统提示注入 = 当前快照
//	    ↓
//	运行期 Write() → 持久化存储 → 下轮 Refresh 时生效
// ============================================================================

// RefreshMode 控制快照何时刷新。
type RefreshMode int

// candidatePoolFactor 交给 renderBlock 的候选池相对 MaxEntries 的倍数。
//
// 见 Snapshot.doRefresh 里候选池粗筛的实测说明：候选池必须显著大于最终注入
// 条数，否则「丢弃超长 + 折叠近重复」腾出的预算没有替补可填。
const candidatePoolFactor = 10

// recentPerScope 主通道每个 scope 取最近多少条作为候选。
//
// 这是「老记忆能不能浮现」的第一道闸门：某个 scope 的记忆量一旦远超这个值，
// 窗口外的条目就完全进不了候选集（详见 relevance.go 的实测说明）。
const recentPerScope = 50

const (
	// ModeLive 实时刷新（默认）：每次构建系统提示时重新检索。
	// bot 始终看到最新记忆状态，包括本轮通过工具写入的内容。
	ModeLive RefreshMode = iota
	// ModeFrozen 冻结模式：会话启动时冻结，整个会话不变。
	// 保护 prefix cache，但运行期写入在下次会话才可见。
	ModeFrozen
	// ModePeriodic 定期刷新：每 RefreshInterval 或 RefreshTurns 轮刷新一次。
	ModePeriodic
)

// SnapshotConfig 配置记忆快照。
type SnapshotConfig struct {
	// Mode 刷新模式（默认 ModeLive）。
	Mode RefreshMode
	// MaxMemoryChars memory（agent 笔记）的字符上限（默认 2200）。
	// 若注入了 Window，则实际上限由 Window.MemoryBudget()*3 动态派生，
	// 与 context.go 的 maxChars := maxTokens*3 口径一致；此字段仅作 fallback。
	MaxMemoryChars int
	// MaxUserChars user（用户画像）的字符上限（默认 1375）。
	MaxUserChars int
	// Window 动态上下文窗口管理器（可选）。注入后记忆块字符上限由
	// Window.MemoryBudget()*3 派生，取代硬编码的 MaxMemoryChars/MaxUserChars，
	// 使记忆预算随模型上下文窗口自适应（与其他使用 window 模块的地方一致）。
	Window *Window
	// MaxEntries 注入上下文的记忆条目数硬上限（默认 20）。
	// 人的注意力约 10 条上下文，给 20 条是富裕上限；无论候选池多大，
	// 实际注入的记忆条目数不超过此值，超出部分按重要性降序截断。
	MaxEntries int
	// CompressTriggerRatio 记忆块字符数达到窗口 memory 预算的该比例时启动压缩（默认 0.2）。
	// 即记忆上下文最多占用 20% 的窗口 memory 预算，超出即截断长条目压缩腾位。
	// 与 MaxEntries 构成「双条件」：条数封顶 + 体积封顶，任一触顶即压缩。
	CompressTriggerRatio float64
	// Header 记忆块的头部模板。
	// 占位符：{usage} → "45% — 990/2200 chars"。
	Header string
	// Separator 条目之间的分隔符。
	Separator string
	// RefreshInterval 定期刷新间隔（仅 ModePeriodic 生效，默认 5min）。
	RefreshInterval time.Duration
	// RefreshTurns 定期刷新轮次间隔（仅 ModePeriodic 生效，默认 10）。
	RefreshTurns int
	// RelevanceRecall 是否启用相关性召回（默认 false，保持原有行为）。
	//
	// 背景：主通道是「每 scope 最近 N 条」，当某 scope 记忆量远大于 N 时，
	// 窗口外的历史记忆（含 importance 最高的一批）永远进不了候选集。
	// 开启后，除主通道外还会从更宽的候选窗口里按与 Query 的相关性补足若干条。
	// 默认关闭：这是行为变更，需先在小范围验证收益再放量。
	RelevanceRecall bool
	// RelevanceCandidates 相关性召回的候选窗口大小（每个 scope，默认 1000）。
	RelevanceCandidates int
	// RelevanceTopK 相关性通道最多补足的条数（跨 scope 合计，默认 5）。
	// 设为总配额而非每 scope 配额，是为了避免相关性条目挤占主通道名额。
	RelevanceTopK int
	// ImportantTopK 高价值保底通道最多补足的条数（跨 scope 合计，默认 5）。
	//
	// 时间窗口再大也覆盖不到的跨月记忆（本机 misskey scope 里 importance 最高的
	// 一批排在第 2400 位之后，09-19 的 50 条窗口完全看不到它们），
	// 只能靠 importance 阈值直接捞。实测这类条目只有个位数，常驻成本可忽略。
	ImportantTopK int
	// ImportantMinImportance 高价值保底的 importance 阈值（默认 0.7）。
	ImportantMinImportance float64
	// ImportantRelevanceWeight 保底通道里相关性相对 importance 的权重（默认 0.6）。
	//
	// 保底通道的排序键是 relevance*weight + importance。设 0 即回到
	// 「纯按 importance 取」的旧行为；加大则更偏向「与本轮话题相关」。
	// 详见 relevance.go 里 DefaultImportantRelevanceWeight 的实测说明。
	ImportantRelevanceWeight float64
	// BeyondWindowRetriever 窗口外补充通道专用的检索源（可选，默认用主检索器）。
	//
	// 存在理由（2026-09-24 实测）：本机 importance 最高的一批记忆是 tier=0，
	// 而主检索链路只含 L1/L3/memory_entries，这批条目不在候选集里，排序再怎么
	// 改也捞不回来。L0 是未升华的原始事件流（2000+ 条），不能并入主通道
	// （会把碎碎念灌进 prompt），因此单独装配给补充通道使用：该通道默认关闭，
	// 且只取 importance 达标 + 与当前话题相关的少数几条。
	BeyondWindowRetriever Retriever
	// RecalledMaxChars 补充条目的单条字符上限（默认 240）。
	// 补充条目普遍偏长，不封顶会吃满整个记忆块预算，挤掉主通道的近期记忆。
	RecalledMaxChars int
	// MaxRenderedEntryChars 注入块里单条记忆的字符上限（默认 240，常开）。
	//
	// 作用于**主通道**（补充通道另有 RecalledMaxChars），属于行为变更：开启后
	// 超过上限 renderedSkipFactor 倍的条目会被丢弃（本机是 14808/15314 字符的
	// 完整档案，它们此前单条就吃满预算，导致注入块只剩 1 条残片）。
	// 2026-09-24 由灰度开关转常开：本机实测注入从 1 条档案残片变成 10 条有效记忆
	// （叠加下面的近重复折叠后是 18 条）。
	// 传负数可显式关闭（仅测试/对照用）。
	// 详见 relevance.go 里 DefaultRenderedMaxChars 的实测说明。
	MaxRenderedEntryChars int
	// NearDuplicateThreshold 近重复折叠的相似度阈值（默认 0.7，常开）。
	//
	// 同一事件常被记成措辞略不同的多条，importance 相近时会同时挤进注入块，
	// 把其它记忆挤出去（实测「变压器事件」×4、「@umeboshicc 档案」×3）。
	// 折叠后每组只留内容更全的一条（importance 取组内最高），腾出的名额留给别的记忆。
	// 传负数可显式关闭（仅测试/对照用）。
	// 详见 relevance.go 里 DefaultNearDuplicateThreshold 的实测说明。
	NearDuplicateThreshold float64
	// RecalledBudgetChars 补充通道的字符总配额（默认 1200）。
	//
	// 独立于单条封顶 RecalledMaxChars：单条封顶约束的是「一条占多少」，
	// 总配额约束的是「补充通道一共能占多少」。实测补充通道 8 条 × 240 字符
	// = 1920 会吃掉 87% 预算，主通道从 18 条掉到 4 条。相关性召回应当是
	// 「在近期记忆之外多想起几条」，不是「替换掉近期记忆」。
	// 传负数可显式关闭（仅测试/对照用）。
	// 详见 relevance.go 里 DefaultRecalledBudgetChars 的实测说明。
	RecalledBudgetChars int
	// PinnedCategories 约束类记忆的 category 名单（默认 preference / bot_personality）。
	//
	// 这些是行为准则（用户偏好、行为约定、人设），不因「与当前话题不相关」
	// 而被挤出注入块。详见 hoistPinned 的实测说明。
	// 传空 slice 可显式关闭（仅测试/对照用）。
	PinnedCategories []string
	// MaxPinnedEntries 约束类记忆的置顶名额上限（默认 6）。
	// 本机约束类条目有 89 条，必须限量，否则会反过来压死主通道。
	MaxPinnedEntries int
	// Query 当前轮次的输入文本，用于相关性打分。由调用方（如 RecallStage）
	// 在每轮构建快照时注入；为空时相关性通道自动跳过。
	Query string
}

// DefaultSnapshotConfig 返回默认快照配置。
func DefaultSnapshotConfig() SnapshotConfig {
	return SnapshotConfig{
		Mode:                     ModeLive,
		MaxMemoryChars:           2200,
		MaxUserChars:             1375,
		MaxEntries:               20,
		CompressTriggerRatio:     0.2,
		Separator:                "\n§\n",
		RefreshInterval:          5 * time.Minute,
		RefreshTurns:             10,
		RelevanceRecall:          false,
		RelevanceCandidates:      DefaultRelevanceCandidates,
		RelevanceTopK:            DefaultRelevanceTopK,
		ImportantTopK:            DefaultImportantTopK,
		ImportantMinImportance:   DefaultImportantMinImportance,
		ImportantRelevanceWeight: DefaultImportantRelevanceWeight,
		RecalledMaxChars:         DefaultRecalledMaxChars,
		// 单条长度封顶与近重复折叠均已从灰度转常开（2026-09-24 验证收益后）。
		// 两者都只减少「重复/超长内容对记忆块预算的独占」，不新增召回来源。
		MaxRenderedEntryChars:  DefaultRenderedMaxChars,
		NearDuplicateThreshold: DefaultNearDuplicateThreshold,
		RecalledBudgetChars:    DefaultRecalledBudgetChars,
		PinnedCategories:       []string{CategoryPreference, CategoryBotPersonality},
		MaxPinnedEntries:       DefaultMaxPinnedEntries,
	}
}

// pinnedSet 把配置的约束类 category 名单转成小写集合，供 hoistPinned 查表。
// 空名单返回 nil（hoistPinned 见 nil 直接跳过）。
func (s *Snapshot) pinnedSet() map[string]struct{} {
	if len(s.config.PinnedCategories) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(s.config.PinnedCategories))
	for _, c := range s.config.PinnedCategories {
		c = strings.ToLower(strings.TrimSpace(c))
		if c != "" {
			set[c] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// Snapshot 管理记忆快照，支持可配置的刷新策略。
type Snapshot struct {
	config SnapshotConfig

	mu sync.RWMutex

	// 初始化时保存的检索器和作用域（用于实时/定期刷新）
	retriever Retriever
	scopes    []Scope

	// 当前快照内容
	cachedMemory string
	cachedUser   string

	// renderedMemory 上一次 memory 块实际注入的条目数（可观测性用）。
	//
	// 存在理由：字符数不是好指标——实测过 2200 字符预算被一条 1.5 万字档案
	// 的残片吃满、showing 1 的情况，只看 chars 会误判成「记忆很充实」。
	renderedMemory int

	// stats 记忆规模与时间跨度元信息（statsValid=false 表示后端不支持或统计失败）。
	//
	// 存在理由：注入上下文的只有重要性最高的 20 条（且偏新），模型据此回答
	// 「你最早的记忆是什么时候」必然答成最近那批的时间。把「总数/最早/最新」
	// 写进记忆块头部，模型不调工具也能给出正确的时间跨度。
	stats      MemoryStatsInfo
	statsValid bool

	// 状态跟踪
	captured    bool
	capturedAt  time.Time
	lastRefresh time.Time
	turnCount   int

	// 脏标记：工具写入后设为 true，表示下次构建时应刷新
	dirty bool

	// logger 可选日志记录器（用于记录刷新失败等非致命错误）
	logger *zap.SugaredLogger
}

// NewSnapshot 创建快照管理器。
func NewSnapshot(config ...SnapshotConfig) *Snapshot {
	cfg := DefaultSnapshotConfig()
	if len(config) > 0 {
		cfg.Mode = config[0].Mode
		if config[0].MaxMemoryChars > 0 {
			cfg.MaxMemoryChars = config[0].MaxMemoryChars
		}
		if config[0].MaxUserChars > 0 {
			cfg.MaxUserChars = config[0].MaxUserChars
		}
		if config[0].Separator != "" {
			cfg.Separator = config[0].Separator
		}
		if config[0].RefreshInterval > 0 {
			cfg.RefreshInterval = config[0].RefreshInterval
		}
		if config[0].RefreshTurns > 0 {
			cfg.RefreshTurns = config[0].RefreshTurns
		}
		if config[0].Window != nil {
			cfg.Window = config[0].Window
		}
		if config[0].MaxEntries > 0 {
			cfg.MaxEntries = config[0].MaxEntries
		}
		if config[0].CompressTriggerRatio > 0 {
			cfg.CompressTriggerRatio = config[0].CompressTriggerRatio
		}
		// 相关性召回：开关是 bool，直接用传入值（false 也是有效值，
		// 不能用 > 0 判断，否则调用方无法显式关闭）。
		cfg.RelevanceRecall = config[0].RelevanceRecall
		if config[0].RelevanceCandidates > 0 {
			cfg.RelevanceCandidates = config[0].RelevanceCandidates
		}
		if config[0].RelevanceTopK > 0 {
			cfg.RelevanceTopK = config[0].RelevanceTopK
		}
		if config[0].ImportantTopK > 0 {
			cfg.ImportantTopK = config[0].ImportantTopK
		}
		if config[0].ImportantMinImportance > 0 {
			cfg.ImportantMinImportance = config[0].ImportantMinImportance
		}
		// 与其他配额字段同一约定：0 表示「未设置」，回落默认值。
		// 需要临时关闭保底通道的相关性排序时，把 Query 置空即可
		// （无 query 时相关性恒为 0，等价于纯 importance 排序）。
		if config[0].ImportantRelevanceWeight > 0 {
			cfg.ImportantRelevanceWeight = config[0].ImportantRelevanceWeight
		}
		if config[0].BeyondWindowRetriever != nil {
			cfg.BeyondWindowRetriever = config[0].BeyondWindowRetriever
		}
		if config[0].RecalledMaxChars > 0 {
			cfg.RecalledMaxChars = config[0].RecalledMaxChars
		}
		// 常开项：0 = 未设置 → 保持默认；负数 = 显式关闭（测试/对照用）。
		if config[0].MaxRenderedEntryChars != 0 {
			cfg.MaxRenderedEntryChars = config[0].MaxRenderedEntryChars
		}
		if config[0].NearDuplicateThreshold != 0 {
			cfg.NearDuplicateThreshold = config[0].NearDuplicateThreshold
		}
		if config[0].RecalledBudgetChars != 0 {
			cfg.RecalledBudgetChars = config[0].RecalledBudgetChars
		}
		// 空 slice = 显式关闭置顶；nil = 未设置，保持默认。
		if config[0].PinnedCategories != nil {
			cfg.PinnedCategories = config[0].PinnedCategories
		}
		if config[0].MaxPinnedEntries != 0 {
			cfg.MaxPinnedEntries = config[0].MaxPinnedEntries
		}
		if config[0].Query != "" {
			cfg.Query = config[0].Query
		}
	}
	return &Snapshot{config: cfg}
}

// SetLogger 设置日志记录器（可选，用于记录非致命错误）。
func (s *Snapshot) SetLogger(logger *zap.SugaredLogger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logger = logger
}

// Init 初始化快照，绑定检索器和作用域，并执行首次检索。
// 对于 ModeFrozen，等同于 Capture()；对于 ModeLive/ModePeriodic，保存
// retriever 和 scopes 供后续 Refresh() 使用。
func (s *Snapshot) Init(ctx context.Context, retriever Retriever, scopes []Scope) error {
	s.mu.Lock()
	s.retriever = retriever
	s.scopes = scopes
	s.mu.Unlock()
	return s.doRefresh(ctx)
}

// Capture 从 Retriever 检索当前记忆并冻结为快照。
// 兼容旧 API，等同于 Init()。
func (s *Snapshot) Capture(ctx context.Context, retriever Retriever, scopes []Scope) error {
	return s.Init(ctx, retriever, scopes)
}

// MarkDirty 标记快照为脏，表示有新写入，下次构建时应刷新。
// 工具写入后应调用此方法。
func (s *Snapshot) MarkDirty() {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
}

// MarkTurnComplete 增加轮次计数器。
// 应在每轮对话结束后调用。
func (s *Snapshot) MarkTurnComplete() {
	s.mu.Lock()
	s.turnCount++
	s.mu.Unlock()
}

// ShouldRefresh 根据当前模式和状态判断是否需要刷新。
func (s *Snapshot) ShouldRefresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.captured {
		return true
	}

	switch s.config.Mode {
	case ModeLive:
		// 实时模式：只要有脏标记就刷新
		return s.dirty
	case ModeFrozen:
		return false
	case ModePeriodic:
		if s.dirty {
			// 脏 + 超过轮次/时间阈值 → 刷新
			if s.turnCount%s.config.RefreshTurns == 0 {
				return true
			}
			if time.Since(s.lastRefresh) >= s.config.RefreshInterval {
				return true
			}
		}
		return false
	default:
		return s.dirty
	}
}

// Refresh 如果需要则重新检索记忆并更新快照。
// 返回是否实际执行了刷新。
func (s *Snapshot) Refresh(ctx context.Context) (bool, error) {
	if !s.ShouldRefresh() {
		return false, nil
	}
	return true, s.doRefresh(ctx)
}

// doRefresh 执行实际的记忆检索和渲染。
func (s *Snapshot) doRefresh(ctx context.Context) error {
	s.mu.Lock()
	retriever := s.retriever
	scopes := s.scopes
	s.mu.Unlock()

	if retriever == nil {
		return nil
	}

	var memoryEntries, userEntries []Entry

	var lastErr error
	var allEntries []Entry
	for _, scope := range scopes {
		entries, err := retriever.Recent(ctx, scope, recentPerScope)
		if err != nil {
			lastErr = err
			continue
		}
		allEntries = append(allEntries, entries...)
	}

	// 窗口外补充通道：相关性召回 + 高价值保底。默认关闭（RelevanceRecall=false），
	// 关闭时不产生任何额外检索，行为与改动前完全一致。
	if extra := s.recallBeyondWindow(ctx, retriever, scopes, allEntries); len(extra) > 0 {
		allEntries = append(allEntries, extra...)
	}

	// 候选池粗筛：按重要性降序保留前 MaxEntries*candidatePoolFactor 条，
	// 真正的「注入条数硬上限」由 renderBlock 的 MaxEntries 执行。
	//
	// 这里**不能**直接截到 MaxEntries（2026-09-24 实测修正）：renderBlock 会先
	// 丢弃超长条目、再折叠近重复，候选池若只有 MaxEntries 条，那一批被丢掉的
	// 名额就没有替补——本机 20 条候选里 14 条是 >960 字的档案，处理完只剩 6 条，
	// 折叠后再减到 3 条，记忆块只用了 732/2200 字符，去重反而让注入更空。
	// 放大候选池后，被丢弃/折叠腾出的预算才能被后面的条目填上。
	poolLimit := s.config.MaxEntries * candidatePoolFactor
	if poolLimit < recentPerScope {
		poolLimit = recentPerScope
	}
	if s.config.MaxEntries > 0 && len(allEntries) > poolLimit {
		sort.SliceStable(allEntries, func(i, j int) bool {
			return allEntries[i].Importance > allEntries[j].Importance
		})
		allEntries = allEntries[:poolLimit]
	}
	for _, e := range allEntries {
		if e.Scope.Kind == ScopeUser {
			userEntries = append(userEntries, e)
		} else {
			memoryEntries = append(memoryEntries, e)
		}
	}

	// 如果所有 scope 都失败了（无条目且有错误），返回错误
	if len(memoryEntries) == 0 && len(userEntries) == 0 && lastErr != nil {
		return fmt.Errorf("snapshot: all scope retrievals failed: %w", lastErr)
	}

	// 规模 / 时间跨度元信息（可选能力）：单次聚合查询，后端不支持则跳过。
	stats, statsValid := memoryStats(ctx, retriever, scopes)

	memoryBlock, memoryShown := s.renderBlock("memory", memoryEntries, stats, statsValid)
	userBlock, _ := s.renderBlock("user", userEntries, stats, statsValid)

	s.mu.Lock()
	s.stats = stats
	s.statsValid = statsValid
	s.cachedMemory = memoryBlock
	s.cachedUser = userBlock
	// 实际注入条数（memory 块）：「记忆块有没有被单条超长档案吃满」的直接指标。
	s.renderedMemory = memoryShown
	s.captured = true
	s.capturedAt = time.Now()
	s.lastRefresh = time.Now()
	s.dirty = false
	s.mu.Unlock()

	return nil
}

// recallBeyondWindow 在主通道（每 scope 最近 recentPerScope 条）之外，
// 补充两类「本该被想起、但被时间窗口挡住」的条目：
//
//  1. 相关性：与当前输入话题相关、却落在时间窗口外的历史记忆；
//  2. 高价值：importance 达阈值的长期事实/偏好，无论多老。
//
// 存在理由（实测 2026-09-24，本机 misskey timeline scope）：
// 该 scope 共 2472 条，最近 50 条全部落在同一天；300 条覆盖 4 天、1000 条覆盖
// 9 天。全表 importance 最高的 7 条（0.800，用户对 bot 行为的偏好、长期人设）
// 全部在 08-13/08-14，排在第 2400 位之后——时间窗口再大也捞不到，
// 这正是必须同时保留「高价值保底」通道的原因。
//
// 两类条目都会经 boostRelevance 抬升有效 importance 到主通道入选门槛之上，
// 否则会在后续「按 importance 降序截断」与「字符预算逐条截断」里被二次挤掉。
func (s *Snapshot) recallBeyondWindow(ctx context.Context, retriever Retriever, scopes []Scope, base []Entry) []Entry {
	if !s.config.RelevanceRecall || retriever == nil {
		return nil
	}
	started := time.Now()

	// 主通道已入选的条目：ID 为空者无法去重，视为不同条目（宁可重复也别丢）。
	seen := make(map[string]struct{}, len(base))
	for _, e := range base {
		if e.ID != "" {
			seen[e.ID] = struct{}{}
		}
	}

	// 抬升基准取主通道**最高** importance：补充条目要严格压过主通道所有条目，
	// 否则会被 doRefresh 的 MaxEntries 截断或 renderBlock 的字符预算挤掉。
	// relevanceGate 只保证越过第 MaxEntries 名，实测不够（见 boostRelevance 注释）。
	gate := topImportance(base)
	var recalled []Entry

	// 通道一：相关性（需要当前输入作为 query）
	var relevantCount, candidateCount int
	if strings.TrimSpace(s.config.Query) != "" {
		var candidates []Entry
		for _, scope := range scopes {
			wider, err := retriever.Recent(ctx, scope, s.config.RelevanceCandidates)
			if err != nil {
				continue
			}
			candidates = append(candidates, wider...)
		}
		// 先按长度过滤再打分：超过补充通道封顶门槛的条目最终会被 capRecalled
		// 丢弃，为它们做分词纯属浪费。本机有 14808/15314 字符的档案条目，
		// 不过滤时每轮快照要多花约 380ms（基线 16ms → 开启后 400ms）。
		candidates = filterOversized(candidates, s.config.RecalledMaxChars)
		candidateCount = len(candidates)

		picked := SelectRelevant(s.config.Query, candidates, seen, s.config.RelevanceTopK)
		if len(picked) > 0 {
			boostRelevance(picked, gate)
			var rel []Entry
			for _, p := range picked {
				rel = append(rel, p.Entry)
				if p.Entry.ID != "" {
					seen[p.Entry.ID] = struct{}{}
				}
			}
			rel = capRecalled(rel, s.config.RecalledMaxChars)
			relevantCount = len(rel)
			recalled = append(recalled, rel...)
		}
	}

	// 通道二：高价值保底（跨全时间取 importance 达标者，按「相关性+重要性」排序）
	//
	// 用补充通道专用检索源（若装配了）：主检索链路不含 L0，而高价值老记忆
	// 恰恰常是 L0，走主检索器会永远看不到它们（实测详情见配置字段注释）。
	var importantCount int
	importantRetriever := retriever
	if s.config.BeyondWindowRetriever != nil {
		importantRetriever = s.config.BeyondWindowRetriever
	}
	if picked := SelectImportant(ctx, importantRetriever, scopes, seen, s.config.Query,
		s.config.ImportantMinImportance, s.config.ImportantRelevanceWeight,
		s.config.ImportantTopK, s.config.RecalledMaxChars, s.logger); len(picked) > 0 {
		boostRelevance(picked, gate)
		var imp []Entry
		for _, p := range picked {
			imp = append(imp, p.Entry)
			if p.Entry.ID != "" {
				seen[p.Entry.ID] = struct{}{}
			}
		}
		imp = capRecalled(imp, s.config.RecalledMaxChars)
		importantCount = len(imp)
		recalled = append(recalled, imp...)
	}

	// 总配额裁剪：补充通道不能吃满整个记忆块预算，否则「多想起几条老记忆」
	// 会变成「替换掉近期记忆」（实测主通道 18 条 → 4 条）。
	var budgetDropped int
	recalled, budgetDropped = capRecalledBudget(recalled, s.config.RecalledBudgetChars)

	if s.logger != nil {
		// INFO 级：运维需能直接观测两条补充通道是否工作、各补进了几条。
		s.logger.Infow("snapshot: recall beyond window",
			"scopes", len(scopes),
			"base", len(base),
			"candidates", candidateCount,
			"relevant", relevantCount,
			"important", importantCount,
			"recalled", len(recalled),
			"budget_dropped", budgetDropped,
			"budget_chars", s.config.RecalledBudgetChars,
			"elapsed_ms", time.Since(started).Milliseconds())
	}

	return recalled
}

// IsCaptured 返回快照是否已初始化。
func (s *Snapshot) IsCaptured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.captured
}

// MemorySnapshot 返回当前的 memory 快照文本。
func (s *Snapshot) MemorySnapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cachedMemory
}

// RenderedMemoryCount 返回上次 memory 块实际注入的条目数。
// 供调用方（如 RecallStage）打日志观测「记忆块有没有被少数几条占满」。
func (s *Snapshot) RenderedMemoryCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.renderedMemory
}

// UserSnapshot 返回当前的 user 快照文本。
func (s *Snapshot) UserSnapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cachedUser
}

// FullSnapshot 返回完整的快照（memory + user 拼接）。
func (s *Snapshot) FullSnapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result string
	if s.cachedMemory != "" {
		result = s.cachedMemory
	}
	if s.cachedUser != "" {
		if result != "" {
			result += "\n\n"
		}
		result += s.cachedUser
	}
	return result
}

// renderBlock 渲染一个记忆块，返回文本与实际注入的条目数。
// 对每条条目执行威胁扫描，匹配的条目被替换为 [BLOCKED: ...] 占位符。
//
// 返回条数是为了可观测性：注入条数（而不是字符数）才是「记忆块有没有被单条
// 档案吃满」的直接指标——实测过字符数顶到 2200 但只 showing 1 的情况，
// 光看 chars 会误判成「记忆很充实」。
func (s *Snapshot) renderBlock(target string, entries []Entry, stats MemoryStatsInfo, statsValid bool) (string, int) {
	if len(entries) == 0 {
		return "", 0
	}

	// 按重要性降序稳定排序：固定的字符预算优先留给高价值记忆，而非最近的 N 条。
	// 同等重要性保持原有顺序（约=时间倒序），不破坏召回的时效性直觉。
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Importance > entries[j].Importance
	})

	// 单条长度约束（常开）：巨型条目会把整个字符预算吃光，
	// 必须先于条数截断执行，否则它们仍会占掉名额。
	//
	// 分两步且中间夹着去重，是因为去重要在**截断前**才有意义（截断后所有长
	// 条目都是等长残片，既算不准相似度也分不出哪条信息更全）；而先剔除注定
	// 被丢弃的巨型条目，能避免为它们做分词——本机有 14808/15314 字符的档案，
	// 不过滤时每轮快照要多花几百毫秒。
	var oversizedDropped int
	if s.config.MaxRenderedEntryChars > 0 {
		before := len(entries)
		entries = filterOversizedBy(entries, s.config.MaxRenderedEntryChars, renderedSkipFactor)
		oversizedDropped = before - len(entries)
		if oversizedDropped > 0 && s.logger != nil {
			s.logger.Infow("snapshot: dropped oversized entries from memory block",
				"dropped", oversizedDropped, "maxChars", s.config.MaxRenderedEntryChars)
		}
	}

	// 近重复折叠（常开）：同一事件常被记成措辞略不同的多条，importance 相近时
	// 会同时挤进注入块。必须在条数截断**之前**做，腾出的名额才能被后面的条目填上。
	// 约束类置顶：pinned 条目是行为准则，不能因为与当前话题不相关而被相关性
	// 排序挤出预算。必须在条数截断之前做，否则置顶了也进不了块。
	var pinnedCount int
	if len(s.config.PinnedCategories) > 0 && s.config.MaxPinnedEntries > 0 {
		entries, pinnedCount = hoistPinned(entries, s.pinnedSet(), s.config.MaxPinnedEntries)
		if pinnedCount > 0 && s.logger != nil {
			s.logger.Debugw("snapshot: hoisted constraint memories",
				"pinned", pinnedCount, "of", len(entries))
		}
	}

	if s.config.NearDuplicateThreshold > 0 {
		kept, folded := dedupeNearDuplicates(entries, s.config.NearDuplicateThreshold)
		if folded > 0 && s.logger != nil {
			s.logger.Infow("snapshot: folded near-duplicate entries from memory block",
				"folded", folded, "threshold", s.config.NearDuplicateThreshold,
				"kept", len(kept))
		}
		entries = kept
	}

	if s.config.MaxRenderedEntryChars > 0 {
		entries, _ = capRendered(entries, s.config.MaxRenderedEntryChars)
	}

	// 条数硬上限（双条件之一）：无论如何最多注入 MaxEntries 条。
	// 人的注意力约 10 条上下文，给 20 条是富裕上限；超出部分按重要性降序截断。
	if s.config.MaxEntries > 0 && len(entries) > s.config.MaxEntries {
		entries = entries[:s.config.MaxEntries]
	}

	limit := s.config.MaxMemoryChars
	if target == "user" {
		limit = s.config.MaxUserChars
	}

	// 若注入了 Window（生产路径），记忆块字符上限改为由 window 模块派生：
	// MemoryBudget() 返回 token 预算，×3 估算字符（与 context.go 的
	// maxChars := maxTokens*3 口径一致），从而避免硬编码魔法数、随模型
	// 上下文窗口自适应。未注入 Window 时回退到上面的硬编码默认值。
	//
	// 双条件之二（压缩触发）：记忆块预算取窗口 memory 预算的 CompressTriggerRatio
	// 比例（默认 0.2），即记忆上下文最多占用 20% 的窗口 memory 预算；
	// 超出即由下方逐条截断逻辑压缩长条目腾位。条数封顶 + 体积封顶共同约束注入体积。
	if s.config.Window != nil {
		if budget := s.config.Window.MemoryBudget(); budget > 0 {
			budgetChars := int(float64(budget*3) * s.config.CompressTriggerRatio)
			if target == "user" {
				// 保持原 2200/1375 的比例（≈0.625），让 user 块占 memory 块的一部分
				limit = budgetChars * 1375 / 2200
			} else {
				limit = budgetChars
			}
		}
	}

	var sanitized []string
	totalChars := 0

	for _, e := range entries {
		content := e.Content

		// 威胁扫描
		findings := ScanMemoryThreats(content)
		if len(findings) > 0 {
			content = "[BLOCKED: memory entry contained threat pattern(s): " +
				ThreatSummary(findings) + ". Removed from system prompt.]"
		}

		// 字符预算检查
		entryLen := len([]rune(content))
		if totalChars+entryLen > limit {
			remaining := limit - totalChars
			if remaining <= 0 {
				break
			}
			runes := []rune(content)
			if remaining < len(runes) {
				content = string(runes[:remaining]) + "..."
				entryLen = len([]rune(content)) // 更新为截断后的实际长度
			}
		}

		sanitized = append(sanitized, content)
		totalChars += entryLen + len([]rune(s.config.Separator))
	}

	if len(sanitized) == 0 {
		return "", 0
	}

	var header string
	if target == "user" {
		header = "USER PROFILE (who the user is)"
	} else {
		header = "MEMORY (your personal notes)"
	}

	usage := formatUsage(totalChars, limit)
	separator := "════════════════════════════════════════════════"

	var sb strings.Builder
	sb.WriteString(separator + "\n" + header + " [" + usage + "]\n")

	// 仅 memory 块带元信息：user 块条数少且不涉及「最早」类问题。
	// 这一段是「你最早的记忆是什么时候」的唯一低成本正解 —— 下面的 N 条是按
	// 重要性截断的，几乎全是近期的，模型若只看条目会把「最近」当成「最早」。
	if target != "user" && statsValid && stats.Total > 0 {
		sb.WriteString(s.renderStatsLine(stats, len(sanitized)))
	}

	sb.WriteString(separator + "\n")
	for i, entry := range sanitized {
		if i > 0 {
			sb.WriteString(s.config.Separator)
		}
		sb.WriteString(entry)
	}

	return sb.String(), len(sanitized)
}

// renderStatsLine 渲染记忆块的规模/时间跨度元信息行。
//
// 关键在最后那句提示：光告诉模型「最早是 2026-08-12」不够，还得告诉它
// 「要看那批内容得用 order=oldest 去查」，否则它知道了日期也编不出内容。
func (s *Snapshot) renderStatsLine(stats MemoryStatsInfo, shown int) string {
	var sb strings.Builder
	sb.WriteString("total " + strconv.Itoa(stats.Total) + " memories")
	if !stats.Oldest.IsZero() {
		sb.WriteString(", oldest " + stats.Oldest.Format("2006-01-02"))
	}
	if !stats.Newest.IsZero() {
		sb.WriteString(", newest " + stats.Newest.Format("2006-01-02"))
	}
	sb.WriteString(" | showing " + strconv.Itoa(shown) +
		" by importance — NOT the full timeline. For \"earliest memory\" / \"what happened in <month>\", " +
		"use the memory tool: search with order=\"oldest\" (optionally since/until), scope_kind=\"all\".\n")
	return sb.String()
}

// memoryStats 从检索器取规模/时间跨度统计；后端不支持时返回 false。
func memoryStats(ctx context.Context, retriever Retriever, scopes []Scope) (MemoryStatsInfo, bool) {
	provider, ok := retriever.(MemoryStatsProvider)
	if !ok {
		return MemoryStatsInfo{}, false
	}
	info, err := provider.MemoryStats(ctx, scopes)
	if err != nil {
		return MemoryStatsInfo{}, false
	}
	if info.Total == 0 {
		return MemoryStatsInfo{}, false
	}
	return info, true
}

// formatUsage 格式化用量字符串。
func formatUsage(current, limit int) string {
	return formatCharCount(current) + "/" + formatCharCount(limit) + " chars"
}

// formatCharCount 格式化字符数（带千分位）。
func formatCharCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	thousands := n / 1000
	remainder := n % 1000
	return strconv.Itoa(thousands) + "," + fmt.Sprintf("%03d", remainder)
}

// ============================================================================
// SnapshotPromptSection — 将快照注入系统提示的 PromptSection
// ============================================================================

// SnapshotPromptSection 返回一个 PromptSection，在系统提示构建时注入快照。
func (s *Snapshot) SnapshotPromptSection() *prompt.Section {
	return &prompt.Section{
		Name:    "memory_snapshot",
		Order:   200, // 200-299: 上下文信息（记忆）
		Enabled: true,
	}
}

// UpdatePromptSection 更新 PromptSection 的 Content 为当前快照。
// 如果处于 ModeLive/ModePeriodic 且有脏标记，会先刷新。
// 应在每轮系统提示组装前调用。
func (s *Snapshot) UpdatePromptSection(ctx context.Context, section *prompt.Section) {
	if s.config.Mode != ModeFrozen {
		if _, err := s.Refresh(ctx); err != nil {
			s.mu.RLock()
			logger := s.logger
			s.mu.RUnlock()
			if logger != nil {
				logger.Warnw("snapshot: refresh failed, using cached snapshot",
					"mode", s.config.Mode, "err", err)
			}
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.captured {
		section.Content = ""
		section.Enabled = false
		return
	}
	section.Content = s.fullSnapshotLocked()
	section.Enabled = section.Content != ""
}

// fullSnapshotLocked 返回完整快照（调用方已持有读锁）。
func (s *Snapshot) fullSnapshotLocked() string {
	var result string
	if s.cachedMemory != "" {
		result = s.cachedMemory
	}
	if s.cachedUser != "" {
		if result != "" {
			result += "\n\n"
		}
		result += s.cachedUser
	}
	return result
}

// ScanMemoryThreats 扫描记忆内容中的威胁模式。
func ScanMemoryThreats(content string) []prompt.ScanFinding {
	return prompt.ScanForThreats(content)
}

// ThreatSummary 返回威胁扫描的摘要字符串。
func ThreatSummary(findings []prompt.ScanFinding) string {
	return prompt.FindingsSummary(findings)
}
