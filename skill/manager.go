package skill

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ============================================================================
// SkillManager — Skill 生命周期管理
//
// 线程安全：所有公有方法均可并发调用。
//
// 集成方式：
//   - 通过 RegistryAdapter 注入 prompt Section（避免循环依赖）
//   - 通过 StoreAdapter 持久化启用状态（避免循环依赖）
//   - 实现 tools.ToolProvider 接口（可选，若 Skill 声明了依赖 Tool）
// ============================================================================

// RegistryAdapter 抽象 prompt.Registry 的最小接口，
// 避免 skill 包直接依赖 agent/prompt 包（防止循环依赖）。
// 实际使用时由 agent 层提供一个适配器实现。
type RegistryAdapter interface {
	// RegisterSection 注册一个 prompt Section。
	RegisterSection(name string, order int, content string, enabled bool)
	// UnregisterSection 移除指定名称的 prompt Section。
	UnregisterSection(name string)
}

// StoreAdapter 抽象 config.Store 的最小接口。
type StoreAdapter interface {
	// Get 读取配置值，不存在返回 ("", false)。
	Get(key string) (string, bool)
	// Set 持久化配置值。
	Set(ctx context.Context, key, value string) error
	// GetBool 读取布尔配置值。
	GetBool(key string, def bool) bool
}

// Logger 日志接口。
type Logger interface {
	Debugw(msg string, keysAndValues ...interface{})
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Errorw(msg string, keysAndValues ...interface{})
}

type noopLogger struct{}

func (noopLogger) Debugw(msg string, keysAndValues ...interface{}) {}
func (noopLogger) Infow(msg string, keysAndValues ...interface{})  {}
func (noopLogger) Warnw(msg string, keysAndValues ...interface{})  {}
func (noopLogger) Errorw(msg string, keysAndValues ...interface{}) {}

// SkillManager 管理所有 Skill 的注册、启用/禁用、触发注入。
type SkillManager struct {
	mu     sync.RWMutex
	skills map[string]*Skill

	registry RegistryAdapter // prompt Section 注入适配器（可为 nil）
	store    StoreAdapter    // 配置持久化适配器（可为 nil）
	logger   Logger
}

// NewSkillManager 创建 SkillManager。
// registry 为 nil 时不注入 prompt（仅管理 Skill 元数据）。
// store 为 nil 时不持久化启用状态（仅内存管理）。
func NewSkillManager(registry RegistryAdapter, store StoreAdapter, logger Logger) *SkillManager {
	if logger == nil {
		logger = noopLogger{}
	}
	return &SkillManager{
		skills:   make(map[string]*Skill),
		registry: registry,
		store:    store,
		logger:   logger,
	}
}

// SetRegistry 运行时设置/替换 prompt Registry 适配器。
func (m *SkillManager) SetRegistry(r RegistryAdapter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registry = r
}

// SetStore 运行时设置/替换配置 Store 适配器。
func (m *SkillManager) SetStore(s StoreAdapter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = s
}

// ============================================================================
// Skill 注册
// ============================================================================

// Register 注册一个 Skill。如果 name 已存在则覆盖。
// 注册时若 Skill.Enabled=true，自动将 Content 注入 Registry。
func (m *SkillManager) Register(skill *Skill) {
	m.mu.Lock()
	defer m.mu.Unlock()

	old, exists := m.skills[skill.Name]
	// 若旧 Skill 已注入 prompt，先移除
	if exists && old.Enabled && old.Content != "" {
		m.unregisterPromptLocked(skill.Name)
	}

	skill.Enabled = m.resolveEnabledLocked(skill, exists, old)
	m.skills[skill.Name] = skill

	// 启用状态：注入 prompt
	if skill.Enabled && skill.Content != "" {
		m.registerPromptLocked(skill)
	}

	m.logger.Debugw("skill registered",
		"name", skill.Name,
		"enabled", skill.Enabled,
		"source", skill.Source,
	)
}

// resolveEnabledLocked 根据配置决定 Skill 的启用状态（必须持有 mu.Lock）。
// 优先级：Store > 已有状态 > SKILL.md front matter > 默认启用。
func (m *SkillManager) resolveEnabledLocked(skill *Skill, exists bool, old *Skill) bool {
	// 1. Store 中有记录 → 以数据库为准
	if m.store != nil {
		key := "skill." + skill.Name + ".enabled"
		if val, ok := m.store.Get(key); ok {
			return val == "true"
		}
	}

	// 2. 更新已有 Skill 且 DB 中无记录 → 保持原状态
	if exists && old != nil {
		return old.Enabled
	}

	// 3. 新注册 → 以 SKILL.md front matter 的 enabled 为准，未指定则默认启用
	// Skill.Enabled 在 loader.go 中已从 meta.Enabled 赋值
	return skill.Enabled
}

