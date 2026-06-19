package tools

import (
	"encoding/json"
	"slices"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ToolPolicy — 工具黑名单权限策略
//
// 设计理念：
//   - 默认全部工具可用（黑名单模式）
//   - 每个 bot 可以配置多条规则（ToolRule）
//   - 规则可以按 channel + chatType 场景限定
//   - 被禁用的工具可以针对特定用户开放（用户白名单）
//
// 规则匹配逻辑：
//   1. 遍历所有规则，找到匹配当前 channel + chatType 的规则
//   2. 检查这些规则是否禁用了目标工具
//   3. 如果被禁用，检查用户是否在任一匹配规则的白名单中
//   4. 用户在白名单中 → 放行；否则 → 过滤掉
// ============================================================================

// ToolRule 定义一条工具黑名单规则。
type ToolRule struct {
	// Channel 限定此规则生效的渠道（如 "misskey", "telegram"）。
	// 空字符串表示对所有渠道生效。
	Channel string `json:"channel,omitempty"`

	// ChatType 限定此规则生效的会话类型（"group", "private"）。
	// 空字符串表示对所有类型生效。
	ChatType string `json:"chatType,omitempty"`

	// Disabled 被此规则禁用的工具名称列表。
	Disabled []string `json:"disabled,omitempty"`

	// AllowedUsers 可以绕过此规则禁用工具的用户 ID 列表。
	// 这些用户即使在该规则匹配的场景下仍可使用被禁用的工具。
	AllowedUsers []string `json:"allowedUsers,omitempty"`
}

// matches 检查规则是否匹配给定的渠道和会话类型。
// 空字段表示通配（匹配任意值）。
func (r ToolRule) matches(channel, chatType string) bool {
	if r.Channel != "" && r.Channel != channel {
		return false
	}
	if r.ChatType != "" && r.ChatType != chatType {
		return false
	}
	return true
}

// ToolPolicy 是一个 bot 的完整工具权限策略。
type ToolPolicy struct {
	// Rules 黑名单规则列表（按顺序求值）。
	Rules []ToolRule `json:"rules,omitempty"`
}

// IsAllowed 检查指定工具在给定上下文下是否对指定用户可用。
//
// 判定流程：
//  1. 找到所有匹配 channel+chatType 的规则
//  2. 如果没有任何匹配规则禁用了该工具 → 允许
//  3. 如果有匹配规则禁用了该工具，检查用户是否在任一匹配规则的白名单中
//  4. 用户在白名单中 → 允许；否则 → 拒绝
func (p ToolPolicy) IsAllowed(toolName, channel, chatType, userID string) bool {
	if len(p.Rules) == 0 {
		return true
	}

	disabled := false
	for _, rule := range p.Rules {
		if !rule.matches(channel, chatType) {
			continue
		}
		if !sliceContains(rule.Disabled, toolName) {
			continue
		}
		// 此规则禁用了该工具
		disabled = true
		// 检查用户是否在白名单中
		if sliceContains(rule.AllowedUsers, userID) {
			return true
		}
	}

	return !disabled
}

// FilterTools 根据策略过滤工具列表。
// 返回过滤后的工具（仅保留对当前用户可用的工具）。
func (p ToolPolicy) FilterTools(toolList []llm.Tool, sctx *ToolSessionContext) []llm.Tool {
	if len(p.Rules) == 0 {
		return toolList
	}
	result := make([]llm.Tool, 0, len(toolList))
	for _, t := range toolList {
		if p.IsAllowed(t.Name, sctx.Channel, sctx.ChatType, sctx.UserID) {
			result = append(result, t)
		}
	}
	return result
}

// ToolPolicyJSON 将 ToolPolicy 序列化为 JSON 字符串。
func ToolPolicyJSON(p ToolPolicy) (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ParseToolPolicy 从 JSON 字符串解析 ToolPolicy。
// 空字符串或无效 JSON 返回空策略（全部放行）。
func ParseToolPolicy(jsonStr string) ToolPolicy {
	if jsonStr == "" {
		return ToolPolicy{}
	}
	var p ToolPolicy
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return ToolPolicy{}
	}
	return p
}

// ============================================================================
// ToolPolicyProvider — 策略提供者接口（运行时动态获取策略）
// ============================================================================

// ToolPolicyProvider 动态提供工具权限策略。
// 实现者通常从 config.Store 实时读取策略，支持运行时修改生效。
type ToolPolicyProvider interface {
	// GetPolicy 返回指定 botID 的工具权限策略。
	GetPolicy(botID string) ToolPolicy
}

// ToolPolicyFunc 是 ToolPolicyProvider 的函数适配器。
type ToolPolicyFunc func(botID string) ToolPolicy

func (f ToolPolicyFunc) GetPolicy(botID string) ToolPolicy {
	return f(botID)
}

// ============================================================================
// 辅助函数
// ============================================================================

func sliceContains(slice []string, item string) bool {
	return slices.Contains(slice, item)
}
