package subagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// SubAgent — 轻量级隔离 Agent
//
// SubAgent 继承主 Agent 的 LLM Provider 和模型设置，
// 但维护完全独立的对话上下文。
//
// 核心特性：
//   - 上下文隔离：每个 SubAgent 的对话历史互不影响
//   - 无记忆：不持久化任何数据，Close 后上下文丢弃
//   - 滑动窗口：自动管理上下文长度，防止 token 爆炸
//   - 只能被主 Agent 程序调用，不监听任何 Channel
//
// 典型用法：
//
//	bundle := bot.CreateLLMBundle(...)  // 主 Agent 的 LLM
//	sub := subagent.New(bundle.Main, "glm-5.2",
//	    subagent.WithSystemPrompt("你是一个代码审查专家"),
//	    subagent.WithMaxTurns(5),
//	)
//	defer sub.Close()
//
//	reply, err := sub.Chat(ctx, "审查这段代码: ...")
// ============================================================================

// defaultSubagentToolSteps 是子 Agent 带工具执行回路时的默认最大 LLM 步数预算。
// 仅当通过 WithTools 注入了可执行工具、且 toolSteps != 0 时启用多步编排回路。
const defaultSubagentToolSteps = 20

// SubAgent 是一个上下文隔离的轻量 Agent。
// 它复用主 Agent 的 LLM Provider，但维护独立的对话历史。
//
// 工具能力：若通过 WithTools 注入了带 Execute 处理函数的工具（通常由
// SubAgentManager 从主 Agent 的 ToolManager 解析后注入），且 toolSteps != 0，
// 则 Chat/Stream 会走 llm.OrchestrateGenerate/Stream 多步回路——自动执行工具、
// 将结果喂回模型，直到模型停止请求工具。子 Agent 因此能像主 Agent 一样使用
// 工作空间（exec/读/写/列目录等），只是不能 spawn 子 Agent（由 spawn 工具的
// scope 排除，防止套娃）。
type SubAgent struct {
	mu sync.Mutex

	// LLM 配置（从主 Agent 继承）
	provider  llm.Provider
	model     string
	system    string
	temp      float64
	maxTokens int

	// 上下文管理
	ctxMgr     *ContextManager
	totalTurns int

	// callTimeout 本次调用的超时覆盖（0 = 使用 SubAgentManager 的默认 delegateTimeout）。
	// 由 WithCallTimeout 设置，仅对 Delegate/DelegateMany 的单次调用生效。
	callTimeout time.Duration

	// stuckTimeout 卡死看门狗阈值（0 = 使用默认 defaultDelegateStuckTimeout）。
	// 由 WithStuckTimeout 设置，仅对 DelegateStream（流式委托）生效。
	stuckTimeout time.Duration

	closed bool

	// 元数据
	id   string
	name string

	// 额外生成参数
	extraTools     []llm.Tool
	responseFormat *llm.ResponseFormat

	// toolSteps 是带工具执行回路时的最大 LLM 步数预算（>0 启用 OrchestrateGenerate/Stream）。
	// 0 = 使用包默认 defaultSubagentToolSteps。仅当 extraTools 非空时生效。
	toolSteps int

	// skipTools 为 true 时，SubAgentManager.ResolveTools 即使解析到了工具也不注入。
	// 用于 Analyzer 等纯 LLM 任务（只需输出 JSON，不需要工具能力），
	// 避免被注入工具后误走 OrchestrateStream 多步编排循环导致卡死或延迟。
	skipTools bool
}

// New 创建一个 SubAgent。
//
// provider 和 model 通常来自主 Agent 的 LLMBundle（如 bundle.Main, bundle.MainDef.Model）。
// 可通过 opts 自定义系统提示词、温度、滑动窗口等参数。
func New(provider llm.Provider, model string, opts ...Option) *SubAgent {
	sa := &SubAgent{
		provider:  provider,
		model:     model,
		temp:      0.7, // 默认与 BotConfig 一致
		maxTokens: 4096,
		ctxMgr:    NewContextManager(20), // 默认保留最近 20 条消息（10 轮）
	}
	for _, opt := range opts {
		opt(sa)
	}
	return sa
}