// ============================================================================
// 启用 / 禁用
// ============================================================================

// Enable 启用指定 Skill，并将其 Content 注入 Registry。
func (m *SkillManager) Enable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	skill, ok := m.skills[name]
	if !ok {
		return &errNotFound{name: name}
	}
	if skill.Enabled {
		return nil // 幂等
	}

	skill.Enabled = true
	if skill.Content != "" {
		m.registerPromptLocked(skill)
	}

	m.persistEnabledLocked(name, true)
	m.logger.Infow("skill enabled", "name", name)
	return nil
}

// Disable 禁用指定 Skill，并从 Registry 移除其 Content。
func (m *SkillManager) Disable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	skill, ok := m.skills[name]
	if !ok {
		return &errNotFound{name: name}
	}
	if !skill.Enabled {
		return nil // 幂等
	}

	skill.Enabled = false
	if skill.Content != "" {
		m.unregisterPromptLocked(name)
	}

	m.persistEnabledLocked(name, false)
	m.logger.Infow("skill disabled", "name", name)
	return nil
}

// Toggle 切换指定 Skill 的启用状态。
func (m *SkillManager) Toggle(name string) error {
	m.mu.RLock()
	skill, ok := m.skills[name]
	if !ok {
		m.mu.RUnlock()
		return &errNotFound{name: name}
	}
	enabled := skill.Enabled
	m.mu.RUnlock()

	if enabled {
		return m.Disable(name)
	}
	return m.Enable(name)
}

// IsEnabled 检查指定 Skill 是否启用。
func (m *SkillManager) IsEnabled(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	skill, ok := m.skills[name]
	return ok && skill.Enabled
}

// persistEnabledLocked 持久化启用状态到 Store（必须持有 mu.Lock）。
func (m *SkillManager) persistEnabledLocked(name string, enabled bool) {
	if m.store == nil {
		return
	}
	key := "skill." + name + ".enabled"
	if err := m.store.Set(context.Background(), key, fmt.Sprintf("%v", enabled)); err != nil {
		m.logger.Warnw("failed to persist skill enabled state",
			"name", name, "error", err)
	}
}

// ============================================================================
// 查询
// ============================================================================

// List 返回所有已注册 Skill 的信息快照（按名称排序）。
func (m *SkillManager) List() []SkillInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]SkillInfo, 0, len(m.skills))
	for _, skill := range m.skills {
		result = append(result, newSkillInfo(skill))
	}

	sortSkillInfo(result)
	return result
}

// GetInfo 获取指定名称的 Skill 信息快照。
func (m *SkillManager) GetInfo(name string) (SkillInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	skill, ok := m.skills[name]
	if !ok {
		return SkillInfo{}, false
	}
	return newSkillInfo(skill), true
}

// Get 获取指定名称的 Skill 指针（内部使用）。
func (m *SkillManager) Get(name string) (*Skill, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	skill, ok := m.skills[name]
	return skill, ok
}

