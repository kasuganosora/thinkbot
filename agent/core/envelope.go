package core

import (
	"sync"
	"time"
)

// ============================================================================
// Message — 统一消息类型
// ============================================================================

// Message 表示从任何 Source 归一化后的消息。
type Message struct {
	// ID 消息唯一标识。
	ID string `json:"id"`
	// TraceID 请求追踪 ID，用于贯穿整个消息生命周期的可观测性。
	// 在 Ingress 入口自动生成（如果未设置），格式为 128-bit hex（与 OTel 兼容）。
	TraceID string `json:"traceId"`
	// BotID 所属 Bot 标识。由 Channel 在投递消息时设置。
	// 消息进入系统后 BotID 不可变，用于路由到正确的 Bot 处理链。
	BotID string `json:"botId"`
	// Source 来源标识（"webhook" / "websocket" / "polling" / "memory" 等）。
	Source string `json:"source"`
	// Channel 频道或会话 ID。
	Channel string `json:"channel"`
	// UserID 发送者 ID。
	UserID string `json:"userId"`
	// Text 消息文本内容。
	Text string `json:"text"`
	// MediaType 媒体类型（text/plain, image/png, ...）。
	MediaType string `json:"mediaType,omitempty"`
	// RawData 原始载荷（可选）。
	RawData []byte `json:"-"`
	// Metadata 扩展元数据（来源特有字段等）。
	Metadata map[string]any `json:"metadata,omitempty"`
	// CreatedAt 消息创建时间。
	CreatedAt time.Time `json:"createdAt"`
}

// ============================================================================
// Action — 输出动作
// ============================================================================

// ActionType 指示 Outbound Dispatcher 如何派发消息。
type ActionType string

const (
	// ActionReply 回复原始消息。
	ActionReply ActionType = "reply"
	// ActionForward 转发到另一个频道/用户。
	ActionForward ActionType = "forward"
	// ActionBroadcast 广播到多个频道。
	ActionBroadcast ActionType = "broadcast"
	// ActionDrop 丢弃消息，不做任何输出。
	ActionDrop ActionType = "drop"
)

// Action 描述一个输出动作，由 Stage 在处理过程中累积到 Envelope 中。
type Action struct {
	// Type 动作类型。
	Type ActionType `json:"type"`
	// Channel 目标频道 ID。
	Channel string `json:"channel,omitempty"`
	// UserID 目标用户 ID。
	UserID string `json:"userId,omitempty"`
	// Payload 要发送的内容。
	Payload any `json:"payload,omitempty"`
	// Metadata 扩展字段。
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ============================================================================
// Envelope — 消息信封（贯穿整个 Pipeline）
// ============================================================================

// Envelope 承载消息在 Pipeline 中流转的全部状态。
// 它是线程安全的：多个 goroutine 可以并发读写 Values 和 Actions。
type Envelope struct {
	// Message 原始输入消息（不可变）。
	Message Message

	mu      sync.RWMutex
	actions []Action
	values  map[string]any
	err     error
	aborted bool
}

// NewEnvelope 创建一个新的消息信封。
func NewEnvelope(msg Message) *Envelope {
	return &Envelope{
		Message: msg,
		values:  make(map[string]any),
	}
}

// Set 设置 Stage 间共享的键值对。
func (e *Envelope) Set(key string, val any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.values[key] = val
}

// Get 获取 Stage 间共享的值。
func (e *Envelope) Get(key string) (any, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	v, ok := e.values[key]
	return v, ok
}

// MustGet 获取值，不存在时 panic。
func (e *Envelope) MustGet(key string) any {
	v, ok := e.Get(key)
	if !ok {
		panic("envelope: missing key: " + key)
	}
	return v
}

// AddAction 向信封追加一个输出动作。
func (e *Envelope) AddAction(a Action) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.actions = append(e.actions, a)
}

// Actions 返回累积的所有输出动作的副本。
func (e *Envelope) Actions() []Action {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Action, len(e.actions))
	copy(out, e.actions)
	return out
}

// Abort 标记信封为中止状态，Pipeline 将停止后续 Stage 的执行。
func (e *Envelope) Abort(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.aborted = true
	e.err = err
}

// Aborted 返回信封是否已被中止。
func (e *Envelope) Aborted() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.aborted
}

// Err 返回信封中记录的错误。
func (e *Envelope) Err() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.err
}

// SetErr 设置错误状态（不中止 Pipeline）。
func (e *Envelope) SetErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}
