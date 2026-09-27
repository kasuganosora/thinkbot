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
//
// 卸载子命令（unload 前缀）：
//   - command 形如 "unload:pdf" 时卸载已加载技能，返回 status:"unloaded"
//   - 卸载未加载技能返回友好提示（status:"not_loaded"），不算错误
// ============================================================================

// UseSkillInput 是 use_skill 工具的输入参数。
type UseSkillInput struct {
	// Command 是技能名称（无参数）。如 "pdf"、"xlsx"、"agent-browser"。
	// 传 "list" 可列出全部可用技能（自启发发现，替代常驻清单注入）。
	// 传 "unload:<skill>" 可卸载已加载技能（任务完成后释放上下文）。
	Command string `json:"command" jsonschema:"Skill name to load (e.g. \"pdf\"), \"list\" to discover all available skills, or \"unload:<skill>\" to unload a loaded skill and free context"`
}

// UseSkill 激活指定技能并返回其完整指令内容。
// 调用后技能 Content 同时注入 prompt Registry（多轮持久化）和返回值（即时上下文），
// 并登记进 loaded 集合（可经 UnloadSkill 卸载）。
// 传 "list" 时不加载任何技能，仅返回可用技能清单（自启发发现）。
//
// 幂等可重入：已加载（含曾卸载后重新调用）时再次调用会重新返回全文，
// 不会因「曾加载/曾卸载」而拒绝——重载语义即重新提供说明书。
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
	// 登记加载状态（供 unload / IsLoaded 使用；重复加载幂等覆盖）
	m.loaded[name] = struct{}{}

	m.logger.Debugw("skill activated via use_skill tool",
		"name", name,
		"has_scripts", len(skill.Resources.Scripts) > 0,
		"dir", skill.Dir,
	)
	return skill, nil
}

// unloadCommandPrefix 是 use_skill 卸载子命令的前缀（如 "unload:pdf"）。
const unloadCommandPrefix = "unload:"

// DelegationNote 构造重型技能的委托执行提示（随 use_skill 的 tool_result 进入本轮上下文）。
// 直接引用框架现有的 spawn 工具名（subagent 包提供），只做提示引导，不新造能力。
func DelegationNote() string {
	return "此技能较大，建议通过 spawn 子代理（subagent）委托执行以保持主上下文干净：用 spawn 工具派生子代理，把遵循该技能说明书的任务交给它执行，只取回结论。"
}

// BuildUseSkillTool 构造 use_skill 工具定义（llm.Tool）。
//
// 该工具注册到 LLM function calling 后，LLM 可通过调用 use_skill 来加载技能指令。
// 工具 Execute 函数会：
//  1. 调用 SkillManager.UseSkill 激活技能
//  2. 返回技能 Content + 资源路径信息作为 tool_result
//  3. command 为 "unload:<skill>" 时改为卸载技能，返回 status:"unloaded"
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
- DELEGATION: Hits and results are tagged light / heavy. A heavy Skill has a large instruction body (or declares delegation). When a heavy Skill is loaded, the result carries a note: prefer delegating the actual work to a subagent with the existing spawn tool and keep only the conclusions in your context. Unload the Skill when done.
- NEVER mention or describe a Skill without actually loading it.
- Load each Skill at most once per task. Do NOT reload a Skill that is already active.
- UNLOAD: When the task that needed a Skill is finished and you no longer need it, call use_skill with command "unload:<skill>" (e.g. "unload:pdf") to release it. The returned note means the Skill's instructions no longer apply; if you need it again later, simply load it again with use_skill. Unloading keeps the context clean when working across many different tasks.
- If the call fails because the Skill does not exist, the error lists the available names. Pick the correct one, or continue without a Skill — do NOT invent skill names.

<example>
user: 帮我把这个 PDF 里的表格提取出来
assistant: [calls use_skill with command "list" if unsure, then calls use_skill with command "pdf", then follows the loaded instructions]

user: 现在再帮我处理这个 Excel
assistant: [calls use_skill with command "unload:pdf" since the PDF task is done, then calls use_skill with command "xlsx"]

user: 帮我按这份大型行程生成规范做一份完整的行程规划报告
assistant: [calls skill_search with "trip planner", sees the hit is tagged heavy, calls use_skill with that skill, then delegates the heavy reading-and-drafting work to a subagent via the spawn tool and returns only the conclusion]
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

			// 卸载子命令：unload:<skill>，卸载后返回标记消息作为 tool_result
			if name, isUnload := parseUnloadCommand(input.Command); isUnload {
				note, unloaded := mgr.UnloadSkill(name)
				if !unloaded {
					// 未加载不算错误：返回友好提示让 LLM 拿到确定性反馈
					return map[string]any{
						"status": "not_loaded",
						"skill":  name,
						"note":   fmt.Sprintf("skill %q is not currently loaded; nothing to unload", name),
					}, nil
				}
				return map[string]any{
					"status": "unloaded",
					"skill":  name,
					"note":   note,
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
				// 分级标注：heavy / light，与 skill_search 命中摘要一致
				"level": s.Level(),
			}

			// 重型技能委托提示：说明书很大，建议把任务委托给 spawn 子代理执行，
			// 保持主上下文干净（只引用现有 spawn 工具名，不新造能力）。
			// 轻型技能不加提示，避免噪音。
			if s.IsHeavy() {
				result["note"] = DelegationNote()
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

// parseUnloadCommand 解析 use_skill 的卸载子命令。
// command 形如 "unload:pdf" 时返回 ("pdf", true)；其余（含纯 "unload"、空技能名）返回 ("", false)，
// 由调用方按普通技能名处理（自然报 not found，帮助 LLM 自纠正）。
func parseUnloadCommand(command string) (string, bool) {
	if !strings.HasPrefix(command, unloadCommandPrefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(command, unloadCommandPrefix))
	if name == "" {
		return "", false
	}
	return name, true
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
//	  "skills": []SearchHit,                      // 命中列表（name + 截断 description + score + level）
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
- Each hit carries a level tag: light (normal) or heavy (large instruction body or delegation declared). For heavy hits, plan to delegate the work to a subagent via the existing spawn tool after loading, instead of reading everything in your own context.
- If nothing matches, broaden or change the keywords and retry; if still nothing, proceed without a Skill.`,
		func(ctx *llm.ToolExecContext, input SkillSearchInput) (any, error) {
			hits := mgr.SearchSkills(input.Query, input.Limit)
			return map[string]any{
				"status": "search",
				// 复用 SearchHit 列表（name/description/score/level，description 已截断）；
				// level 为 light/heavy 分级标注，提示 LLM 重型技能宜委托 spawn 子代理执行
				"skills": hits,
				"hint":   "Call use_skill with the skill's name to load it. Hits tagged heavy are large: prefer delegating the work to a subagent via the spawn tool.",
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
