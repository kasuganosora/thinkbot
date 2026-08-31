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
}

// DefaultSnapshotConfig 返回默认快照配置。
func DefaultSnapshotConfig() SnapshotConfig {
	return SnapshotConfig{
		Mode:            ModeLive,
		MaxMemoryChars:  2200,
		MaxUserChars:    1375,
		MaxEntries:      20,
		CompressTriggerRatio: 0.2,
		Separator:       "\n§\n",
		RefreshInterval: 5 * time.Minute,
		RefreshTurns:    10,
	}
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
		entries, err := retriever.Recent(ctx, scope, 50)
		if err != nil {
			lastErr = err
			continue
		}
		allEntries = append(allEntries, entries...)
	}

	// 双条件之一：条数硬上限——合并 memory + user 后按重要性降序取前 MaxEntries 条，
	// 作为整个注入上下文的总条数上限（人的注意力约 10 条，给 20 条是富裕上限）。
	// 合并后再切分，保证最宝贵的 20 条（含用户画像）优先进入上下文。
	if s.config.MaxEntries > 0 && len(allEntries) > s.config.MaxEntries {
		sort.SliceStable(allEntries, func(i, j int) bool {
			return allEntries[i].Importance > allEntries[j].Importance
		})
		allEntries = allEntries[:s.config.MaxEntries]
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

	s.mu.Lock()
	s.cachedMemory = s.renderBlock("memory", memoryEntries)
	s.cachedUser = s.renderBlock("user", userEntries)
	s.captured = true
	s.capturedAt = time.Now()
	s.lastRefresh = time.Now()
	s.dirty = false
	s.mu.Unlock()

	return nil
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

// renderBlock 渲染一个记忆块。
// 对每条条目执行威胁扫描，匹配的条目被替换为 [BLOCKED: ...] 占位符。
func (s *Snapshot) renderBlock(target string, entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}

	// 按重要性降序稳定排序：固定的字符预算优先留给高价值记忆，而非最近的 N 条。
	// 同等重要性保持原有顺序（约=时间倒序），不破坏召回的时效性直觉。
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Importance > entries[j].Importance
	})

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
		return ""
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
	sb.WriteString(separator + "\n" + header + " [" + usage + "]\n" + separator + "\n")
	for i, entry := range sanitized {
		if i > 0 {
			sb.WriteString(s.config.Separator)
		}
		sb.WriteString(entry)
	}

	return sb.String()
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
