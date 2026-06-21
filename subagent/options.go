package subagent

import (
	"fmt"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Functional Options
// ============================================================================

// Option 配置 SubAgent 的可选参数。
type Option func(*SubAgent)

// WithSystemPrompt 设置系统提示词。
// 如果不调用此选项，默认使用空字符串（无系统提示词）。
func WithSystemPrompt(prompt string) Option {
	return func(sa *SubAgent) {
		sa.system = prompt
	}
}

// WithTemperature 设置 LLM 温度参数（0.0 ~ 2.0）。
func WithTemperature(temp float64) Option {
	return func(sa *SubAgent) {
		sa.temp = temp
	}
}

// WithMaxTokens 设置 LLM 最大输出 token 数。
func WithMaxTokens(tokens int) Option {
	return func(sa *SubAgent) {
		if tokens > 0 {
			sa.maxTokens = tokens
		}
	}
}

// WithMaxMessages 设置上下文滑动窗口大小（保留的最大消息条数）。
// 设为 0 表示无限制。
// 默认值：20（约 10 轮对话）。
func WithMaxMessages(max int) Option {
	return func(sa *SubAgent) {
		if max >= 0 {
			sa.ctxMgr = NewContextManager(max)
		}
	}
}

// WithID 设置 SubAgent 的唯一标识符。
func WithID(id string) Option {
	return func(sa *SubAgent) {
		sa.id = id
	}
}

// WithName 设置 SubAgent 的显示名称。
func WithName(name string) Option {
	return func(sa *SubAgent) {
		sa.name = name
	}
}

// WithTools 附加工具定义（LLM function calling）。
// 注意：SubAgent 本身不执行工具，仅将定义传递给 LLM。
func WithTools(tools ...llm.Tool) Option {
	return func(sa *SubAgent) {
		sa.extraTools = append(sa.extraTools, tools...)
	}
}

// WithResponseFormat 设置响应格式（如 JSON 模式）。
func WithResponseFormat(format *llm.ResponseFormat) Option {
	return func(sa *SubAgent) {
		sa.responseFormat = format
	}
}

// String 返回 SubAgent 的可读描述。
func (sa *SubAgent) String() string {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	name := sa.name
	if name == "" {
		name = sa.id
	}
	return fmt.Sprintf("SubAgent(%s, model=%s, turns=%d)", name, sa.model, sa.totalTurns)
}
