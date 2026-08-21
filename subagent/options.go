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

// WithTools 注入带 Execute 处理函数的工具（LLM function calling）。
// 当同时设置 WithToolSteps（>0）时，SubAgent 会走多步编排回路自动执行这些工具，
// 使其能像主 Agent 一样使用工作空间（exec/读/写/列目录等）。
// 工具通常由 SubAgentManager 从主 Agent 的 ToolManager 解析后注入。
func WithTools(tools ...llm.Tool) Option {
	return func(sa *SubAgent) {
		sa.extraTools = append(sa.extraTools, tools...)
	}
}

// WithToolSteps 设置带工具执行回路时的最大 LLM 步数预算（仅当通过 WithTools
// 注入了可执行工具时生效）。0 = 使用包默认 defaultSubagentToolSteps。
func WithToolSteps(n int) Option {
	return func(sa *SubAgent) {
		if n > 0 {
			sa.toolSteps = n
		}
	}
}

// WithResponseFormat 设置响应格式（如 JSON 模式）。
func WithResponseFormat(format *llm.ResponseFormat) Option {
	return func(sa *SubAgent) {
		sa.responseFormat = format
	}
}

// WithCallTimeout 设置 DelegateMany 调用的超时时间（固定一刀切），覆盖
// SubAgentManager 的默认 delegateTimeout（120s）。设为 0 表示不覆盖（用管理器默认）。
// 注意：仅对 DelegateMany 生效——Delegate 已收敛为 DelegateStream 的薄封装，
// 受卡死看门狗保护（WithStuckTimeout），不再受此一刀切超时约束。
func WithCallTimeout(d time.Duration) Option {
	return func(sa *SubAgent) {
		sa.callTimeout = d
	}
}

// WithChatTimeout 设置持久 subagent（Spawn/Chat）单次交互的墙钟硬上限。
// 覆盖默认的 defaultChatHardTimeout（10min）。防止工具挂死/LLM 假活时 Chat
// 无限阻塞调用方 goroutine。仅对持久 subagent 的 Chat 生效；
// Delegate/DelegateStream/DelegateMany 走各自的卡死看门狗，不受此影响。
func WithChatTimeout(d time.Duration) Option {
	return func(sa *SubAgent) {
		if d > 0 {
			sa.chatTimeout = d
		}
	}
}

// WithStuckTimeout 设置 DelegateStream 卡死看门狗的阈值（连续无 token 输出的容忍时长）。
// 仅对 DelegateStream（流式委托）生效。设为 0 表示使用包默认 defaultDelegateStuckTimeout(180s)。
//
// 看门狗逻辑（与 sandbox 卡死看门狗一致）：
//   - 只要 LLM 持续输出 token（哪怕很慢）就不杀——正常处理超长 prompt（如 86 个 lint 问题）
//     不会因固定超时被迫中断；
//   - 只有连续 stuckTimeout 无任何 token（且已过首 token 宽限期）才判定「卡死」并终止；
//   - 硬上限 = stuckTimeout × delegateHardTimeoutFactor（派生，不写死）是「总运行时间」的
//     绝对兜底上限（墙钟），只拦「永远在吐 token 但永不结束」的失控流；正常持续吐 token
//     的 agent 不会被 hard 杀掉（倍数取较大值，避免误杀慢任务，见 delegateHardTimeoutFactor）。
func WithStuckTimeout(d time.Duration) Option {
	return func(sa *SubAgent) {
		sa.stuckTimeout = d
	}
}

// WithSkipTools 跳过 SubAgentManager 的工具注入。
//
// 正常情况下 SubAgentManager.SetToolResolver 后，所有 Delegate/DelegateStream/
// DelegateMany 调用都会自动注入主 Agent 在子 Agent 场景可用的工具（exec/读/写等），
// 使子 Agent 能操作工作空间。但某些场景是纯 LLM 任务（如工作流需求分析器 Analyzer），
// 只需模型输出结构化 JSON，不需要任何工具能力；被注入工具后会误走
// OrchestrateStream 多步编排循环，导致不必要的延迟或卡死。
//
// 传入此选项后，DelegateStream 即使在 SetToolResolver 生效的情况下也不会注入工具，
// 等效于走无工具的简单 LLM 流式调用。
func WithSkipTools() Option {
	return func(sa *SubAgent) {
		sa.skipTools = true
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
