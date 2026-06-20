package tools

import (
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 细粒度工具权限系统（Pattern-based）
//
// 细粒度工具权限系统：
//   - 支持 glob 通配符模式匹配工具名（如 "sandbox_*"）
//   - allow / deny / ask 三种决策
//   - 多条规则叠加，最后匹配的规则生效
//   - 可按 channel + chatType + user 维度限定
//
// 注意：策略评估使用工具的原始注册名（含 "sandbox_" 前缀）。
// 编排层（orchestrate.go）会在将工具列表发送给 LLM 之前剥离该前缀，
// 因此 LLM 看到的是 "exec"、"read_file" 等通用名称。
// Pattern 规则中的 "sandbox_*" 等模式在此处（LLM 之前）生效。
//
// 与现有 ToolPolicy（黑名单模式）的关系：
//   - PatternPolicy 是更强大的替代方案
//   - 两者可以共存：先 PatternPolicy 过滤，再 ToolPolicy 过滤
//   - 建议新项目使用 PatternPolicy
// ============================================================================

// PermissionDecision 是权限判定结果。
type PermissionDecision string

const (
	// PermAllow 允许使用。
	PermAllow PermissionDecision = "allow"
	// PermDeny 禁止使用。
	PermDeny PermissionDecision = "deny"
	// PermAsk 需要用户确认。
	PermAsk PermissionDecision = "ask"
)

// PatternRule 定义一条 pattern 匹配规则。
type PatternRule struct {
	// Pattern 工具名匹配模式（支持 * 通配符）。
	// 如 "sandbox_*" 匹配所有 sandbox_ 前缀工具。
	// "web_*" 匹配所有 web_ 前缀工具。
	// "*" 匹配所有工具。
	Pattern string `json:"pattern"`

	// Decision 匹配时的权限决策。
	Decision PermissionDecision `json:"decision"`

	// Channel 限定生效的渠道（空=所有渠道）。
	Channel string `json:"channel,omitempty"`

	// ChatType 限定生效的会话类型（空=所有类型）。
	ChatType string `json:"chatType,omitempty"`

	// Reason 决策原因（可选，用于审计/展示）。
	Reason string `json:"reason,omitempty"`
}

// PatternPolicy 是基于 pattern 匹配的权限策略。
type PatternPolicy struct {
	// Rules 规则列表（按顺序求值，最后匹配的规则生效）。
	Rules []PatternRule `json:"rules"`

	// DefaultDecision 没有规则匹配时的默认决策。
	// 默认为 PermAllow。
	DefaultDecision PermissionDecision `json:"defaultDecision,omitempty"`
}

// Evaluate 评估指定工具在给定上下文下的权限。
//
// 规则匹配逻辑：
//  1. 遍历所有规则，找到最后一条匹配 channel+chatType+pattern 的规则
//  2. 返回该规则的 Decision
//  3. 如果没有匹配的规则，返回 DefaultDecision
func (p PatternPolicy) Evaluate(toolName, channel, chatType string) PermissionDecision {
	var lastMatch *PatternRule

	for i := range p.Rules {
		rule := &p.Rules[i]
		if !rule.matchesContext(channel, chatType) {
			continue
		}
		if !matchPattern(rule.Pattern, toolName) {
			continue
		}
		// 后面的规则覆盖前面的
		lastMatch = rule
	}

	if lastMatch != nil {
		return lastMatch.Decision
	}

	if p.DefaultDecision != "" {
		return p.DefaultDecision
	}
	return PermAllow
}

// FilterTools 根据 pattern 策略过滤工具列表。
// 只有 Evaluate 返回 PermAllow 的工具保留。
// PermAsk 的工具保留（但运行时需要确认）。
// PermDeny 的工具被移除。
func (p PatternPolicy) FilterTools(toolList []llm.Tool, sctx *ToolSessionContext) []llm.Tool {
	result := make([]llm.Tool, 0, len(toolList))
	for _, t := range toolList {
		decision := p.Evaluate(t.Name, sctx.Channel, sctx.ChatType)
		if decision == PermAllow || decision == PermAsk {
			result = append(result, t)
		}
	}
	return result
}

// matchesContext 检查规则的上下文限定是否匹配。
func (r *PatternRule) matchesContext(channel, chatType string) bool {
	if r.Channel != "" && r.Channel != channel {
		return false
	}
	if r.ChatType != "" && r.ChatType != chatType {
		return false
	}
	return true
}

// matchPattern 检查工具名是否匹配 pattern。
// 支持 * 通配符：
//   - "sandbox_*" 匹配 "sandbox_exec"、"sandbox_read_file" 等
//   - "*_file" 匹配 "read_file"、"write_file" 等
//   - "*" 匹配所有
//   - "exact_name" 精确匹配
//   - "tool1|tool2" 匹配 tool1 或 tool2（管道分隔的 OR）
func matchPattern(pattern, name string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}

	// 支持 OR 语法：a|b|c
	if strings.Contains(pattern, "|") {
		parts := strings.Split(pattern, "|")
		for _, p := range parts {
			if matchSinglePattern(p, name) {
				return true
			}
		}
		return false
	}

	return matchSinglePattern(pattern, name)
}

