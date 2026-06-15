package misskey

import "encoding/json"

// ============================================================================
// Misskey API / Streaming 类型定义
// ============================================================================
// 仅定义 thinkbot 所需的字段子集。
// 完整 API 文档: https://misskey-hub.net/en/docs/api/

// ----------------------------------------------------------------------------
// Streaming 消息类型
// ----------------------------------------------------------------------------

// streamMessage 是所有 WebSocket 消息的基础结构。
type streamMessage struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// connectBody 是 type=connect 时 body 的结构。
type connectBody struct {
	Channel string          `json:"channel"`
	ID      string          `json:"id"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// channelMessage 是 type=channel 时服务端推送的消息。
type channelMessage struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// ----------------------------------------------------------------------------
// Note（帖子）类型
// ----------------------------------------------------------------------------

// Note 表示一条 Misskey 帖子。
type Note struct {
	ID                string `json:"id"`
	CreatedAt         string `json:"createdAt"`
	Text              string `json:"text"`
	CW                string `json:"cw,omitempty"`
	User              User   `json:"user"`
	Visibility        string `json:"visibility,omitempty"`
	ReplyID           string `json:"replyId,omitempty"`
	RenoteID          string `json:"renoteId,omitempty"`
	URI               string `json:"uri,omitempty"`
	URL               string `json:"url,omitempty"`
	LocalOnly         bool   `json:"localOnly,omitempty"`
	NoExtractMentions bool   `json:"noExtractMentions,omitempty"`
}

// User 表示一个 Misskey 用户。
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username"`
	Host     string `json:"host,omitempty"`
}

// Visibility 可见性常量。
const (
	VisibilityPublic    = "public"
	VisibilityHome      = "home"
	VisibilityFollowers = "followers"
	VisibilitySpecified = "specified"
)

// ----------------------------------------------------------------------------
// API 请求/响应类型
// ----------------------------------------------------------------------------

// createNoteRequest 对应 notes/create API。
type createNoteRequest struct {
	I          string `json:"i"`
	Text       string `json:"text"`
	ReplyID    string `json:"replyId,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

// createNoteResponse 对应 notes/create 响应中 createdNote 的内容。
type createNoteResponse struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

// createNoteAPIResponse 是 notes/create API 的外层包装。
// Misskey API 返回 {"createdNote": {...}}，需要解包。
type createNoteAPIResponse struct {
	CreatedNote createNoteResponse `json:"createdNote"`
}

// getSelfRequest 对应 i API（获取当前用户信息）。
type getSelfRequest struct {
	I string `json:"i"`
}
