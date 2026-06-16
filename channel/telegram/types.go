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
	// Caption 图片/文件的附带文字。
	Caption string `json:"caption,omitempty"`
	// Entities 消息中的格式化实体（@提及、/命令、链接等）。
	Entities []MessageEntity `json:"entities,omitempty"`
	// CaptionEntities caption 文本中的格式化实体。
	CaptionEntities []MessageEntity `json:"caption_entities,omitempty"`
	// Photo 图片消息中包含的缩略图（取最大尺寸）。
	Photo []PhotoSize `json:"photo,omitempty"`
	// Document 文件附件。
	Document *Document `json:"document,omitempty"`
	// Sticker 贴纸。
	Sticker *Sticker `json:"sticker,omitempty"`
	// MediaGroupID 相册组 ID（多张图片合并发送时共享）。
	MediaGroupID string `json:"media_group_id,omitempty"`
	// 回复相关
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

// PhotoSize 表示一张图片的某个尺寸。
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// Document 表示一个文件附件。
type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// Sticker 表示一个贴纸。
type Sticker struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	IsAnimated   bool   `json:"is_animated,omitempty"`
	IsVideo      bool   `json:"is_video,omitempty"`
	Emoji        string `json:"emoji,omitempty"`
}

// MessageEntity 表示消息文本中的一个格式化实体。
type MessageEntity struct {
	Type   string `json:"type"`           // "mention", "text_mention", "bot_command", "url", ...
	Offset int    `json:"offset"`         // 在 Text 中的 UTF-16 code unit 偏移
	Length int    `json:"length"`         // 实体长度（UTF-16 code units）
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

// sendChatActionRequest 对应 sendChatAction 方法（发送"正在输入..."等状态）。
type sendChatActionRequest struct {
	ChatID int64  `json:"chat_id"`
	Action string `json:"action"` // "typing", "upload_photo", "record_video", ...
}

// editMessageTextRequest 对应 editMessageText 方法。
type editMessageTextRequest struct {
	ChatID    int64  `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}
