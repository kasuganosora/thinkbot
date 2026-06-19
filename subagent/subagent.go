package subagent

import (
	"context"
	"fmt"
	"sync"

	"github.com/kasuganosora/thinkbot/llm"
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

// SubAgent 是一个上下文隔离的轻量 Agent。
// 它复用主 Agent 的 LLM Provider，但维护独立的对话历史。
type SubAgent struct {
	mu sync.Mutex

	// LLM 配置（从主 Agent 继承）
	provider  llm.Provider
	model     string
	system    string
	temp      float64
	maxTokens int

	// 上下文管理
	ctxMgr    *ContextManager
	totalTurns int
	closed     bool

	// 元数据
	id   string
	name string

	// 额外生成参数
	extraTools      []llm.Tool
	responseFormat  *llm.ResponseFormat
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

	params := sa.buildParams(msgs)

	result, err := sa.provider.DoGenerate(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("subagent %q: LLM generate failed: %w", sa.name, err)
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

	params := sa.buildParams(msgs)

	result, err := sa.provider.DoStream(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("subagent %q: LLM stream failed: %w", sa.name, err)
	}

	// 包装原始 channel，在流结束时更新上下文
	originalCh := result.Stream
	wrappedCh := make(chan llm.StreamPart, 64)

	go func() {
		defer close(wrappedCh)
		var textBuf string
		for part := range originalCh {
			wrappedCh <- part
			if tp, ok := part.(*llm.TextDeltaPart); ok {
				textBuf += tp.Text
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
	temp := sa.temp
	maxTokens := sa.maxTokens

	params := llm.GenerateParams{
		Model:      llm.ChatModel(sa.model),
		System:     sa.system,
		Messages:   msgs,
		Temperature: &temp,
		MaxTokens:  &maxTokens,
	}

	if len(sa.extraTools) > 0 {
		params.Tools = sa.extraTools
	}
	if sa.responseFormat != nil {
		params.ResponseFormat = sa.responseFormat
	}

	return params
}
