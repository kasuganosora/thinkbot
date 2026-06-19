package subagent

import (
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
}

// NewContextManager 创建一个上下文管理器。
// maxMessages 为 0 表示无限制（不截断）。
func NewContextManager(maxMessages int) *ContextManager {
	return &ContextManager{
		maxMessages: maxMessages,
	}
}

// Append 追加一条消息到上下文尾部。
// 如果超过窗口限制，自动截断头部。
func (cm *ContextManager) Append(msg llm.Message) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = append(cm.messages, msg)
	cm.truncateLocked()
}

// AppendTurn 追加一轮对话（user + assistant）。
func (cm *ContextManager) AppendTurn(userText, assistantText string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = append(cm.messages,
		llm.UserMessage(userText),
		llm.AssistantMessage(assistantText),
	)
	cm.truncateLocked()
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
// 确保不丢失一轮完整对话（不会只保留半轮）。
func (cm *ContextManager) truncateLocked() {
	if cm.maxMessages <= 0 || len(cm.messages) <= cm.maxMessages {
		return
	}

	// 计算需要保留的消息数
	keep := cm.maxMessages

	// 确保从一条 user 消息开始（保持对话完整性）
	// 找到第一个 user 消息的位置
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

	cm.messages = cm.messages[start:]
}
