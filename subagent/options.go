package subagent

import (
	"fmt"
	"time"

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

// WithCallTimeout 设置本次 Delegate/DelegateMany 调用的超时时间（固定一刀切）。
// 覆盖 SubAgentManager 的默认 delegateTimeout（120s）。
// 设为 0 表示不覆盖（使用管理器默认值）。
// 注意：对 DelegateStream 无效——流式委托用卡死看门狗（WithStuckTimeout）替代固定超时。
func WithCallTimeout(d time.Duration) Option {
	return func(sa *SubAgent) {
		sa.callTimeout = d
	}
}

// WithStuckTimeout 设置 DelegateStream 卡死看门狗的阈值（连续无 token 输出的容忍时长）。
// 仅对 DelegateStream（流式委托）生效。设为 0 表示使用包默认 defaultDelegateStuckTimeout(180s)。
//
// 看门狗逻辑（与 sandbox 卡死看门狗一致）：
//   - 只要 LLM 持续输出 token（哪怕很慢）就不杀——正常处理超长 prompt（如 86 个 lint 问题）
//     不会因固定超时被迫中断；
//   - 只有连续 stuckTimeout 无任何 token（且已过首 token 宽限期）才判定「卡死」并终止；
//   - 硬上限 = stuckTimeout × delegateHardTimeoutFactor（派生，不写死），作为绝对兜底，
//     防止无限挂起（如模型以极小间隔吐 token 骗过卡死检测）。
func WithStuckTimeout(d time.Duration) Option {
	return func(sa *SubAgent) {
		sa.stuckTimeout = d
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
