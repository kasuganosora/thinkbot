// Package session 提供对话会话管理能力。
//
// Session 代表一个连续的对话上下文——Bot 与用户在同一个话题/对话链上的交互序列。
// 它填补了单条消息处理（Pipeline/Envelope）和长期记忆（Memory）之间的层次空白。
//
// 架构层次：
//
//	┌─────────────────────────────────┐
//	│  Envelope (单条消息)              │  ← 最短命
//	├─────────────────────────────────┤
//	│  Session (当前对话上下文)          │  ← 中等寿命：一个话题/对话链
//	├─────────────────────────────────┤
//	│  Memory (长期记忆 L0~L3)          │  ← 长期：跨对话
//	└─────────────────────────────────┘
//
// Session 的生命周期：
//  1. SessionResolver.Resolve(msg) 判断消息是否属于/应创建 session
//  2. SessionManager.GetOrCreate(id) 取得或创建 session
//  3. 消息追加到 session 的 working memory（最近 N 轮对话）
//  4. 空闲超时或话题切换 → 归档 session（精华写入 Memory L1）
//
// 多平台适配：
//   - Telegram: 一个 chat = 一个 session（连续对话流）
//   - Misskey: 一条回复链 = 一个 session；时间线帖子无 session
//   - RSS/Feed: 无 session（纯信息流）
package session

import (
	"sync"
	"time"
)

// ============================================================================
// Session — 会话实体
// ============================================================================

// SessionStatus 表示会话的状态。
type SessionStatus string

const (
	// StatusActive 活跃会话，正在使用中。
	StatusActive SessionStatus = "active"
	// StatusArchived 已归档，不再接受新消息。
	StatusArchived SessionStatus = "archived"
)

// Session 代表一个连续的对话上下文。
//
// 它维护"当前对话的最近 N 轮交互"作为工作记忆，
// 让 Bot 在同一对话中保持上下文连贯性。
//
// Session 是线程安全的：多个 goroutine 可以并发读写。
type Session struct {
	mu sync.RWMutex

	// ID 会话唯一标识（由 SessionResolver 生成）。
	id string
	// BotID 所属 Bot 标识。
	botID string
	// Channel 来源会话空间标识（与 core.Message.Channel 一致）。
	channel string
	// Topic 检测到的话题摘要（可选，为空表示未提取）。
	topic string
	// Status 会话状态。
	status SessionStatus
	// messages 工作记忆：最近的对话消息（环形缓冲，FIFO）。
	messages []Message
	// maxMessages 工作记忆容量上限。
	maxMessages int
	// StartedAt 会话开始时间。
	startedAt time.Time
	// LastActivityAt 最后活动时间。
	lastActivityAt time.Time
	// CreatedBy 发起者类型（"user" / "bot"）。
	createdBy string
	// messageCount 累计消息总数（不含已淘汰的旧消息）。
	messageCount int
}

// Message 是 session 工作记忆中的一条消息记录。
type Message struct {
	// Role 消息角色（"user" / "assistant"）。
	Role string `json:"role"`
	// Text 消息文本。
	Text string `json:"text"`
	// UserID 发送者 ID（仅 user 消息）。
	UserID string `json:"userId,omitempty"`
	// Timestamp 消息时间。
	Timestamp time.Time `json:"timestamp"`
}

// NewSession 创建一个新会话。
func NewSession(id, botID, channel string, opts ...Option) *Session {
	s := &Session{
		id:            id,
		botID:         botID,
		channel:       channel,
		status:        StatusActive,
		messages:      make([]Message, 0, 20),
		maxMessages:   20,
		startedAt:     time.Now(),
		lastActivityAt: time.Now(),
		createdBy:     "user",
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Option 配置 Session 的可选参数。
type Option func(*Session)

// WithMaxMessages 设置工作记忆容量。
func WithMaxMessages(n int) Option {
	return func(s *Session) {
		if n > 0 {
			s.maxMessages = n
		}
	}
}

// WithCreatedBy 设置发起者类型。
func WithCreatedBy(creator string) Option {
	return func(s *Session) {
		if creator != "" {
			s.createdBy = creator
		}
	}
}

// ID 返回会话唯一标识。
func (s *Session) ID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

// BotID 返回所属 Bot 标识。
func (s *Session) BotID() string {
	return s.botID
}

// Channel 返回来源会话空间标识。
func (s *Session) Channel() string {
	return s.channel
}

// Topic 返回话题摘要。
func (s *Session) Topic() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.topic
}

// SetTopic 设置话题摘要。
func (s *Session) SetTopic(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.topic = topic
}

// Status 返回会话状态。
func (s *Session) Status() SessionStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// IsActive 返回会话是否活跃。
func (s *Session) IsActive() bool {
	return s.Status() == StatusActive
}

// StartedAt 返回会话开始时间。
func (s *Session) StartedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.startedAt
}

// LastActivityAt 返回最后活动时间。
func (s *Session) LastActivityAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastActivityAt
}

// CreatedBy 返回发起者类型。
func (s *Session) CreatedBy() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.createdBy
}

// MessageCount 返回累计消息总数。
func (s *Session) MessageCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.messageCount
}

// AppendMessage 向工作记忆追加一条消息。
// 超过容量上限时自动淘汰最旧的消息。
func (s *Session) AppendMessage(msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	s.messages = append(s.messages, msg)
	s.messageCount++
	s.lastActivityAt = msg.Timestamp

	// FIFO 淘汰：超过容量时移除最旧的
	if len(s.messages) > s.maxMessages {
		s.messages = s.messages[len(s.messages)-s.maxMessages:]
	}
}

// Messages 返回工作记忆中所有消息的副本。
func (s *Session) Messages() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// RecentMessages 返回最近 N 条消息。
func (s *Session) RecentMessages(n int) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n <= 0 || n > len(s.messages) {
		n = len(s.messages)
	}
	start := len(s.messages) - n
	out := make([]Message, n)
	copy(out, s.messages[start:])
	return out
}

// Archive 将会话标记为已归档。
func (s *Session) Archive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = StatusArchived
}

// IdleDuration 返回自上次活动以来的空闲时长。
func (s *Session) IdleDuration() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Since(s.lastActivityAt)
}
