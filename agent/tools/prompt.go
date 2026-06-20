package tools

import (
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/prompt"
)

// ============================================================================
// ToolPromptManager — 工具提示词管理（集成 prompt 模块）
// ============================================================================

// ToolPromptManager 负责将工具提示词注册到 prompt.Registry。
//
// 每个工具可以定义一个 ToolPromptSection（使用规则、注意事项等），
// ToolPromptManager 将这些段落转换为 prompt.Section 注册到 Registry，
// 这样 PromptStage 在组装 system prompt 时会自动包含工具说明。
type ToolPromptManager struct {
	registry *prompt.Registry // 目标 prompt Registry
	prefix   string           // Section 名称前缀（避免冲突）

	// 已注册的段落名称（用于清理或重新注册）
	registered []string
}

// NewToolPromptManager 创建工具提示词管理器。
//
// registry 是 prompt 模块的注册中心。
// prefix 是注册到 Registry 的 Section 名称前缀，默认 "tool_"。
func NewToolPromptManager(registry *prompt.Registry, prefix string) *ToolPromptManager {
	if prefix == "" {
		prefix = "tool_"
	}
	return &ToolPromptManager{
		registry: registry,
		prefix:   prefix,
	}
}

// RegisterToolPrompt 注册单个工具的提示词段落。
func (m *ToolPromptManager) RegisterToolPrompt(section *ToolPromptSection) {
	if section == nil || strings.TrimSpace(section.Content) == "" {
		return
	}

	name := m.prefix + section.Name
	m.registry.Register(prompt.Section{
		Name:    name,
		Order:   section.Order,
		Content: section.Content,
		Enabled: section.Enabled,
	})
	m.registered = append(m.registered, name)
}

// RegisterFromDefs 批量从工具定义中注册提示词段落。
func (m *ToolPromptManager) RegisterFromDefs(defs []ToolDef) {
	for _, def := range defs {
		m.RegisterToolPrompt(def.PromptSection)
	}
}

// UnregisterAll 清除所有已注册的工具提示词段落。
func (m *ToolPromptManager) UnregisterAll() {
	for _, name := range m.registered {
		m.registry.Unregister(name)
	}
	m.registered = nil
}

// RegisteredNames 返回已注册的段落名称列表。
func (m *ToolPromptManager) RegisteredNames() []string {
	out := make([]string, len(m.registered))
	copy(out, m.registered)
	return out
}

// ============================================================================
// Prompt Section Builders — 预定义的工具提示词段落
// ============================================================================

// DefaultToolHeaderSection 返回工具说明的总标题段落。
// 放在所有具体工具段落之前（Order=300），作为工具能力的总述。
func DefaultToolHeaderSection(toolNames []string) *ToolPromptSection {
	if len(toolNames) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("# 可用工具\n\n")
	sb.WriteString("你可以使用以下工具来完成任务。请在合适的场景主动调用工具：\n\n")
	for _, name := range toolNames {
		fmt.Fprintf(&sb, "- `%s`\n", name)
	}
	sb.WriteString("\n调用工具时，确保参数符合 schema 要求。工具结果会自动返回给你用于后续推理。")

	return &ToolPromptSection{
		Name:    "_header",
		Order:   300,
		Content: sb.String(),
		Enabled: true,
	}
}

// DefaultToolRulesSection 返回工具使用的通用规则段落（Order=301）。
func DefaultToolRulesSection() *ToolPromptSection {
	return &ToolPromptSection{
		Name:  "_rules",
		Order: 301,
		Content: `# 工具使用规则

- **只在必要时调用工具**，不要为了调用而调用
- 如果已有足够的信息回答，直接回答而不需要调用工具
- **你可以在一次回复中并行调用多个独立的工具**，大幅提高效率
- 工具调用失败时，向用户说明失败原因并尝试替代方案
- 不要编造工具结果，只使用实际返回的数据
- 对于需要审批的工具，等待用户确认后再执行
- 如果工具输出被截断（出现 truncated 标记），使用更精确的参数重新调用，不要重复相同的大范围查询`,
		Enabled: true,
	}
}

// BuildToolDescriptionSection 为单个工具生成描述段落。
// 会自动从 ToolDef 中提取信息构建提示词。
func BuildToolDescriptionSection(def *ToolDef) *ToolPromptSection {
	if def == nil {
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## 工具：%s\n\n", def.Name)
	if def.Description != "" {
		sb.WriteString(def.Description)
		sb.WriteString("\n\n")
	}

	if len(def.Scopes) > 0 {
		sb.WriteString("适用场景：")
		sb.WriteString(strings.Join(def.Scopes, ", "))
		sb.WriteString("\n\n")
	}

	if def.RequireApproval {
		sb.WriteString("⚠️ 此工具需要用户确认后才能执行。\n\n")
	}

	return &ToolPromptSection{
		Name:    def.Name + "_desc",
		Order:   310, // 具体工具描述在 header/rules 之后
		Content: sb.String(),
		Enabled: true,
	}
}