// Chat 发送一条消息并返回回复文本，同时更新内部上下文。
// 返回的回复会自动追加到上下文中。
func (sa *SubAgent) Chat(ctx context.Context, text string) (string, error) {
	result, err := sa.ChatWithResult(ctx, text)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// ChatWithResult 发送消息并返回完整的 GenerateResult。
// 对话历史会自动更新。
func (sa *SubAgent) ChatWithResult(ctx context.Context, text string) (*llm.GenerateResult, error) {
	sa.mu.Lock()
	if sa.closed {
		sa.mu.Unlock()
		return nil, fmt.Errorf("subagent %q: already closed", sa.name)
	}

	// 构建消息序列：历史 + 当前用户消息
	history := sa.ctxMgr.Messages()
	sa.mu.Unlock()

	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.UserMessage(text))

	// 带可执行工具时走多步编排回路：自动执行工具并把结果喂回模型，
	// 直到模型停止请求工具。子 Agent 因此能像主 Agent 一样使用工作空间。
	if len(sa.extraTools) > 0 {
		params := sa.buildParams(msgs)
		steps := sa.toolSteps
		if steps <= 0 {
			steps = defaultSubagentToolSteps
		}
		cfg := &llm.OrchestrateConfig{
			Params:       params,
			MaxSteps:     steps,
			HardMaxSteps: 0, // 0 = 自动 = MaxSteps * 3（看门狗兜底）
			// 延迟加载：子 Agent 与主 Agent 共用同一批工具（含可能延迟的
			// MCP 工具），按需经 tool_search / 直接引用加载完整 schema。
			ToolDeferral: llm.NewToolDeferral(true),
		}
		result, err := llm.OrchestrateGenerate(ctx, sa.provider, cfg)
		if err != nil {
			return nil, errs.Wrapf(err, "subagent %q: orchestrated generate failed", sa.name)
		}
		// 持久化完整输出消息（含工具调用/结果），保证多轮上下文连贯。
		// 注意：result.Messages 仅含本轮的 assistant/tool 消息，不含本轮 user 消息，
		// 必须先把 user 消息写回上下文，否则下一轮会丢失 user 导致对话错乱。
		sa.mu.Lock()
		if !sa.closed {
			sa.ctxMgr.Append(llm.UserMessage(text))
			for _, m := range result.Messages {
				sa.ctxMgr.Append(m)
			}
			sa.totalTurns++
		}
		sa.mu.Unlock()
		return result, nil
	}

	params := sa.buildParams(msgs)

	result, err := sa.provider.DoGenerate(llm.WithStatsFeature(ctx, "subagent"), params)
	if err != nil {
		return nil, errs.Wrapf(err, "subagent %q: LLM generate failed", sa.name)
	}

	// 更新上下文
	sa.mu.Lock()
	if !sa.closed {
		sa.ctxMgr.AppendTurn(text, result.Text)
		sa.totalTurns++
	}
	sa.mu.Unlock()

	return result, nil
}

