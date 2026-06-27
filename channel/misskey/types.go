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
	ID                string   `json:"id"`
	CreatedAt         string   `json:"createdAt"`
	Text              string   `json:"text"`
	CW                string   `json:"cw,omitempty"`
	UserID            string   `json:"userId,omitempty"`
	User              User     `json:"user"`
	Visibility        string   `json:"visibility,omitempty"`
	ReplyID           string   `json:"replyId,omitempty"`
	RenoteID          string   `json:"renoteId,omitempty"`
	Reply             *Note    `json:"reply,omitempty"`
	Renote            *Note    `json:"renote,omitempty"`
	Files             []File   `json:"files,omitempty"`
	Mentions          []string `json:"mentions,omitempty"`
	URI               string   `json:"uri,omitempty"`
	URL               string   `json:"url,omitempty"`
	LocalOnly         bool     `json:"localOnly,omitempty"`
	NoExtractMentions bool     `json:"noExtractMentions,omitempty"`
}

// File 表示 Misskey 帖子附带的文件。
type File struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"` // MIME type
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
	Size         int64  `json:"size,omitempty"`
	IsSensitive  bool   `json:"isSensitive,omitempty"`
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
// 注意：根据 Misskey 源码，text 是条件必需的——仅在无 renoteId/fileIds/poll 时强制要求。
// 纯转发或带文件帖子可以省略 text。
type createNoteRequest struct {
	I          string   `json:"i"`
	Text       string   `json:"text,omitempty"`
	ReplyID    string   `json:"replyId,omitempty"`
	RenoteID   string   `json:"renoteId,omitempty"`
	Visibility string   `json:"visibility,omitempty"`
	CW         string   `json:"cw,omitempty"`
	FileIDs    []string `json:"fileIds,omitempty"`
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

// reactionRequest 对应 notes/reactions/create 和 notes/reactions/delete。
type reactionRequest struct {
	I        string `json:"i"`
	NoteID   string `json:"noteId"`
	Reaction string `json:"reaction,omitempty"` // 仅 create 需要
}

// deleteNoteRequest 对应 notes/delete。
type deleteNoteRequest struct {
	I      string `json:"i"`
	NoteID string `json:"noteId"`
}

// ----------------------------------------------------------------------------
// 关注/取消关注 API 类型
// ----------------------------------------------------------------------------

// followRequest 对应 following/create。
type followRequest struct {
	I      string `json:"i"`
	UserID string `json:"userId"`
}

// unfollowRequest 对应 following/delete。
type unfollowRequest struct {
	I      string `json:"i"`
	UserID string `json:"userId"`
}

// ----------------------------------------------------------------------------
// 文件上传 API 类型
// ----------------------------------------------------------------------------

// driveUploadResponse 对应 drive/files/create 的响应。
//
//nolint:unused // 预留：供 future misskey_upload_file 工具使用
type driveUploadResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Size     int64  `json:"size"`
	IsFolder bool   `json:"isFolder"`
}

// ----------------------------------------------------------------------------
// 用户搜索 API 类型
// ----------------------------------------------------------------------------

// searchUserRequest 对应 users/search。
type searchUserRequest struct {
	I      string `json:"i"`
	Query  string `json:"query"`
	Limit  int    `json:"limit,omitempty"`
	Origin string `json:"origin,omitempty"` // "local"/"remote"/"combined"
}

// UserDetail 包含用户详细信息（users/show 响应）。
type UserDetail struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	Username       string `json:"username"`
	Host           string `json:"host,omitempty"`
	Description    string `json:"description,omitempty"`
	FollowersCount int    `json:"followersCount"`
	FollowingCount int    `json:"followingCount"`
	NotesCount     int    `json:"notesCount"`
	IsFollowing    bool   `json:"isFollowing"`
	IsFollowed     bool   `json:"isFollowed"`
	AvatarURL      string `json:"avatarUrl,omitempty"`
}

// followingListRequest 对应 users/following。
//
//nolint:unused // 预留：供 getUserDetail API 方法使用
type getUserDetailRequest struct {
	I      string `json:"i"`
	UserID string `json:"userId"`
}

type followingListRequest struct {
	I      string `json:"i"`
	UserID string `json:"userId,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// FollowingUser 是关注列表中的用户条目。
type FollowingUser struct {
	ID       string `json:"id"`
	Followee User   `json:"followee"`
	Follower User   `json:"follower"`
}
