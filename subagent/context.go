package subagent

import (
	"context"
	"sync"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ContextManager — 对话上下文管理器
//
// 维护一个线程安全的消息列表，支持滑动窗口截断。
// 当消息数超过 maxMessages 时，自动丢弃最早的消息（FIFO）。
// ============================================================================

// ContextManager 管理 SubAgent 的对话历史。
type ContextManager struct {
	mu          sync.Mutex
	messages    []llm.Message
	maxMessages int // 0 = 无限制

	// summarizeHead 可选：当消息数超过 maxMessages 时，用它把最早的一批消息
	// （head）压缩成单条摘要消息替代删除，避免纯删除导致早期上下文永久丢失
	// （持久 subagent 多轮对话场景）。返回 (摘要消息, true) 表示已压缩；
	// 否则回退纯删除。由 SubAgent 在持有 compactor+provider 时注入。
	// head 可能很大（一次溢出整批），调用方需自行保证不在此函数内持有关键锁。
	summarizeHead func(ctx context.Context, head []llm.Message) (llm.Message, bool)
}

// NewContextManager 创建一个上下文管理器。
// maxMessages 为 0 表示无限制（不截断）。
func NewContextManager(maxMessages int) *ContextManager {
	return &ContextManager{
		maxMessages: maxMessages,
	}
}

// Append 追加一条消息到上下文尾部（无 ctx 变体：截断时只能纯删除，无法做 LLM 摘要）。
// 用于预填充（SeedMessages）等尚无可用 ctx 的场景；正常对话持久化请改用 AppendWithCtx。
// 如果超过窗口限制，自动截断头部。
func (cm *ContextManager) Append(msg llm.Message) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = append(cm.messages, msg)
	cm.truncateLocked(context.Background())
}

// AppendWithCtx 与 Append 等价，但携带 ctx 以便在溢出时触发 LLM 摘要压缩（summarizeHead）。
// 正常对话持久化（ChatWithResult/Stream）使用本方法，使早期上下文以摘要形式保留而非被删除。
func (cm *ContextManager) AppendWithCtx(ctx context.Context, msg llm.Message) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = append(cm.messages, msg)
	cm.truncateLocked(ctx)
}

// AppendTurn 追加一轮对话（user + assistant）。无 ctx 变体，见 Append 说明。
func (cm *ContextManager) AppendTurn(userText, assistantText string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = append(cm.messages,
		llm.UserMessage(userText),
		llm.AssistantMessage(assistantText),
	)
	cm.truncateLocked(context.Background())
}

// AppendTurnWithCtx 与 AppendTurn 等价，但携带 ctx 以便溢出时触发 LLM 摘要压缩。
func (cm *ContextManager) AppendTurnWithCtx(ctx context.Context, userText, assistantText string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = append(cm.messages,
		llm.UserMessage(userText),
		llm.AssistantMessage(assistantText),
	)
	cm.truncateLocked(ctx)
}

// Messages 返回当前上下文消息的切片（直接引用，调用方不应修改）。
func (cm *ContextManager) Messages() []llm.Message {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.messages
}

// Clear 清空所有消息。
func (cm *ContextManager) Clear() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = nil
}

// Len 返回当前消息数。
func (cm *ContextManager) Len() int {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return len(cm.messages)
}

// truncateLocked 在已持锁的状态下执行滑动窗口截断。
// 优先「压缩」：若配置了 summarizeHead（compactor+provider），则把溢出的最早整批
// 消息压缩为单条摘要消息替代删除，保留早期上下文；压缩不可行时回退纯删除。
// 确保不丢失一轮完整对话（不会只保留半轮）。
//
// ctx 用于驱动 summarizeHead 内的 LLM 摘要调用；调用方须已持有 cm.mu。
func (cm *ContextManager) truncateLocked(ctx context.Context) {
	if cm.maxMessages <= 0 || len(cm.messages) <= cm.maxMessages {
		return
	}

	// 计算需要保留的消息数（保持从一条 user 消息开始的对话完整性）。
	keep := cm.maxMessages
	start := len(cm.messages) - keep
	for start < len(cm.messages) {
		if cm.messages[start].Role == llm.MessageRoleUser {
			break
		}
		start++
	}
	if start >= len(cm.messages) {
		// 没找到 user 消息，保留最后 keep 条
		start = len(cm.messages) - keep
	}

	head := cm.messages[:start]
	if cm.summarizeHead != nil && len(head) > 0 {
		if summaryMsg, ok := cm.summarizeHead(ctx, head); ok {
			newMsgs := make([]llm.Message, 0, 1+len(cm.messages)-start)
			newMsgs = append(newMsgs, summaryMsg)
			newMsgs = append(newMsgs, cm.messages[start:]...)
			// 摘要后消息数未减少（head 太短）则回退纯删除，避免无限递归。
			if len(newMsgs) < len(cm.messages) {
				cm.messages = newMsgs
				if len(cm.messages) > cm.maxMessages {
					cm.truncateLocked(ctx) // 仍超上限则继续压缩，直到收敛
				}
				return
			}
		}
	}

	// 回退：纯删除最早整批（含对齐 user 起始）。
	cm.messages = append([]llm.Message(nil), cm.messages[start:]...)
}