// EnabledNames 返回所有已启用 Skill 的名称列表（按名称排序）。
func (m *SkillManager) EnabledNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var names []string
	for name, skill := range m.skills {
		if skill.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ============================================================================
// 触发注入（prompt Registry 集成）
// ============================================================================

// registerPromptLocked 将 Skill.Content 注册为 prompt Section（必须持有 mu.Lock）。
func (m *SkillManager) registerPromptLocked(skill *Skill) {
	if m.registry == nil || skill.Content == "" {
		return
	}
	// Skill 内容作为 prompt Section 注入，Order 设为 500（附加指令区域）
	// 可通过 RegisterSection 的 order 参数调整
	m.registry.RegisterSection(
		m.skillSectionName(skill.Name),
		500, // 默认 Order，可在 RegisterSection 实现中覆盖
		skill.Content,
		true,
	)
}

// unregisterPromptLocked 从 Registry 移除 Skill 对应的 Section。
func (m *SkillManager) unregisterPromptLocked(name string) {
	if m.registry == nil {
		return
	}
	m.registry.UnregisterSection(m.skillSectionName(name))
}

// skillSectionName 返回 Skill 对应的 prompt Section 名称。
func (m *SkillManager) skillSectionName(name string) string {
	return "skill_" + name
}

// BuildTriggerPrompt 构建可用技能列表段落（包含所有已启用 Skill 的 name + description）。
// 返回的字符串应作为 system prompt 的一个固定 Section（Order 建议在 150 左右）。
//
// LLM 通过调用 use_skill 工具（function calling）来加载技能指令，
// 而非通过文本标签。这与 CodeBuddy 的 use_skill 设计对齐。
//
// 格式：
//
//	## 可用技能
//	当用户请求需要特定技能时，调用 use_skill 工具加载技能指令。
//	- pdf：处理 PDF 文件（提取文本、合并、拆分等）。
//	- xlsx：处理 Excel 表格。
func (m *SkillManager) BuildTriggerPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var buf strings.Builder
	buf.WriteString("## 可用技能\n\n")
	buf.WriteString("当用户请求涉及以下技能领域时，调用 `use_skill` 工具（传入技能名称）加载完整指令。\n")
	buf.WriteString("加载后必须严格遵循技能指令。如果用户请求涉及某个技能领域，应立即调用，不要先尝试其他方式。\n\n")

	enabled := make([]*Skill, 0, len(m.skills))
	for _, s := range m.skills {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	sortSkills(enabled)

	for _, s := range enabled {
		buf.WriteString("- ")
		buf.WriteString(s.Name)
		buf.WriteString("：")
		buf.WriteString(s.Description)
		buf.WriteString("\n")
	}

	return buf.String()
}

// BuildTriggerSection 构建触发判断段落的 prompt Section（可直接注册到 Registry）。
// Order 默认 150（行为规则区域），可通过 order 参数调整。
func (m *SkillManager) BuildTriggerSection(order int) PromptSection {
	return PromptSection{
		Name:    "skill_trigger",
		Order:   order,
		Content: m.BuildTriggerPrompt(),
		Enabled: true,
	}
}

// PromptSection 是传递给外部 Registry 的 Section 描述（避免直接依赖 agent/prompt）。
type PromptSection struct {
	Name    string
	Order   int
	Content string
	Enabled bool
}

// TriggerIfNeeded 解析 LLM 输出，判断是否请求了某个 Skill。
// 匹配格式：<use_skill: skill_name>
// 返回请求的 Skill 名称，若无匹配返回空字符串。
//
// Deprecated: 使用 use_skill 工具（function calling）替代。
// 保留是为了向后兼容旧的文本标签协议。
func (m *SkillManager) TriggerIfNeeded(llmOutput string) string {
	idx := strings.Index(llmOutput, "<use_skill:")
	if idx < 0 {
		return ""
	}
	rest := llmOutput[idx+len("<use_skill:"):]
	end := strings.Index(rest, ">")
	if end < 0 {
		return ""
	}
	name := strings.TrimSpace(rest[:end])
	return name
}

// InjectSkillContent 手动触发将指定 Skill 的 Content 注入 prompt Registry。
//
// Deprecated: 使用 UseSkill 方法替代，它会同时返回 Content 并注入 Registry。
func (m *SkillManager) InjectSkillContent(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	skill, ok := m.skills[name]
	if !ok || !skill.Enabled {
		return false
	}
	if skill.Content == "" {
		return false
	}

	m.registerPromptLocked(skill)
	m.logger.Debugw("skill content injected", "name", name)
	return true
}

// RemoveSkillContent 从 prompt Registry 移除指定 Skill 的 Content。
// 用于多轮对话结束后清理（可选，避免跨会话污染）。
func (m *SkillManager) RemoveSkillContent(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unregisterPromptLocked(name)
}

// ============================================================================
// 辅助函数
// ============================================================================

func newSkillInfo(s *Skill) SkillInfo {
	return SkillInfo{
		Name:          s.Name,
		Description:   s.Description,
		Compatibility: s.Compatibility,
		Enabled:       s.Enabled,
		Source:        s.Source,
		HasContent:    s.Content != "",
		HasScripts:    len(s.Resources.Scripts) > 0,
		HasReferences: len(s.Resources.References) > 0,
		HasAssets:     len(s.Resources.Assets) > 0,
	}
}

func sortSkills(skills []*Skill) {
	n := len(skills)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if skills[i].Name > skills[j].Name {
				skills[i], skills[j] = skills[j], skills[i]
			}
		}
	}
}

func sortSkillInfo(infos []SkillInfo) {
	n := len(infos)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if infos[i].Name > infos[j].Name {
				infos[i], infos[j] = infos[j], infos[i]
			}
		}
	}
}

// errNotFound 是 Skill 未找到的错误。
type errNotFound struct {
	name string
}

func (e *errNotFound) Error() string {
	return fmt.Sprintf("skill: %q not found", e.name)
}

func (e *errNotFound) NotFound() bool {
	return true
}
