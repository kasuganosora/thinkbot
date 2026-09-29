package dao

import "time"

// ChatMessage 持久化的聊天消息记录。
// 记录 Web 聊天中的每一条用户消息和 Bot 回复，支持游标分页查询。
//
// 索引设计：复合索引 (bot_id, user_id, created_at DESC) 覆盖
// 游标分页查询的 WHERE + ORDER BY，时间复杂度 O(log N + page_size)。
type ChatMessage struct {
	// ID 自增主键（游标的一部分，用于同时间戳的消息排序）。
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`

	// BotID Bot 标识。
	BotID string `gorm:"size:64;not null;index:idx_chat_session_time,priority:1" json:"botId"`

	// UserID 用户标识。
	UserID string `gorm:"size:64;not null;index:idx_chat_session_time,priority:2" json:"userId"`

	// SessionID 会话 ID（关联 ChatSession）。用于按会话隔离消息与上下文。
	// 空字符串表示未归属到具体会话（兼容历史数据 / 未选择会话时）。
	SessionID string `gorm:"size:64;index:idx_chat_msg_session" json:"sessionId"`

	// Role 消息角色："user" 或 "assistant"。
	Role string `gorm:"size:16;not null" json:"role"`

	// Content 消息内容。
	Content string `gorm:"type:text" json:"content"`

	// ToolCalls 本轮 assistant 回复关联的工具调用信息（JSON 序列化数组）。
	// 仅 assistant 消息可能有值；user 消息为空。刷新后前端据此复原工具卡片。
	ToolCalls string `gorm:"type:text" json:"-"`

	// PartsJSON 有序 parts 数组（JSON），按 LLM 实际输出顺序排列：
	//   [{type:"text",content:"..."},{type:"tool",id:"...",name:"...",...},...]
	// 保留文本与工具调用的交错位置信息，解决"所有工具堆在消息底部"的问题。
	// 空字符串或 null 表示旧格式消息（仅 content + tool_calls），前端降级处理。
	PartsJSON string `gorm:"type:text;default:''" json:"parts,omitempty"`

	// TraceID 追踪 ID，用于关联请求-回复对。
	TraceID string `gorm:"size:128" json:"traceId"`

	// Streaming 标记该 assistant 消息是否仍在流式产出中。
	//
	// 流式回复采用增量落库（按 traceID upsert），因此 DB 中会出现「未完成」的中间态行，
	// 让用户中途刷新页面也能看到已产出的内容。该字段用于把这种中间态告知前端：
	//   - true：本轮尚未结束，其中 status="running" 的工具卡片是**真的还在跑**。
	//   - false：本轮已收尾（正常完成或断连收尾）。此时若仍有 running 工具，
	//     说明进程在中途终止，前端应将其显示为中断而非无限转圈。
	// 历史数据默认 false，语义与旧行为一致（旧逻辑只在收尾时写一次）。
	Streaming bool `gorm:"not null;default:false" json:"streaming"`

	// ---- 以下为会话划分（S0）新增列，S0 阶段**不参与任何逻辑**，仅建列/索引 ----
	// 全部 `json:"-"`：S0 要求「零行为变化」，不对既有 API 响应暴露新字段。

	// ExternalMsgID 渠道内消息/帖子的原生 ID（Misskey noteID / Telegram messageID）。
	// 用途一：让 I1「一条 inbound 物理只落一行」成为**数据库不变量**——目前只靠
	//         dedupSeen 的 2 分钟内存窗口撑着，超窗重放会双写（2026-09-01 事故同类）；
	// 用途二：fork 的 reclaim 按它精确定位行。
	// 空串 = 无外部 ID（Web 会话消息），不进 partial 唯一索引。
	ExternalMsgID string `gorm:"size:128;not null;default:''" json:"-"`

	// IsContext 该行是否是被引用进本会话的「外部上下文」（如 thread 视图里 root 的引用行）。
	// S1/S2 的 X2 前端「话题起点」置顶卡会用到它，届时放开 json tag。
	IsContext bool `gorm:"not null;default:false" json:"-"`

	// OriginSessionID fork 迁移前的 session_id（留痕，I1 硬要求）。
	// 缺它则 session_id 的单向迁移不可回滚、不可审计「这条来自时间线」。
	OriginSessionID string `gorm:"size:64;not null;default:''" json:"-"`

	// ForkedSessionID 仅 root 行使用：标记该 root 已分叉出的 thread 会话。
	// 前端据此在 root 上显示「已分叉出对话 ↗」。
	ForkedSessionID string `gorm:"size:64;not null;default:''" json:"-"`

	// CreatedAt 创建时间（游标的主排序键）。
	CreatedAt time.Time `gorm:"not null;index:idx_chat_session_time,priority:3,sort:desc" json:"createdAt"`
}

// TableName 指定 GORM 表名。
func (ChatMessage) TableName() string { return "chat_messages" }

// 消息角色常量。
const (
	ChatRoleUser      = "user"
	ChatRoleAssistant = "assistant"

	// ChatRoleContextSummary 仅用于内存中的合成消息（不落库）：LLM 上下文加载时
	// 由上下文检查点（ContextCheckpoint）生成，代表被压缩的更早历史。
	ChatRoleContextSummary = "context_summary"
	// ChatRoleNotify 是 notify 接口写入主人会话的系统备注（外部程序推送的通知原文要点，
	// 外部数据）。落库；加载为 LLM 上下文时转成 system 消息（见 api.chatHistoryToLLM），
	// 纯文本、不含 tool 调用。
	ChatRoleNotify = "notify"
)
