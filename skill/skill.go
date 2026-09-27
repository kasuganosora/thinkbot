// Package skill 实现 Anthropic Skills 规范的 Skill 系统。
//
// Skill 是"增强系统提示词的知识与指令包"，与 Tool（执行能力）平级但职责不同：
//   - Tool = 给 LLM 执行能力（function calling）
//   - Skill = 给 LLM 知识、指令、工作流模板（注入 system prompt）
//
// 设计要点：
//   - 每个 Skill 是一个目录，核心文件为 SKILL.md（YAML front matter + Markdown 正文）
//   - 附加资源（scripts/、references/、assets/）按 Anthropic Skills 规范支持
//   - 三级上下文加载（渐进式披露）：元数据 → SKILL.md 正文 → 附加资源
//   - LLM 自主判断触发：技能清单不常驻上下文，经 skill_search / use_skill "list" 按需发现
//   - 与 prompt.Registry 集成：Skill 正文作为 Section 注册，自动组装进 system prompt
//   - 与 config.Store 集成：启用状态持久化到数据库
//   - 技能分级（light/heavy）：front matter 声明 delegation: preferred，或未声明时
//     正文超过 SkillHeavyContentBytes（12000）字节判为重型；重型技能在 use_skill / skill_search 返回中标注，
//     并建议经 spawn 子代理委托执行（见 Skill.IsHeavy）
//   - 卸载：use_skill 支持 "unload:<skill>" 子命令，任务完成后移除技能正文、
//     保持上下文干净，之后可再次 use_skill 重新加载（见 SkillManager.UnloadSkill）
package skill

// ============================================================================
// 委托执行分级（delegation）
//
// 重型技能注入的上下文很大，直接在主 Agent 上下文里执行会挤占工作记忆；
// 框架已有 spawn 工具（subagent 包）可把任务委托给子代理执行。
// 分级只影响提示词标注与 use_skill 返回的委托建议，不改变加载行为。
// ============================================================================

const (
	// DelegationPreferred 是 front matter 的 delegation 字段取值之一，
	// 表示技能作者显式声明该技能适合委托给子代理执行（delegation: preferred）。
	// 显式声明优先于体积阈值自动判定；其他显式取值（如 none）一律视为轻型。
	DelegationPreferred = "preferred"

	// SkillLevelLight / SkillLevelHeavy 是分级结果标签，
	// 用于 skill_search 命中摘要与技能清单的 light/heavy 标注。
	SkillLevelLight = "light"
	SkillLevelHeavy = "heavy"

	// SkillHeavyContentBytes 是体积自动分级阈值（字节，按 UTF-8 字节计）：
	// 未显式声明 delegation 时，Content 超过该值视为重型技能，恰好等于阈值仍为轻型。
	// 原为 3000（约 1K token）：50 个内置技能里 31 个被判重型，连一次性查询类技能
	// 也被提示委托子代理。12000 字节约 3–4K token，只有真正的大说明书才算重型
	// （2026-09-28：内置 7/50，bot 自装 2/12）。
	SkillHeavyContentBytes = 12000
)

// ============================================================================
// 核心数据结构
// ============================================================================

// Skill 描述一个已加载的技能。
// 对应文件系统中的一个 Skill 目录（以 SKILL.md 为核心）。
type Skill struct {
	// Name 技能唯一标识符（小写+连字符，如 "pdf"、"web-search"）。
	Name string

	// Description 技能功能和触发场景描述。
	// 这是触发判断的核心依据，需要覆盖用户可能省略明确技能名称的模糊请求场景。
	Description string

	// Compatibility 声明技能运行需要的依赖（如需要的 Tool 名称、最低 LLM 能力等）。
	Compatibility []string

	// Content 是 SKILL.md 的 Markdown 正文（即指令内容）。
	// 触发后通过 prompt Section 注入 system prompt。
	Content string

	// Resources 附加资源路径（可选）。
	Resources SkillResources

	// Enabled 是否启用。禁用后元数据仍可见，但不会注入 Content。
	Enabled bool

	// Source 来源："fs"（文件系统）或 "db"（数据库）。
	Source string

	// Dir 文件系统路径（Source="fs" 时有值）。
	Dir string

	// Delegation 是 front matter delegation 字段的原始值（可选，向后兼容：缺省为空）。
	// 目前取值 "preferred" 表示技能作者显式声明适合委托子代理执行；空或其他值表示未声明。
	// 分级结果（重型/轻型）由 IsHeavy / Level 动态计算，不在此持久化。
	Delegation string
}

// IsHeavy 判断技能是否为「重型」（建议委托 spawn 子代理执行）。
// 判定规则（显式声明优先于体积阈值）：
//  1. Delegation == "preferred" → 重型（作者显式声明委托）；
//  2. 未显式声明（空或其他取值）→ Content 字节数 > SkillHeavyContentBytes 时视为重型。
func (s *Skill) IsHeavy() bool {
	if s.Delegation == DelegationPreferred {
		return true
	}
	return len(s.Content) > SkillHeavyContentBytes
}

// Level 返回技能分级标签："heavy"（重型，建议委托）或 "light"（轻型）。
// 供 skill_search 命中摘要、技能清单标注与 use_skill 委托提示统一取用。
func (s *Skill) Level() string {
	if s.IsHeavy() {
		return SkillLevelHeavy
	}
	return SkillLevelLight
}

// SkillResources 描述 Skill 目录下的附加资源。
type SkillResources struct {
	Scripts    []string // scripts/ 下可执行脚本路径
	References []string // references/ 下参考文档路径
	Assets     []string // assets/ 下资产文件路径
}

// SkillMeta 是 SKILL.md 的 YAML front matter。
type SkillMeta struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	Compatibility []string `yaml:"compatibility"`
	Enabled       *bool    `yaml:"enabled"`

	// Delegation 委托执行声明（可选，向后兼容：不写即为空）。
	// 目前取值 "preferred"：技能体积大/执行链长，建议委托 spawn 子代理执行。
	Delegation string `yaml:"delegation"`
}

// SkillInfo 是 Skill 的只读详情快照，供列表展示、API 返回等场景使用。
type SkillInfo struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Compatibility []string `json:"compatibility,omitempty"`
	Enabled       bool     `json:"enabled"`
	Source        string   `json:"source"`
	HasContent    bool     `json:"hasContent"`
	HasScripts    bool     `json:"hasScripts"`
	HasReferences bool     `json:"hasReferences"`
	HasAssets     bool     `json:"hasAssets"`
}

// SearchHit 是 skill_search 粗检索（L1）的单条命中结果。
// 仅携带元数据（name + 截断后的 description + 得分），不读技能正文，保证检索便宜快。
// 风格对齐 SkillInfo：字段带 json tag，供工具返回 / API 序列化使用。
type SearchHit struct {
	// Name 技能唯一标识符，命中后用 use_skill "name" 加载。
	Name string `json:"name"`

	// Description 技能描述。超过 maxDescriptionRunes 个字符（rune 数）时
	// 截断到该长度并追加省略号 "…"，控制回包体积。
	Description string `json:"description"`

	// Score 排序得分：name 命中权重（skillSearchNameWeight）远高于
	// description 命中（skillSearchDescWeight），仅降序排序用。
	Score int `json:"score"`

	// Level 分级标注："light"（轻型）或 "heavy"（重型，建议经 spawn 委托执行）。
	// 由加载时的 delegation 声明与体积阈值共同决定（见 Skill.IsHeavy），
	// 让 LLM 在粗检索阶段就能把重型技能的执行委托给子代理。
	Level string `json:"level"`
}
