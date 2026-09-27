package skill

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
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

	// loaded 记录经 UseSkill 显式加载的技能（对话级使用状态，非持久化）。
	// 与 Enabled（管理员级生命周期开关）正交：unload 只影响本集合与 prompt 注入，
	// 不改变 skills 中的注册信息，也不写 Store。
	loaded map[string]struct{}

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
		loaded:   make(map[string]struct{}),
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
	m.refreshTriggerLocked()
}

// Unregister 移除指定 Skill，并从 prompt Registry 清掉其段落与触发清单。
func (m *SkillManager) Unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	old, ok := m.skills[name]
	if !ok {
		return
	}
	if old.Enabled && old.Content != "" {
		m.unregisterPromptLocked(name)
	}
	delete(m.skills, name)
	delete(m.loaded, name) // 技能已不存在，加载状态一并失效
	m.refreshTriggerLocked()
	m.logger.Infow("skill unregistered", "name", name)
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
	m.refreshTriggerLocked()
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
	delete(m.loaded, name) // 禁用即不可用，加载状态一并失效

	m.persistEnabledLocked(name, false)
	m.refreshTriggerLocked()
	m.logger.Infow("skill disabled", "name", name)
	return nil
}

// Toggle 切换指定 Skill 的启用状态。
func (m *SkillManager) Toggle(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	skill, ok := m.skills[name]
	if !ok {
		return &errNotFound{name: name}
	}

	if skill.Enabled {
		// inline disable 逻辑
		skill.Enabled = false
		if skill.Content != "" {
			m.unregisterPromptLocked(name)
		}
		delete(m.loaded, name) // 禁用即不可用，加载状态一并失效
		m.persistEnabledLocked(name, false)
		m.logger.Infow("skill disabled", "name", name)
	} else {
		// inline enable 逻辑
		skill.Enabled = true
		if skill.Content != "" {
			m.registerPromptLocked(skill)
		}
		m.persistEnabledLocked(name, true)
		m.logger.Infow("skill enabled", "name", name)
	}
	m.refreshTriggerLocked()
	return nil
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
// 已加载 / 卸载（use_skill 的对话级使用状态）
//
// 与 Enabled 的区别：Enabled 是管理员级生命周期开关（持久化到 Store），
// loaded 只表示「当前经 use_skill 显式加载、说明书仍在上下文中」。
// ============================================================================

// IsLoaded 返回指定技能当前是否处于已加载状态（经 UseSkill 加载且未被卸载）。
func (m *SkillManager) IsLoaded(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.loaded[name]
	return ok
}

// LoadedNames 返回当前已加载技能的名称列表（按名称排序）。
func (m *SkillManager) LoadedNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.loaded))
	for name := range m.loaded {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// UnloadSkill 卸载已加载的技能：
//   - 若技能未处于已加载状态，返回 ok=false（不算错误，调用方可据此给出友好提示）
//   - 否则从 prompt Registry 移除其 Content Section、从 loaded 集合删除，并返回标记消息
//
// 返回的 note 是给模型的系统级标记（形如 "[skill unloaded: xxx] ..."），
// 依据调研结论以 tool_result 形式进入本轮上下文并持久化，无需框架改动。
// 卸载不改变技能的 Enabled 状态（管理员开关与加载状态正交）。
func (m *SkillManager) UnloadSkill(name string) (note string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, loaded := m.loaded[name]; !loaded {
		// 未加载：非错误场景，由调用方返回友好提示（如 not currently loaded）
		return "", false
	}

	// 从 prompt Registry 移除 Content Section（下一轮 system prompt 即不含该内容）
	m.unregisterPromptLocked(name)
	delete(m.loaded, name)
	m.logger.Infow("skill unloaded via use_skill tool", "name", name)
	return UnloadedNote(name), true
}

// UnloadedNote 构造卸载完成的标记消息（进入本轮 tool_result，提示模型该技能说明书已失效）。
func UnloadedNote(name string) string {
	return fmt.Sprintf("[skill unloaded: %s] 该技能说明书已失效，后续如需使用需重新 use_skill 加载。", name)
}

// ============================================================================
// 关键词粗检索（L1 skill_search）
// ============================================================================

// skill_search 粗检索的计分 / 截断参数。
// 设计参考 research/skill-search-two-stage.md 的 L1 层：
// 只对 name/description 元数据做子串匹配，不读正文，保证主上下文调用便宜快。
const (
	// skillSearchNameWeight name 命中单个关键词的权重。
	// 明显高于 description 命中（10:1），name 命中的技能优先返回。
	skillSearchNameWeight = 10

	// skillSearchDescWeight description 命中单个关键词的权重。
	skillSearchDescWeight = 1

	// skillSearchDefaultLimit limit 缺省/非法时使用的默认返回条数。
	skillSearchDefaultLimit = 10

	// skillSearchMaxLimit 单次检索允许的最大返回条数（limit > 20 时截断到 10）。
	skillSearchMaxLimit = 20

	// maxDescriptionRunes 单条 description 的最大 rune 数（含中文）。
	// 超过则截断到该长度并追加 "…"。
	maxDescriptionRunes = 200
)

// SearchSkills 对所有已启用技能做关键词粗检索（L1）。
//
// 匹配规则：
//   - query 按空格分词（strings.Fields），忽略大小写（strings.ToLower）
//   - 子串匹配 name 或 description；所有关键词都命中才算候选（AND 语义）
//
// 计分规则：name 每命中一个关键词 +10，description 每命中一个 +1（name 明显更高）。
//
// 排序规则：分数降序；同分按 name 字典序稳定排序。
//
// limit：<=0 或 >20 时取默认 10；返回前 limit 条（不足则全部）。
// 每条 Description 超过 200 字符（rune 数）时截断并追加 "…"。
//
// 空 query 或无命中时返回空切片（非 nil，与 List 风格一致）。
func (m *SkillManager) SearchSkills(query string, limit int) []SearchHit {
	hits := make([]SearchHit, 0, skillSearchDefaultLimit)

	keywords := searchKeywords(query)
	if len(keywords) == 0 {
		return hits // 空 query：返回空切片
	}

	// limit 归一化：非法值统一回落到默认 10
	if limit <= 0 || limit > skillSearchMaxLimit {
		limit = skillSearchDefaultLimit
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, s := range m.skills {
		// 只检索已启用技能（与 BuildSkillListPrompt / use_skill 可加载范围一致）
		if !s.Enabled {
			continue
		}
		score, ok := scoreSkill(s, keywords)
		if !ok {
			continue // 任一关键词未命中 → 不是候选
		}
		hits = append(hits, SearchHit{
			Name:        s.Name,
			Description: truncateRunes(s.Description, maxDescriptionRunes),
			Score:       score,
			// 分级标注（light/heavy）：让 LLM 在粗检索阶段即可识别重型技能，
			// 把执行委托给 spawn 子代理，避免大说明书挤占主上下文。
			Level: s.Level(),
		})
	}

	sortSearchHits(hits)

	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// searchKeywords 将 query 切分为小写关键词（strings.Fields 按空格分词，忽略大小写）。
// 返回空切片表示空 query（调用方应直接返回空结果）。
func searchKeywords(query string) []string {
	fields := strings.Fields(strings.ToLower(query))
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// scoreSkill 计算单个技能对所有关键词的得分。
// 返回 (总分, 是否全部命中)：任一关键词在 name 与 description 都未出现 → (0, false)。
func scoreSkill(s *Skill, keywords []string) (int, bool) {
	name := strings.ToLower(s.Name)
	desc := strings.ToLower(s.Description)

	score := 0
	for _, kw := range keywords {
		// name 命中权重远高于 description（10:1）
		switch {
		case strings.Contains(name, kw):
			score += skillSearchNameWeight
		case strings.Contains(desc, kw):
			score += skillSearchDescWeight
		default:
			return 0, false // AND 语义：有未命中的关键词即淘汰
		}
	}
	return score, true
}

// sortSearchHits 按分数降序排序；同分按 name 字典序升序（稳定排序）。
func sortSearchHits(hits []SearchHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Name < hits[j].Name
	})
}

// truncateRunes 按 rune 数截断字符串（正确处理中文等多字节字符）。
// 超过 max 时截断到 max 个 rune 并追加省略号 "…"，否则原样返回。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
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
//	## Available Skills
//	When a request falls into one of the domains below, call `use_skill` ...
//	- pdf — 处理 PDF 文件（提取文本、合并、拆分等）。
//	- xlsx — 处理 Excel 表格。
func (m *SkillManager) BuildTriggerPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buildTriggerPromptLocked()
}

func (m *SkillManager) buildTriggerPromptLocked() string {
	// 自启发加载（lazy discovery）：不常驻技能清单，节省每次请求的 token。
	// 技能数量增长时本段长度恒定；LLM 需要时通过 skill_search / use_skill "list" 按需发现。
	var buf strings.Builder
	buf.WriteString("## Skills\n\n")
	buf.WriteString("A Skill is a package of specialized instructions for a specific domain, system or data format. Skills exist in this system, but the list is intentionally NOT shown here to save context.\n\n")
	buf.WriteString("When a request involves a specialized domain (a file format, a framework, a workflow, a known tool, a repeated task pattern), discover and load a Skill first:\n")
	// 发现两步走：skill_search 按关键词粗检索（便宜的第一步）；use_skill "list" 返回全部清单（较大）。
	buf.WriteString("1. Call `skill_search` with a few keywords to cheaply find matching skills (name + short description per hit).\n")
	buf.WriteString("2. Call `use_skill` with command \"list\" only when you need the full catalog of all available skills (larger).\n")
	buf.WriteString("3. If a matching skill exists, call `use_skill` with that skill's name as your FIRST action. Do NOT attempt the task, guess at a workflow, or call other tools before the skill is loaded.\n")
	buf.WriteString("4. After loading, follow the skill's instructions exactly. They override your general defaults for that task.\n")
	buf.WriteString("5. If no skill matches, proceed normally without loading. Load each skill at most once per task, and do NOT reload one already active.\n")
	buf.WriteString("6. When the task that needed a Skill is finished and you no longer need it, call `use_skill` with \"unload:<skill>\" to release it and keep the context clean; load it again with `use_skill` if needed later.\n")
	buf.WriteString("7. DELEGATION: skills are graded `light` / `heavy` (see skill_search hits). A heavy Skill has a large instruction body; after loading it, prefer delegating the actual work to a subagent with the existing `spawn` tool and keep only the conclusions in your own context.\n")
	buf.WriteString("8. NEVER mention a skill to the user without actually loading it.\n\n")
	buf.WriteString("These instructions are in English, but you reply to the user in Chinese (中文) by default — if the user writes in another language, match theirs.\n")
	return buf.String()
}

// BuildSkillListPrompt 返回完整的可用技能清单（name — description），供按需发现使用。
// 触发段落（buildTriggerPromptLocked）不再内联此清单；LLM 通过 use_skill "list" 获取。
func (m *SkillManager) BuildSkillListPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buildSkillListLocked()
}

