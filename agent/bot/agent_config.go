package bot

import (
	"fmt"
	"maps"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// AgentConfig — Per-Bot Agent 配置
//
// Agent 配置模型：
//   - 每个 Bot 可以独立配置模型、温度、步数限制等
//   - 支持工具白名单/黑名单
//   - 支持自定义 system prompt override
//
// 这是 BotConfig 的扩展层，不影响现有 BotConfig 的使用。
// ============================================================================

// AgentConfig 是 per-bot 的 Agent 级别配置。
// 与 BotConfig 互补：BotConfig 是基础设施配置，AgentConfig 是行为配置。
type AgentConfig struct {
	// MaxSteps 工具调用循环的最大步数。
	// 0 = 单步（不自动执行工具）
	// -1 = 无限制
	// >0 = 最多 N 步
	MaxSteps int `json:"maxSteps" yaml:"maxSteps"`

	// Temperature 采样温度。
	// 为 nil 时使用 BotConfig.Temperature。
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`

	// TopP nucleus 采样参数。
	TopP *float64 `json:"topP,omitempty" yaml:"topP,omitempty"`

	// ReasoningEffort 推理强度（如 "low"、"medium"、"high"）。
	// 仅对支持此参数的模型有效（如 o1 系列）。
	ReasoningEffort *string `json:"reasoningEffort,omitempty" yaml:"reasoningEffort,omitempty"`

	// StopSequences 停止序列。
	StopSequences []string `json:"stopSequences,omitempty" yaml:"stopSequences,omitempty"`

	// ToolAllowlist 工具白名单。
	// 非空时只允许列表中的工具。
	ToolAllowlist []string `json:"toolAllowlist,omitempty" yaml:"toolAllowlist,omitempty"`

	// ToolBlocklist 工具黑名单。
	// 列表中的工具被禁用（优先级高于白名单）。
	ToolBlocklist []string `json:"toolBlocklist,omitempty" yaml:"toolBlocklist,omitempty"`

	// SystemPromptOverride 覆盖默认 system prompt。
	// 非空时替代 BotConfig.SystemPrompt。
	SystemPromptOverride string `json:"systemPromptOverride,omitempty" yaml:"systemPromptOverride,omitempty"`

	// CompactionEnabled 是否启用上下文压缩。
	CompactionEnabled *bool `json:"compactionEnabled,omitempty" yaml:"compactionEnabled,omitempty"`

	// MaxContextTokens 上下文窗口大小（token）。
	// 用于上下文压缩的阈值判断。
	// 为 0 时使用模型默认值。
	MaxContextTokens int `json:"maxContextTokens,omitempty" yaml:"maxContextTokens,omitempty"`

	// Extra 扩展配置。
	Extra map[string]any `json:"extra,omitempty" yaml:"extra,omitempty"`
}

// DefaultAgentConfig 返回默认 Agent 配置。
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		MaxSteps: 10,
	}
}

// Merge 将 other 的非零值合并到当前配置。
func (c AgentConfig) Merge(other AgentConfig) AgentConfig {
	result := c

	if other.MaxSteps != 0 {
		result.MaxSteps = other.MaxSteps
	}
	if other.Temperature != nil {
		result.Temperature = other.Temperature
	}
	if other.TopP != nil {
		result.TopP = other.TopP
	}
	if other.ReasoningEffort != nil {
		result.ReasoningEffort = other.ReasoningEffort
	}
	if len(other.StopSequences) > 0 {
		result.StopSequences = other.StopSequences
	}
	if len(other.ToolAllowlist) > 0 {
		result.ToolAllowlist = other.ToolAllowlist
	}
	if len(other.ToolBlocklist) > 0 {
		result.ToolBlocklist = other.ToolBlocklist
	}
	if other.SystemPromptOverride != "" {
		result.SystemPromptOverride = other.SystemPromptOverride
	}
	if other.CompactionEnabled != nil {
		result.CompactionEnabled = other.CompactionEnabled
	}
	if other.MaxContextTokens > 0 {
		result.MaxContextTokens = other.MaxContextTokens
	}
	if other.Extra != nil {
		if result.Extra == nil {
			result.Extra = make(map[string]any)
		}
		maps.Copy(result.Extra, other.Extra)
	}
	return result
}

// EffectiveTemperature 返回有效温度。
// 优先使用 AgentConfig 的值，回退到 BotConfig 的值。
func (c AgentConfig) EffectiveTemperature(botCfg BotConfig) float64 {
	if c.Temperature != nil {
		return *c.Temperature
	}
	return botCfg.Temperature
}

// EffectiveSystemPrompt 返回有效 system prompt。
func (c AgentConfig) EffectiveSystemPrompt(botCfg BotConfig) string {
	if c.SystemPromptOverride != "" {
		return c.SystemPromptOverride
	}
	return botCfg.SystemPrompt
}

// FilterTools 按白名单/黑名单过滤工具列表。
func (c AgentConfig) FilterTools(tools []llm.Tool) []llm.Tool {
	if len(c.ToolAllowlist) == 0 && len(c.ToolBlocklist) == 0 {
		return tools
	}

	result := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		// 黑名单优先
		if contains(c.ToolBlocklist, t.Name) {
			continue
		}
		// 白名单非空时检查
		if len(c.ToolAllowlist) > 0 && !contains(c.ToolAllowlist, t.Name) {
			continue
		}
		result = append(result, t)
	}
	return result
}

// ToGenerateOptions 将 AgentConfig 转换为 GenerateParams 的覆盖值。
// 返回的闭包接收一个 GenerateParams 并返回修改后的版本。
func (c AgentConfig) ApplyToParams(params *llm.GenerateParams) {
	if c.Temperature != nil {
		params.Temperature = c.Temperature
	}
	if c.TopP != nil {
		params.TopP = c.TopP
	}
	if c.ReasoningEffort != nil {
		params.ReasoningEffort = c.ReasoningEffort
	}
	if len(c.StopSequences) > 0 {
		params.StopSequences = c.StopSequences
	}
}

// String 返回可读表示。
func (c AgentConfig) String() string {
	return fmt.Sprintf("AgentConfig{maxSteps=%d, allowlist=%v, blocklist=%v}",
		c.MaxSteps, c.ToolAllowlist, c.ToolBlocklist)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
