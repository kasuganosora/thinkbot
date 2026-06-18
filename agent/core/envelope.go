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
	// Channel 会话空间标识，代表消息所在的"对话流"。
	// 同一会话空间中的多条消息共享同一 Channel 值，可用于关联对话上下文和记忆。
	//
	// 各平台语义：
	//   - Telegram: chatID（同一 chat 中所有消息共享）
	//   - Misskey: userID（同一用户的帖子视为一个对话流）
	//   - Memory: channel name
	//
	// 注意：Channel 不等于"outbound 回复目标"。回复目标由 Metadata["reply_target"] 指定。
	Channel string `json:"channel"`
	// ChatType 会话类型（"private" / "group" / "channel" / "supergroup"）。
	// Pipeline 可据此判断是否需要在群聊中 @mention 才回复等策略。
	// 空字符串表示未知类型，调用方应做容错处理。
	ChatType string `json:"chatType,omitempty"`
	// UserID 发送者 ID。
	UserID string `json:"userId"`
	// Text 消息文本内容。
	Text string `json:"text"`
	// Mentioned 表示此消息是否显式 @提及了 Bot。
	// 在群聊中，Pipeline 可据此决定是否只处理被 @ 的消息。
	// 私聊中通常恒为 true。
	Mentioned bool `json:"mentioned"`
	// MediaType 媒体类型（text/plain, image/png, ...）。
	MediaType string `json:"mediaType,omitempty"`
	// RawData 原始载荷（可选）。
	RawData []byte `json:"-"`
	// Metadata 扩展元数据（来源特有字段等）。
	Metadata map[string]any `json:"metadata,omitempty"`
	// CreatedAt 消息创建时间。
	CreatedAt time.Time `json:"createdAt"`
}

// ChatType 常量定义。各 Channel 应尽量映射到这些标准值。
// 平台特有类型可放在 Metadata 中补充。
const (
	// ChatPrivate 一对一私聊。
	ChatPrivate string = "private"
	// ChatGroup 群组聊天（成员可发言）。
	ChatGroup string = "group"
	// ChatSupergroup 超级群组（Telegram 特有，成员上限更大）。
	ChatSupergroup string = "supergroup"
	// ChatChannel 频道/公告板（仅管理员可发言）。
	ChatChannel string = "channel"
)

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
	// ActionNote 写入备注/内部笔记，不输出到 Channel。
	// 用于 Bot 自主决定"不回复但记住此信息"的场景。
	// Payload 为备注文本（string），Metadata 可包含关联上下文。
	// NoteHandler 处理此类型，将备注持久化供记忆模块使用。
	ActionNote ActionType = "note"
	// ActionCallback 执行回调，将结果回传给任务发起方。
	// 用于 sub-agent/子任务场景：父 Agent 创建子任务时注册回调 ID，
	// 子任务完成后通过 ActionCallback 将结果回传。
	//
	// 约定：
	//   - Metadata["callback_id"]：回调标识（必需），用于路由到正确的回调函数
	//   - Payload：回调结果数据（any 类型，由回调双方约定结构）
	//   - Metadata["status"]：任务状态（"success" / "error" / "partial"，可选）
	//   - Metadata["error"]：错误描述（status=error 时使用，可选）
	ActionCallback ActionType = "callback"
	// ActionSilent 表示 Bot 已处理消息但主动选择不做任何外部输出。
	// 与 ActionDrop 的区别：
	//   - ActionDrop = 异常/过滤导致的丢弃（被拦截）
	//   - ActionSilent = 正常决策后的主动静默（已知晓但无需回应）
	//
	// SilentHandler 仅记录 trace/log，不执行任何 I/O。
	// 典型场景：LLM 判定此消息不需要回应（如群聊中的闲聊、重复问题等）。
	ActionSilent ActionType = "silent"
	// ActionDrop 丢弃消息，不做任何输出。
	ActionDrop ActionType = "drop"
)

// Action 描述一个输出动作，由 Stage 在处理过程中累积到 Envelope 中。
type Action struct {
	// Type 动作类型。
	Type ActionType `json:"type"`
	// Channel 目标频道/会话标识。
	// 该字段的具体含义由 Outbound Sender 实现解释，不同平台语义不同：
	//   - Telegram: chatID（群组/私聊 ID）
	//   - Misskey: noteID 或 userID
	//   - Webhook: 回调 URL 或 endpoint 标识
	//   - Memory: channel name
	//
	// 设置方通常从 Message.Metadata["reply_target"] 或 Message.Channel 获取。
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

// Actions 返回累积的所有输出动作的深拷贝。
// Metadata map 也会被复制，防止调用方修改返回值影响 Envelope 内部状态。
func (e *Envelope) Actions() []Action {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Action, len(e.actions))
	for i, a := range e.actions {
		out[i] = a
		if a.Metadata != nil {
			meta := make(map[string]any, len(a.Metadata))
			for k, v := range a.Metadata {
				meta[k] = v
			}
			out[i].Metadata = meta
		}
	}
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
