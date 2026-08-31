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
//   - LLM 自主判断触发：所有已启用 Skill 的 name+description 常驻上下文
//   - 与 prompt.Registry 集成：Skill 正文作为 Section 注册，自动组装进 system prompt
//   - 与 config.Store 集成：启用状态持久化到数据库
package skill

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
