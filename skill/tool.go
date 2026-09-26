package skill

import (
	"context"
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// use_skill 工具 — 对齐 CodeBuddy 的 use_skill 设计
//
// 与 CodeBuddy 的 use_skill 工具对齐：
//   - LLM 通过 function calling 调用 use_skill，传入 command（技能名称）
//   - 调用后技能指令加载到上下文，后续 MUST 遵循
//   - 替代旧的 <use_skill: skill_name> 文本标签协议
//
// 工具参数：
//   - command: 技能名称（如 "pdf"、"xlsx"），无额外参数
//
// 工具行为：
//   1. 查找技能，校验存在性和启用状态
//   2. 将技能 Content 注入 prompt Registry（多轮持久化）
//   3. 返回技能 Content 作为 tool_result（即时上下文）
// ============================================================================

// UseSkillInput 是 use_skill 工具的输入参数。
type UseSkillInput struct {
	// Command 是技能名称（无参数）。如 "pdf"、"xlsx"、"agent-browser"。
	// 传 "list" 可列出全部可用技能（自启发发现，替代常驻清单注入）。
	Command string `json:"command" jsonschema:"Skill name to load, or \"list\" to discover all available skills. E.g. \"pdf\", \"xlsx\", \"list\""`
}

// UseSkill 激活指定技能并返回其完整指令内容。
// 调用后技能 Content 同时注入 prompt Registry（多轮持久化）和返回值（即时上下文）。
// 传 "list" 时不加载任何技能，仅返回可用技能清单（自启发发现）。
func (m *SkillManager) UseSkill(name string) (*Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	skill, ok := m.skills[name]
	if !ok {
		// 列出可用技能帮助 LLM 自纠正
		available := m.availableNamesLocked()
		return nil, fmt.Errorf("skill %q not found. Available: %s", name, strings.Join(available, ", "))
	}
	if !skill.Enabled {
		return nil, fmt.Errorf("skill %q is disabled", name)
	}
	if skill.Content == "" {
		return nil, fmt.Errorf("skill %q has no content", name)
	}

	// 注入 prompt Registry（多轮持久化）
	m.registerPromptLocked(skill)

	m.logger.Debugw("skill activated via use_skill tool",
		"name", name,
		"has_scripts", len(skill.Resources.Scripts) > 0,
		"dir", skill.Dir,
	)
	return skill, nil
}

// BuildUseSkillTool 构造 use_skill 工具定义（llm.Tool）。
//
// 该工具注册到 LLM function calling 后，LLM 可通过调用 use_skill 来加载技能指令。
// 工具 Execute 函数会：
//  1. 调用 SkillManager.UseSkill 激活技能
//  2. 返回技能 Content + 资源路径信息作为 tool_result
func (m *SkillManager) BuildUseSkillTool() llm.Tool {
	mgr := m
	return llm.NewTool("use_skill",
		`Load a Skill to obtain specialized domain knowledge, workflows, or tool instructions.

Use this tool when the request involves a specific domain, system, or data format.

Rules:
- DISCOVERY: The full skill catalog is NOT injected into your context. Prefer skill_search with a few keywords to discover matching skills cheaply; use use_skill with command "list" only when you truly need the full catalog, then load the matching one.
- CRITICAL: Call this tool IMMEDIATELY as your first action when a relevant Skill exists. Do NOT attempt the task, and do NOT call other tools, before the Skill is loaded.
- After loading, you MUST follow the Skill's instructions. They override your general defaults for that task.
- The result may include `+"`baseDir`"+`, `+"`scripts`"+` and `+"`references`"+`. Prefer the Skill's own scripts over improvising an equivalent yourself.
- NEVER mention or describe a Skill without actually loading it.
- Load each Skill at most once per task. Do NOT reload a Skill that is already active.
- If the call fails because the Skill does not exist, the error lists the available names. Pick the correct one, or continue without a Skill — do NOT invent skill names.

<example>
user: 帮我把这个 PDF 里的表格提取出来
assistant: [calls use_skill with command "list" if unsure, then calls use_skill with command "pdf", then follows the loaded instructions]
</example>`,
		func(ctx *llm.ToolExecContext, input UseSkillInput) (any, error) {
			// 自启发发现：list 不加载技能，仅返回完整清单
			if input.Command == "list" {
				return map[string]any{
					"status": "list",
					"skills": mgr.BuildSkillListPrompt(),
					"hint":   "Call use_skill with a skill's name to load it.",
				}, nil
			}

			s, err := mgr.UseSkill(input.Command)
			if err != nil {
				return nil, err
			}

			result := map[string]any{
				"status":  "loaded",
				"skill":   s.Name,
				"content": s.Content,
			}

			// 暴露脚本路径（技能可能定义可执行脚本）
			if len(s.Resources.Scripts) > 0 {
				result["scripts"] = s.Resources.Scripts
			}
			if len(s.Resources.References) > 0 {
				result["references"] = s.Resources.References
			}
			if s.Dir != "" {
				result["baseDir"] = s.Dir
			}

			return result, nil
		})
}

// availableNamesLocked 返回所有已注册技能的名称（必须持有 mu.RLock 或 mu.Lock）。
func (m *SkillManager) availableNamesLocked() []string {
	names := make([]string, 0, len(m.skills))
	for name := range m.skills {
		names = append(names, name)
	}
	return names
}

// ============================================================================
// skill_search 工具 — 关键词粗检索（L1，两段式检索的第一步）
//
// 与 use_skill 分工：
//   - skill_search：按关键词廉价检索技能元数据（name + 截断描述的命中列表），
//     是发现技能的第一步；只读元数据，不加载正文
//   - use_skill：加载命中的技能（传技能名），或传 "list" 取完整清单（较大）
//
// 设计参考 research/skill-search-two-stage.md：L1 粗搜必须便宜快，
// 重阅读理解的 L2 精搜（subagent）不在本工具职责内。
// ============================================================================

// SkillSearchInput 是 skill_search 工具的输入参数。
type SkillSearchInput struct {
	// Query 检索关键词，多个词用空格分隔（全部命中才算候选，AND 语义）。
	Query string `json:"query" jsonschema:"Space-separated keywords to search skill names and descriptions, e.g. \"pdf extract\", \"excel 表格\". All keywords must match"`

	// Limit 最多返回的命中条数，默认 10，上限 20。
	Limit int `json:"limit,omitempty" jsonschema:"Maximum number of hits to return. Default 10, max 20"`
}

// BuildSkillSearchTool 构造 skill_search 工具定义（llm.Tool）。
//
// 构造方式与 BuildUseSkillTool 完全对齐（llm.NewTool 泛型风格）。
// Execute 返回：
//
//	map[string]any{
//	  "status": "search",                         // 固定标记本次调用类型
//	  "skills": []SearchHit,                      // 命中列表（name + 截断 description + score）
//	  "hint":   "Call use_skill with the skill's name to load it."
//	}
//
// 这里选择直接返回 SearchHit 列表（结构化，带 score，便于上层二次加工），
// 而非逐行字符串——两者二选一，本实现取前者并在注释中说明。
//
// query 为空（或只有空格）时不返回错误，而是返回空结果 + 提示：
// 让 LLM 拿到确定性反馈自行纠正（补关键词重试），错误应留给真正的失败场景。
func (m *SkillManager) BuildSkillSearchTool() llm.Tool {
	mgr := m
	return llm.NewTool("skill_search",
		`Cheaply search skills by keywords. This is the FIRST step of skill discovery: it returns a short list of hits (skill name + truncated description), without loading any skill content.

Input: a few keywords separated by spaces (all keywords must match). Use the language of the task (Chinese keywords work).

Workflow:
- Call this tool first to find candidate skills for a specialized domain (a file format, a framework, a workflow, a known tool).
- Then call use_skill with the chosen skill name to load it.
- Call use_skill with command "list" ONLY when you need the full catalog of all available skills (larger) — prefer skill_search for keyword discovery.

Rules:
- This tool never loads skill content. It only returns metadata hits.
- If nothing matches, broaden or change the keywords and retry; if still nothing, proceed without a Skill.`,
		func(ctx *llm.ToolExecContext, input SkillSearchInput) (any, error) {
			hits := mgr.SearchSkills(input.Query, input.Limit)
			return map[string]any{
				"status": "search",
				// 复用 SearchHit 列表（name/description/score，description 已截断）
				"skills": hits,
				"hint":   "Call use_skill with the skill's name to load it.",
			}, nil
		})
}

// ============================================================================
// SkillToolProvider — 将 SkillManager 适配为 tools.ToolProvider
//
// 实现 tools.ToolProvider 接口，在每次 Resolve 时根据当前技能状态
// 动态决定是否提供技能工具。
//
// 行为：
//   - 存在已启用技能 → 返回 [use_skill, skill_search] 两个工具
//     （skill_search 是便宜的发现第一步，use_skill 负责加载/取全量清单）
//   - 无已启用技能   → 返回 nil（LLM 不会看到这两个工具）
//   - 主 Agent 与子 Agent 共用：子 Agent 同样可检索/加载技能（仅不能 spawn）
// ============================================================================

// SkillToolProvider 将 SkillManager 适配为动态工具提供者。
type SkillToolProvider struct {
	Manager *SkillManager
}

// Tools 实现 tools.ToolProvider 接口。
// 主 Agent 与子 Agent 共用同一套技能工具（子 Agent 仅不能 spawn，其余有权使用的工具皆可访问）。
func (p *SkillToolProvider) Tools(ctx context.Context, sctx *tools.ToolSessionContext) ([]llm.Tool, error) {
	if !p.Manager.HasEnabledSkills() {
		return nil, nil
	}
	// use_skill（加载/全量清单）+ skill_search（关键词粗检索）成对提供
	return []llm.Tool{
		p.Manager.BuildUseSkillTool(),
		p.Manager.BuildSkillSearchTool(),
	}, nil
}

// HasEnabledSkills 返回是否存在已启用的技能。
func (m *SkillManager) HasEnabledSkills() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.skills {
		if s.Enabled {
			return true
		}
	}
	return false
}

// ============================================================================
// RegisterTools — 便捷注册函数（对齐 mcp.RegisterTools 模式）
// ============================================================================

// RegisterTools 将 SkillManager 的技能工具（use_skill + skill_search）注册到 ToolManager。
//
// 注册后，ToolManager 在每次解析工具列表时，
// 会通过 SkillToolProvider 动态判断是否提供这两个技能工具。
//
// 如果 mgr 为 nil，直接返回（no-op）。
func RegisterTools(toolMgr *tools.ToolManager, mgr *SkillManager) error {
	if mgr == nil {
		return nil
	}
	toolMgr.AddProvider(&SkillToolProvider{Manager: mgr})
	return nil
}