func (m *SkillManager) buildSkillListLocked() string {
	enabled := make([]*Skill, 0, len(m.skills))
	for _, s := range m.skills {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	sortSkills(enabled)

	var buf strings.Builder
	if len(enabled) == 0 {
		buf.WriteString("No skills available.\n")
		return buf.String()
	}
	buf.WriteString(fmt.Sprintf("%d skills available:\n", len(enabled)))
	for _, s := range enabled {
		buf.WriteString("- ")
		buf.WriteString(s.Name)
		// 分级标注：heavy 表示重型技能（体积超阈值或声明 delegation: preferred），
		// 建议加载后经 spawn 子代理委托执行；light 为普通技能。
		buf.WriteString(" [")
		buf.WriteString(s.Level())
		buf.WriteString("] — ")
		// 单条描述超长时截断（rune 数，注意中文），清单仍返回全部条目
		buf.WriteString(truncateRunes(s.Description, maxDescriptionRunes))
		buf.WriteString("\n")
	}
	return buf.String()
}

const skillTriggerSection = "skill_trigger"
const skillTriggerOrder = 150

// refreshTriggerLocked 用当前已启用技能清单重写 skill_trigger 段落（必须持有 mu.Lock）。
func (m *SkillManager) refreshTriggerLocked() {
	if m.registry == nil {
		return
	}
	m.registry.RegisterSection(skillTriggerSection, skillTriggerOrder, m.buildTriggerPromptLocked(), true)
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