// Stream 发送消息并以流式方式返回结果。
// 对话历史在流完成（调用 StreamResult.Text() 或 ToResult()）后更新。
func (sa *SubAgent) Stream(ctx context.Context, text string) (*llm.StreamResult, error) {
	sa.mu.Lock()
	if sa.closed {
		sa.mu.Unlock()
		return nil, fmt.Errorf("subagent %q: already closed", sa.name)
	}
	history := sa.ctxMgr.Messages()
	sa.mu.Unlock()

	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.UserMessage(text))

	// 带可执行工具时走多步编排流式回路（自动执行工具）。
	if len(sa.extraTools) > 0 {
		params := sa.buildParams(msgs)
		steps := sa.toolSteps
		if steps <= 0 {
			steps = defaultSubagentToolSteps
		}
		cfg := &llm.OrchestrateConfig{
			Params:       params,
			MaxSteps:     steps,
			HardMaxSteps: 0, // 0 = 自动 = MaxSteps * 3
			// 延迟加载：子 Agent 与主 Agent 共用同一批工具（含可能延迟的
			// MCP 工具），按需经 tool_search / 直接引用加载完整 schema。
			// 与 ChatWithResult 路径保持一致，避免流式下延迟工具暴露完整 schema。
			ToolDeferral: llm.NewToolDeferral(true),
		}
		result, err := llm.OrchestrateStream(ctx, sa.provider, cfg)
		if err != nil {
			return nil, errs.Wrapf(err, "subagent %q: orchestrated stream failed", sa.name)
		}
		// 流结束后把本轮完整消息持久化到上下文：先写回 user 消息，
		// 再写入编排结果（含工具调用/结果等中间消息），保证多轮上下文连贯。
		// OrchestrateStream 在关闭 channel 前已填充 result.Messages，此处可安全读取。
		originalCh := result.Stream
		wrappedCh := make(chan llm.StreamPart, 256)
		go func() {
			defer close(wrappedCh)
			for part := range originalCh {
				select {
				case wrappedCh <- part:
				case <-ctx.Done():
					return
				}
			}
			sa.mu.Lock()
			if !sa.closed {
				sa.ctxMgr.Append(llm.UserMessage(text))
				for _, m := range result.Messages {
					sa.ctxMgr.Append(m)
				}
				sa.totalTurns++
			}
			sa.mu.Unlock()
		}()
		result.Stream = wrappedCh
		return result, nil
	}

	params := sa.buildParams(msgs)

	result, err := sa.provider.DoStream(llm.WithStatsFeature(ctx, "subagent"), params)
	if err != nil {
		return nil, errs.Wrapf(err, "subagent %q: LLM stream failed", sa.name)
	}

	// 包装原始 channel，在流结束时更新上下文
	originalCh := result.Stream
	wrappedCh := make(chan llm.StreamPart, 64)

	go func() {
		defer close(wrappedCh)
		var textBuf string
		for part := range originalCh {
			select {
			case wrappedCh <- part:
				if tp, ok := part.(*llm.TextDeltaPart); ok {
					textBuf += tp.Text
				}
			case <-ctx.Done():
				return // context 取消，退出避免 goroutine 泄漏
			}
		}
		// 流结束后更新上下文
		if textBuf != "" {
			sa.mu.Lock()
			if !sa.closed {
				sa.ctxMgr.AppendTurn(text, textBuf)
				sa.totalTurns++
			}
			sa.mu.Unlock()
		}
	}()

	result.Stream = wrappedCh
	return result, nil
}

// Clear 重置对话上下文（保留系统提示词和配置，只清除历史消息）。
func (sa *SubAgent) Clear() {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	sa.ctxMgr.Clear()
	sa.totalTurns = 0
}

// History 返回当前上下文消息的副本。
func (sa *SubAgent) History() []llm.Message {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	msgs := sa.ctxMgr.Messages()
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	return out
}

// TurnCount 返回总对话轮数（不受滑动窗口影响）。
func (sa *SubAgent) TurnCount() int {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	return sa.totalTurns
}

// ID 返回 SubAgent 的标识符。
func (sa *SubAgent) ID() string {
	return sa.id
}

// Name 返回 SubAgent 的名称。
func (sa *SubAgent) Name() string {
	return sa.name
}

// SetSystem 动态修改系统提示词（影响后续所有调用）。
func (sa *SubAgent) SetSystem(prompt string) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	sa.system = prompt
}

// SeedMessages 用给定消息预填充上下文（在首次 Chat 之前调用）。
// 适用于从外部导入已有对话的场景。
func (sa *SubAgent) SeedMessages(msgs []llm.Message) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	for _, m := range msgs {
		sa.ctxMgr.Append(m)
	}
}

// Close 关闭 SubAgent，释放上下文。
// Close 后调用 Chat 会返回错误。
// 可以安全地多次调用。
func (sa *SubAgent) Close() {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	sa.closed = true
	sa.ctxMgr = nil
}

// buildParams 根据当前配置和消息构建 GenerateParams。
func (sa *SubAgent) buildParams(msgs []llm.Message) llm.GenerateParams {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	temp := sa.temp
	maxTokens := sa.maxTokens

	params := llm.GenerateParams{
		Model:       llm.ChatModel(sa.model),
		System:      sa.system,
		Messages:    msgs,
		Temperature: &temp,
		MaxTokens:   &maxTokens,
	}

	if len(sa.extraTools) > 0 {
		params.Tools = sa.extraTools
	}
	if sa.responseFormat != nil {
		params.ResponseFormat = sa.responseFormat
	}

	return params
}
