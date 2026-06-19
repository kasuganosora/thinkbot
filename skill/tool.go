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
	Command string `json:"command" jsonschema:"The skill name (no arguments). E.g., \"pdf\", \"xlsx\""`
}

// UseSkill 激活指定技能并返回其完整指令内容。
// 调用后技能 Content 同时注入 prompt Registry（多轮持久化）和返回值（即时上下文）。
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
//   1. 调用 SkillManager.UseSkill 激活技能
//   2. 返回技能 Content + 资源路径信息作为 tool_result
func (m *SkillManager) BuildUseSkillTool() llm.Tool {
	mgr := m
	return llm.NewTool("use_skill",
		`Load a Skill to get specialized domain knowledge, workflows, or tool instructions.

Only use this tool when ALL of the following conditions are met:
1. The request involves a specific domain, system, or data format.
2. A relevant Skill is listed in the available skills section.
3. Using the Skill would improve correctness, efficiency, or quality.

After loading a Skill, you MUST follow its instructions. The Skill may define
required workflows or expose executable scripts via its base directory path.

Call this tool IMMEDIATELY as your first action when a relevant skill exists.
Do NOT attempt the task without loading the skill first.`,
		func(ctx *llm.ToolExecContext, input UseSkillInput) (any, error) {
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
// SkillToolProvider — 将 SkillManager 适配为 tools.ToolProvider
//
// 实现 tools.ToolProvider 接口，在每次 Resolve 时根据当前技能状态
// 动态决定是否提供 use_skill 工具。
//
// 行为：
//   - 存在已启用技能 → 返回 [use_skill] 工具
//   - 无已启用技能   → 返回 nil（LLM 不会看到 use_skill）
//   - SubAgent 场景  → 返回 nil（不暴露技能工具给子 Agent）
// ============================================================================

// SkillToolProvider 将 SkillManager 适配为动态工具提供者。
type SkillToolProvider struct {
	Manager *SkillManager
}

// Tools 实现 tools.ToolProvider 接口。
func (p *SkillToolProvider) Tools(ctx context.Context, sctx *tools.ToolSessionContext) ([]llm.Tool, error) {
	if sctx != nil && sctx.IsSubagent {
		return nil, nil
	}
	if !p.Manager.HasEnabledSkills() {
		return nil, nil
	}
	return []llm.Tool{p.Manager.BuildUseSkillTool()}, nil
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

// RegisterTools 将 SkillManager 的 use_skill 工具注册到 ToolManager。
//
// 注册后，ToolManager 在每次解析工具列表时，
// 会通过 SkillToolProvider 动态判断是否提供 use_skill 工具。
//
// 如果 mgr 为 nil，直接返回（no-op）。
func RegisterTools(toolMgr *tools.ToolManager, mgr *SkillManager) error {
	if mgr == nil {
		return nil
	}
	toolMgr.AddProvider(&SkillToolProvider{Manager: mgr})
	return nil
}