// matchSinglePattern 匹配单个 pattern（不含 |）。
func matchSinglePattern(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "*" || pattern == "" {
		return true
	}

	// 精确匹配
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}

	// 处理前后缀通配
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return parts[0] == name
	}

	// 前缀匹配
	if parts[0] != "" && !strings.HasPrefix(name, parts[0]) {
		return false
	}

	// 后缀匹配
	lastPart := parts[len(parts)-1]
	if lastPart != "" && !strings.HasSuffix(name, lastPart) {
		return false
	}

	// 中间部分需要按序出现
	remaining := name
	for _, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(remaining, part)
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(part):]
	}

	return true
}

// ============================================================================
// PatternPolicy → ToolPolicyProvider 适配
// ============================================================================

// PatternPolicyStore 是存储 PatternPolicy 的接口。
type PatternPolicyStore interface {
	GetString(key, def string) string
}

// patternPolicyKey 构造存储键。
func patternPolicyKey(botID string) string {
	return "tools.pattern." + botID + ".policy"
}

// NewPatternPolicyProvider 从 PolicyStore 创建 PatternPolicy 提供者。
func NewPatternPolicyProvider(store PatternPolicyStore) ToolPolicyProvider {
	return ToolPolicyFunc(func(botID string) ToolPolicy {
		// PatternPolicy 使用独立的 FilterTools
		// 这里转回 ToolPolicy 接口（为兼容现有架构）
		if store == nil {
			return ToolPolicy{}
		}
		return ParseToolPolicy(store.GetString(patternPolicyKey(botID), ""))
	})
}

// PatternPolicyProvider 提供 PatternPolicy 的动态获取能力。
type PatternPolicyProvider interface {
	GetPatternPolicy(botID string) PatternPolicy
}

// PatternPolicyFunc 是函数适配器。
type PatternPolicyFunc2 func(botID string) PatternPolicy

func (f PatternPolicyFunc2) GetPatternPolicy(botID string) PatternPolicy {
	return f(botID)
}

// ============================================================================
// 预设策略工厂
// ============================================================================

// ReadOnlyPolicy 返回只读策略：只允许非破坏性工具。
func ReadOnlyPolicy() PatternPolicy {
	return PatternPolicy{
		Rules: []PatternRule{
			// 通用放行在前
			{Pattern: "*", Decision: PermAllow},
			{Pattern: "sandbox_*", Decision: PermAllow, Reason: "read-only operations"},
			{Pattern: "web_*", Decision: PermAllow},
			// 危险操作放后面（后匹配覆盖前面）
			{Pattern: "sandbox_exec", Decision: PermDeny, Reason: "read-only mode"},
			{Pattern: "sandbox_write_file", Decision: PermDeny, Reason: "read-only mode"},
			{Pattern: "sandbox_replace_in_file", Decision: PermDeny, Reason: "read-only mode"},
			{Pattern: "sandbox_delete_file", Decision: PermDeny, Reason: "read-only mode"},
			{Pattern: "sandbox_move_file", Decision: PermDeny, Reason: "read-only mode"},
		},
		DefaultDecision: PermAllow,
	}
}

// SafePolicy 返回安全策略：危险操作需要确认。
func SafePolicy() PatternPolicy {
	return PatternPolicy{
		Rules: []PatternRule{
			{Pattern: "*", Decision: PermAllow},
			{Pattern: "sandbox_*", Decision: PermAllow},
			// 危险操作需确认（后匹配覆盖前面）
			{Pattern: "sandbox_exec", Decision: PermAsk, Reason: "shell command execution"},
			{Pattern: "sandbox_delete_file", Decision: PermAsk, Reason: "file deletion"},
		},
		DefaultDecision: PermAllow,
	}
}

// SubagentPolicy 返回子代理策略：限制可用工具范围。
func SubagentPolicy() PatternPolicy {
	return PatternPolicy{
		Rules: []PatternRule{
			// 默认拒绝在前
			{Pattern: "*", Decision: PermDeny, Reason: "subagent: unknown tools disabled"},
			{Pattern: "sandbox_*", Decision: PermDeny, Reason: "subagent: destructive tools disabled"},
			// 允许的安全工具在后
			{Pattern: "now", Decision: PermAllow},
			{Pattern: "web_*", Decision: PermAllow},
			{Pattern: "calculate", Decision: PermAllow},
			{Pattern: "sandbox_read_file", Decision: PermAllow},
			{Pattern: "sandbox_list_dir", Decision: PermAllow},
			{Pattern: "sandbox_search_content", Decision: PermAllow},
		},
		DefaultDecision: PermDeny,
	}
}

// GroupChatPolicy 返回群聊策略：限制危险工具。
func GroupChatPolicy() PatternPolicy {
	return PatternPolicy{
		Rules: []PatternRule{
			{Pattern: "*", Decision: PermAllow},
			// 危险操作放后面
			{Pattern: "sandbox_exec", Decision: PermDeny, Reason: "group chat: shell execution disabled"},
			{Pattern: "sandbox_delete_file", Decision: PermDeny, Reason: "group chat: file deletion disabled"},
		},
		DefaultDecision: PermAllow,
	}
}
