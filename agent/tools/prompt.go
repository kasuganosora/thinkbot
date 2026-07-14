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
		Content: "# 工具使用规则\n\n" +
			"## 强制调用（必须遵守）\n" +
			"- **涉及环境状态的问题必须先调工具验证再回答**。例如：\n" +
			"  - 是否安装了某软件 / 某命令是否存在 → 必须先执行 shell 命令（如 which、type、command -v）确认\n" +
			"  - 文件/目录是否存在、内容是什么 → 必须先调用 read_file / list_dir\n" +
			"  - 系统信息（OS 版本、内存、磁盘等） → 必须先执行对应查询命令\n" +
			"  - 网络连通性 / DNS 解析等 → 必须先执行实际探测\n" +
			"  - **绝对禁止凭知识或经验猜测环境状态**——sandbox 环境可能与你训练数据中的任何环境都不同\n" +
			"- 如果你需要判断某个事实才能回答用户问题，**优先调用工具获取真实数据**，而不是基于猜测给出答案\n\n" +
			"## 一般规则\n" +
			"- **你可以在一次回复中并行调用多个独立的工具**，大幅提高效率\n" +
			"- 对于纯知识性、创意性、分析性问题（不依赖当前环境状态），可以直接回答\n" +
			"- 工具调用失败时，向用户说明失败原因并尝试替代方案\n" +
			"- 不要编造工具结果，只使用实际返回的数据\n" +
			"- 对于需要审批的工具，等待用户确认后再执行\n" +
			"- 如果工具输出被截断（出现 truncated 标记），使用更精确的参数重新调用，不要重复相同的大范围查询",
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
