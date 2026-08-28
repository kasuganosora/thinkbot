package core

import (
	"context"
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
	// InjectContext 模型可见但不作为「对话内容」的注入上下文（如心跳唤醒提示）。
	//
	// 与 Text 的区别：Text 是用户原文，会被 note_capture 捕获为 L0 长期记忆
	// （speaker:"user"）；InjectContext 由特定触发源（如心跳）设置，仅在本轮
	// pipeline 内对 LLM 可见（拼入 user/system 消息），不参与记忆捕获、不进对话历史。
	// 这避免了「把系统唤醒提示当成用户原话写进长期记忆」的污染（见 docs/heartbeat-redesign.md §7）。
	InjectContext string `json:"injectContext,omitempty"`
	// Mentioned 表示此消息是否显式 @提及了 Bot。
	// 在群聊中，Pipeline 可据此决定是否只处理被 @ 的消息。
	// 私聊中通常恒为 true。
	Mentioned bool `json:"mentioned"`
	// FromIsBot 表示发送者是否为 Bot 账号。
	// agent 可据此感知对方身份，自行决定是否/如何回复（不强制拦截）。
	FromIsBot bool `json:"fromIsBot,omitempty"`
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

// NormalizeChatType 归一化会话类型：把平台特有类型归入标准类型。
//
// 目前唯一需要归一是 Telegram 的 supergroup —— 普通群升级后即为 supergroup，
// 绝大多数活跃群都是它。若下游按字面量 == ChatGroup 匹配，supergroup 会漏判，
// 导致 Scopes 含 "group" 的工具（含 memory）在超级群里被整体剔除、bot 失忆。
//
// 凡是需要按会话类型做匹配的地方（工具 Scopes、权限策略规则等）都应调用本函数，
// 且建议两侧（上下文值与规则配置值）都归一化，这样用户配 "group" 或 "supergroup" 都能命中。
// 未识别的类型返回原值，不做假设。
func NormalizeChatType(ct string) string {
	switch ct {
	case ChatSupergroup:
		return ChatGroup
	default:
		return ct
	}
}

// Source 常量：消息来源标识（Message.Source 的取值）。
const (
	// SourceHeartbeat 系统自主心跳唤醒。
	// bot 被周期性触发自我审视（看有什么事做），不是用户发来的消息。
	// 心跳消息走与 @bot 相同的 pipeline，但由准入/闸门控制其自主行为，
	// 且不参与 L0 记忆捕获（见 note_capture / llmroute）。
	SourceHeartbeat string = "heartbeat"

	// SourceCron 用户级定时任务触发。
	// 由 cron 调度器在 Job 到期时自动注入，bot 以「无人监督」模式跑一次 Job.Prompt。
	// 与心跳类似走完整 pipeline，但目的是产出并投递结果到指定渠道（Channel 非空时）。
	SourceCron string = "cron"
)

// ============================================================================
// Envelope KV 约定键
// ============================================================================

// Envelope KV 中由多个包共同约定的键名。
// 定义在 core 包避免跨包硬编码字符串（写错一个字符就静默失效）。
const (
	// KVSuppressReply 标记「本轮不要向用户发送回复」。
	//
	// 由参与度评估（engagement）等上游 Stage 在判定「此刻不该说话」时设置，
	// 由产出 ActionReply 的 Stage（LLMStage）读取并跳过回复动作。
	//
	// 为什么不直接 Abort 整条 Pipeline：Bot 仍应「听到并思考」这条消息 ——
	// 记忆写入、画像更新等下游 Stage 需要继续执行。抑制的只是**对外发送**，
	// 而非整轮处理。这正是「心里话不发出去」的语义。
	//
	// 值类型 bool；仅 true 生效。
	KVSuppressReply = "reply.suppress"

	// KVSuppressReplyReason 记录抑制原因（字符串），供日志与 trace 排查。
	// 静默降级必须可解释，否则会被误判为 Bot 故障。
	KVSuppressReplyReason = "reply.suppress_reason"

	// KVSuppressReasonPassive 是「被动回复（仅被 @ 才回）模式下，此消息未被真人 @」
	// 这一**硬权限门**的 reason 值。它与软节流原因（rhythm_*/engagement_*）有本质区别：
	// 软原因是「此刻该不该说」的概率/节奏判断，可被模型显式 REPLY_CONTROL send:true 覆盖；
	// 而本原因是「bot 根本没有在此消息上发言的权限」，绝不可被模型覆盖。
	//
	// 该 reason 在 ingress 阶段由 passive-speak enricher 写入，必须在整条 pipeline 中保持
	// 不被节奏等软门改写（见 agent/stages/rhythm.go 的早退保护），否则会被 reply-control
	// 的模型放行覆盖，导致被动 bot 对未 @ 的消息发帖。
	KVSuppressReasonPassive = "passive_mode_unmentioned"

	// KVLurkMode 标记当前消息来自「潜水 / 只读」渠道。
	//
	// 由 lurk-detect enricher 在渠道只读（permSvc.IsReadOnly 为 true）时设置。
	// LLMStage 读取后切换为「观察者模式」：仍然正常调用 LLM 思考，
	// 但把结果作为内部学习笔记（ActionNote）写入 L0 工作记忆，绝不发帖。
	//
	// 语义：潜水 = 说（speak）关闭，学（learn）开启。这与 KVSuppressReply 不同——
	// 后者只是「本轮不发送」，仍可能因为没有 ActionNote 而什么都没记住；
	// 本标志确保潜水时「看而学」，思考结果沉淀为长期记忆。
	// 值类型 bool；仅 true 生效。
	KVLurkMode = "lurk.mode"

	// KVSoulContent 携带 bot 的 SOUL.md 人格文本（由 Bot.OnBeforeProcess 注入），
	// 供 LLMStage 在潜水模式下结合人格构建观察者 prompt（「结合 soul.md 模块分析」）。
	KVSoulContent = "bot.soul.content"

	// KVMemoryRecall 携带从长期记忆检索出的「对话上下文记忆」文本（由 RecallStage 注入），
	// 供 LLMStage 拼入 system prompt。这是「潜水学到的经验在真人交互里浮现」闭环的读侧：
	// 写入侧由潜水观察者（LLMStage lurk 分支）产出 ActionNote 沉淀进 L0/L1，
	// 读取侧由 RecallStage 在每轮对话前按 [bot, channel, user] 三 scope 召回并注入。
	// 值类型 string；空串表示无相关记忆。
	KVMemoryRecall = "memory.recall"

	// KVLLMDeferred 标记「本轮因工具审批被 defer 而暂停」——LLMStage 已阻断半成品回复，
	// 等待人类确认后由 ResumeDeferredApproval 续跑。供下游 Stage / 可观测层识别此状态。
	// 值类型 bool；仅 true 生效。
	KVLLMDeferred = "llm.deferred"

	// KVHeartbeatMode 标记当前消息为「心跳自主唤醒」决策模式。
	//
	// 由心跳 Executor 在构造唤醒消息时设置。LLMStage 读取后强制 JSON 结构化输出
	// （与潜水模式同机制：ResponseFormat=json_object + 低温），使心跳决策
	// （silent / post / note + 目标渠道 + 内容）成为结构化字段，杜绝自由文本歧义
	// （LLM 换个说法表达「静默」就被程序解析失败的老问题）。
	//
	// 心跳恒设 KVSuppressReply=true：LLM 照常思考（记忆/工具/SOUL 全在线），
	// 但不走伪频道 "heartbeat" 的通用出站；决策产出的真实发帖由 Executor 经
	// ChannelPoster 手动路由到选定渠道，绕开 "no sender for channel heartbeat" 死路。
	// 值类型 bool；仅 true 生效。
	KVHeartbeatMode = "heartbeat.mode"

	// KVHeartbeatTargets 携带本次心跳「可发帖目标」列表（[]heartbeat.ChannelTarget，
	// 由心跳包设置，core 仅持有 string 键）。供唤醒提示词展示给 LLM 选择，
	// 并由 Executor 校验 LLM 选定的目标合法性——只认列表内存在的真实渠道/会话。
	KVHeartbeatTargets = "heartbeat.targets"
)

// ============================================================================
// Channel 能力接口（供自主心跳等场景枚举发帖目标 / 直接发帖）
//
// 定义在 core 低层包，避免 heartbeat → channel 的循环依赖：
// channel 实现这些接口，heartbeat / botservice 通过接口消费，互不 import。
// ============================================================================

// ChatRef 描述一个可发帖的会话引用，由 Channel 在入站时记录，
// 供自主心跳等场景枚举「能在哪些会话主动发言」。
type ChatRef struct {
	// ID 平台会话 ID（如 Telegram chatID）。
	ID int64 `json:"id"`
	// Title 会话标题（群名等），可能为空。
	Title string `json:"title,omitempty"`
}

// RecentChatLister 由 Channel 实现，返回近期活跃会话列表。
// 心跳据此把「近期聊过的 Telegram 群 / Misskey 对话」作为可选发帖目标呈现给 LLM。
type RecentChatLister interface {
	RecentChats() []ChatRef
}

// TimelinePoster 由 Channel 实现，支持向自身时间线 / 动态发顶层新帖
// （如 Misskey 时间线）。心跳「想对大家说点什么」时走此路径，
// 而非回复某条具体帖子（回复走通用 Sender.Send + noteID 目标）。
type TimelinePoster interface {
	// PostTimeline 发布一条顶层新帖，返回新帖 ID。
	PostTimeline(ctx context.Context, text, visibility, cw string) (string, error)
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
