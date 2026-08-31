package tools

import (
	"fmt"
	"strings"
	"sync"

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

	mu         sync.Mutex
	registered []string // 已注册的段落名称（用于清理或重新注册）
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range m.registered {
		m.registry.Unregister(name)
	}
	m.registered = nil
}

// RegisteredNames 返回已注册的段落名称列表。
func (m *ToolPromptManager) RegisteredNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	sb.WriteString("# Available Tools\n\n")
	sb.WriteString("You have access to the tools below. Call them proactively whenever a task needs real data or a real action:\n\n")
	for _, name := range toolNames {
		fmt.Fprintf(&sb, "- `%s`\n", name)
	}
	sb.WriteString("\nWhen calling a tool, make sure the arguments conform to its JSON schema. Tool results are returned to you automatically for further reasoning.")

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
		Content: "# Tool Usage Policy\n\n" +
			"## Mandatory Verification (non-negotiable)\n" +
			"- **ALWAYS verify with a tool before answering any question about environment state.** For example:\n" +
			"  - Whether a program is installed / whether a command exists → run a shell command first (`which`, `type`, `command -v`)\n" +
			"  - Whether a file or directory exists and what it contains → call `read_file` / `list_dir` first\n" +
			"  - System information (OS version, memory, disk, ...) → run the corresponding query command first\n" +
			"  - Network reachability / DNS resolution → run an actual probe first\n" +
			"  - **NEVER guess environment state from prior knowledge or experience** — the sandbox may differ from any environment in your training data\n" +
			"- IMPORTANT: If you must establish a fact in order to answer the user, **call a tool to get real data** instead of answering from assumption\n\n" +
			"## General Rules\n" +
			"- **You can call multiple independent tools in a single response** — batch them for a large efficiency win\n" +
			"- For purely knowledge-based, creative, or analytical questions that do not depend on current environment state, answer directly\n" +
			"- When a tool call fails, tell the user why it failed and try an alternative approach\n" +
			"- NEVER fabricate tool results. Use only the data actually returned\n" +
			"- For tools that require approval, wait for the user's confirmation before executing\n" +
			"- If tool output is truncated (a `truncated` marker appears), re-run with narrower parameters. Do NOT repeat the same broad query",
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
	fmt.Fprintf(&sb, "## Tool: %s\n\n", def.Name)
	if def.Description != "" {
		sb.WriteString(def.Description)
		sb.WriteString("\n\n")
	}

	if len(def.Scopes) > 0 {
		sb.WriteString("Applicable scopes: ")
		sb.WriteString(strings.Join(def.Scopes, ", "))
		sb.WriteString("\n\n")
	}

	if def.RequireApproval {
		sb.WriteString("⚠️ IMPORTANT: This tool requires explicit user confirmation before it can be executed.\n\n")
	}

	return &ToolPromptSection{
		Name:    def.Name + "_desc",
		Order:   310, // 具体工具描述在 header/rules 之后
		Content: sb.String(),
		Enabled: true,
	}
}
