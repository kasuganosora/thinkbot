package telegram

// ============================================================================
// Telegram Bot API 类型定义
// ============================================================================
// 仅定义 thinkbot 所需的字段子集，未使用的字段省略。
// 完整 API 文档: https://core.telegram.org/bots/api

// Update 表示一个来自 Telegram 的更新事件。
type Update struct {
	UpdateID      int64              `json:"update_id"`
	Message       *Message           `json:"message,omitempty"`
	EditedMessage *Message           `json:"edited_message,omitempty"`
	MyChatMember  *ChatMemberUpdated `json:"my_chat_member,omitempty"`
}

// Message 表示一条 Telegram 消息。
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text,omitempty"`
	// Entities 消息中的格式化实体（@提及、/命令、链接等）。
	Entities []MessageEntity `json:"entities,omitempty"`
	// 回复相关
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

// MessageEntity 表示消息文本中的一个格式化实体。
type MessageEntity struct {
	Type   string `json:"type"`           // "mention", "text_mention", "bot_command", "url", ...
	Offset int    `json:"offset"`         // 在 Text 中的字节偏移
	Length int    `json:"length"`         // 实体长度（字节）
	User   *User  `json:"user,omitempty"` // 仅 text_mention 有效
}

// Chat 表示一个聊天会话。
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // "private", "group", "supergroup", "channel"
	Title     string `json:"title,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// User 表示一个 Telegram 用户。
type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

// ChatMemberUpdated 表示聊天成员变更事件。
type ChatMemberUpdated struct {
	From          User       `json:"from"`
	Chat          Chat       `json:"chat"`
	Date          int64      `json:"date"`
	OldChatMember ChatMember `json:"old_chat_member"`
	NewChatMember ChatMember `json:"new_chat_member"`
}

// ChatMember 表示聊天成员信息（简化版）。
type ChatMember struct {
	User   User   `json:"user"`
	Status string `json:"status"` // "creator", "administrator", "member", "left", "kicked"
}

// ----------------------------------------------------------------------------
// API 请求/响应类型
// ----------------------------------------------------------------------------

// apiResponse 是所有 Telegram Bot API 响应的通用包装。
type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
}

// getUpdatesRequest 对应 getUpdates 方法。
type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	Timeout        int      `json:"timeout,omitempty"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// sendMessageRequest 对应 sendMessage 方法。
type sendMessageRequest struct {
	ChatID           int64  `json:"chat_id"`
	Text             string `json:"text"`
	ParseMode        string `json:"parse_mode,omitempty"`
	ReplyToMessageID int64  `json:"reply_to_message_id,omitempty"`
	DisablePreview   bool   `json:"disable_web_page_preview,omitempty"`
}

// sendMessageResult 是 sendMessage 返回的消息。
type sendMessageResult struct {
	MessageID int64 `json:"message_id"`
}


