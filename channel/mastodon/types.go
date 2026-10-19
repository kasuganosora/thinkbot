package mastodon

import (
	"encoding/json"
	"errors"
	"strings"
)

// errEmptyPayload 表示 streaming 事件的 payload 为空（如 ping 心跳）。
var errEmptyPayload = errors.New("mastodon: empty streaming payload")

// ============================================================================
// Mastodon API / Streaming 类型定义
// ============================================================================
// 仅定义 thinkbot 所需的字段子集。
// 完整 API 文档: https://docs.joinmastodon.org/api/

// ----------------------------------------------------------------------------
// Streaming 消息类型
// ----------------------------------------------------------------------------

// streamEvent 是 Mastodon WebSocket streaming 的顶层事件结构。
//
// 与 Misskey 的 {type, body} 不同，Mastodon 是 {event, payload}：
//   - event: 事件名（"update" / "notification" / "delete" / "ping" ...）
//   - payload: **JSON 编码的字符串**，需要二次反序列化（delete 事件除外，是纯 ID 字符串）
//
// 部分实现（如 Pleroma）会直接把 payload 作为对象下发，故 payload 用
// json.RawMessage 接收，解码时兼容「字符串套 JSON」与「裸对象」两种形态。
type streamEvent struct {
	// Stream 标识该事件来自哪个订阅流（如 ["public:local"]），仅作日志用。
	Stream []string `json:"stream,omitempty"`
	Event  string   `json:"event"`
	// Payload 可能是 JSON 字符串（标准 Mastodon）、裸 JSON 对象或纯文本（delete）。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// decodePayload 把 streamEvent.Payload 解析到 out。
// 兼容两种服务端形态：
//  1. 标准 Mastodon：payload 是 JSON 编码的字符串 → 先解出字符串再解一次
//  2. 部分兼容实现：payload 直接是 JSON 对象
func decodePayload(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return errEmptyPayload
	}
	// 形态 1：JSON 字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return errEmptyPayload
		}
		return json.Unmarshal([]byte(s), out)
	}
	// 形态 2：裸对象
	return json.Unmarshal(raw, out)
}

// payloadID 把 payload 当纯文本 ID 取出（delete 事件用）。
func payloadID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

// ----------------------------------------------------------------------------
// Account（用户）类型
// ----------------------------------------------------------------------------

// Account 表示一个 Mastodon 账号。
type Account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	// Acct 是带域名的完整账号标识：本站为 "alice"，远程为 "alice@example.social"。
	// 与 Misskey 的 username + host 两字段不同，Mastodon 直接给合并好的 acct，
	// 出站提及必须用它，否则联邦用户收不到通知。
	Acct        string `json:"acct"`
	DisplayName string `json:"display_name,omitempty"`
	// Bot 对应 Misskey 的 isBot，用于让 LLM 感知对方是机器人账号。
	Bot    bool   `json:"bot,omitempty"`
	Note   string `json:"note,omitempty"`
	URL    string `json:"url,omitempty"`
	Avatar string `json:"avatar,omitempty"`

	FollowersCount int `json:"followers_count,omitempty"`
	FollowingCount int `json:"following_count,omitempty"`
	StatusesCount  int `json:"statuses_count,omitempty"`
}

// handle 返回用于展示/提及的 @acct（保证带前导 @）。
func (a Account) handle() string {
	if a.Acct == "" {
		return "@" + a.Username
	}
	return "@" + a.Acct
}

// ----------------------------------------------------------------------------
// Status（帖子）类型
// ----------------------------------------------------------------------------

// Status 表示一条 Mastodon 帖子（toot）。
//
// 关键差异（相对 Misskey Note）：
//   - Content 是 **HTML**（<p>…</p>），不是纯文本，入站必须转纯文本；
//   - 纯转发（boost）表现为 Reblog != nil 且 Content 为空字符串；
//   - 引用转发在 Mastodon 4.x 起才有，且表现为 Reblog != nil 且 Content 非空；
//   - 被转发内容在 Reblog 字段里（不是 Misskey 那样 renoteId 指针 + 独立 note）。
type Status struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	// InReplyToID 回复目标帖子 ID（Misskey 的 replyId）。纯转发帖此字段为空。
	InReplyToID string `json:"in_reply_to_id,omitempty"`
	// InReplyToAccountID 被回复者账号 ID，用于 timeline 判定「是否指向本 Bot」。
	InReplyToAccountID string `json:"in_reply_to_account_id,omitempty"`
	Sensitive          bool   `json:"sensitive,omitempty"`
	// SpoilerText 即 CW 折叠文本（Misskey 的 cw）。
	SpoilerText string `json:"spoiler_text"`
	Visibility  string `json:"visibility,omitempty"`
	URI         string `json:"uri,omitempty"`
	URL         string `json:"url,omitempty"`
	// Content 是 HTML 正文。纯 boost 时为空字符串。
	Content string  `json:"content"`
	Account Account `json:"account"`
	// Reblog 非 nil 表示这是一条转发（boost），里面是被转发的原帖
	// （与 Misskey 的 Renote 字段对等，但 Mastodon 总是内联完整对象）。
	Reblog           *Status           `json:"reblog,omitempty"`
	MediaAttachments []MediaAttachment `json:"media_attachments,omitempty"`
	Mentions         []Mention         `json:"mentions,omitempty"`
	Tags             []Tag             `json:"tags,omitempty"`
	Poll             *Poll             `json:"poll,omitempty"`
	// Favourited / Reblogged 是「本账号（bot）是否已表态」，用于对等 Misskey 的
	// ALREADY_REACTED / NOT_REACTED 幂等判定。
	Favourited bool `json:"favourited,omitempty"`
	Reblogged  bool `json:"reblogged,omitempty"`
	// RepliesCount 等计数，仅作上下文用。
	RepliesCount    int `json:"replies_count,omitempty"`
	ReblogsCount    int `json:"reblogs_count,omitempty"`
	FavouritesCount int `json:"favourites_count,omitempty"`
}

// IsPureReblog reports whether the status is a plain boost (reblog wrapper with
// no own content). Unlike Misskey, Mastodon *does* allow replying to a boost,
// so this is informational only — no reply suppression is applied.
func (s *Status) IsPureReblog() bool {
	return s != nil && s.Reblog != nil && htmlToText(s.Content) == ""
}

// Mention 是帖子中显式提及的账号。
type Mention struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Acct     string `json:"acct"`
	URL      string `json:"url"`
}

// Tag 是帖子中的话题标签。
type Tag struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// MediaAttachment 表示帖子附带媒体。
//
// 与 Misskey File 的差异：Mastodon 的 Type 是分类枚举
// （"image"/"gifv"/"video"/"audio"/"unknown"）而非 MIME 字符串，
// 真正的 MIME 在 meta 里且经常缺省，故归一化时按 Type 推断即可。
type MediaAttachment struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	PreviewURL  string `json:"preview_url,omitempty"`
	RemoteURL   string `json:"remote_url,omitempty"`
	Description string `json:"description,omitempty"`
}

// Poll 表示帖子附带的投票。
//
// 与 Misskey 的 Poll 结构不同：Mastodon 服务端返回的是「带票数的只读快照」，
// 且 **不暴露每个投票人身份**（只有 own_votes 表示自己投了哪些）。
// 这直接影响 user_choice 回填：无法像 Misskey 那样从事件里拿到 voter，
// 只能靠票数变化推断「有人投了」，详见 channel.go 的轮询回填实现。
type Poll struct {
	ID          string       `json:"id"`
	ExpiresAt   string       `json:"expires_at,omitempty"`
	Expired     bool         `json:"expired,omitempty"`
	Multiple    bool         `json:"multiple,omitempty"`
	VotesCount  int          `json:"votes_count,omitempty"`
	VotersCount int          `json:"voters_count,omitempty"`
	Voted       bool         `json:"voted,omitempty"`
	OwnVotes    []int        `json:"own_votes,omitempty"`
	Options     []PollOption `json:"options,omitempty"`
}

// PollOption 是投票的一个选项及其票数。
type PollOption struct {
	Title      string `json:"title"`
	VotesCount int    `json:"votes_count,omitempty"`
}

// ----------------------------------------------------------------------------
// Notification（通知）类型 — mention / favourite 入站感知
// ----------------------------------------------------------------------------

// Notification 是 Mastodon 的通知对象。
// mention 类通知的 Status 字段非空；favourite / reblog / follow 类可能为空
// （favourite 有 status，follow 没有）。
type Notification struct {
	ID        string  `json:"id"`
	CreatedAt string  `json:"created_at"`
	Type      string  `json:"type"`
	Account   Account `json:"account"`
	Status    *Status `json:"status,omitempty"`
}

// ----------------------------------------------------------------------------
// Visibility 可见性常量
// ----------------------------------------------------------------------------
// Mastodon 的可见性集合与 Misskey 不对等，映射关系见 mapVisibility：
//
//	Misskey public    → Mastodon public
//	Misskey home      → Mastodon unlisted
//	Misskey followers → Mastodon private
//	Misskey specified → Mastodon direct
const (
	VisibilityPublic   = "public"
	VisibilityUnlisted = "unlisted"
	VisibilityPrivate  = "private"
	VisibilityDirect   = "direct"
)

// ----------------------------------------------------------------------------
// API 请求类型
// ----------------------------------------------------------------------------

// createStatusRequest 对应 POST /api/v1/statuses。
//
// Mastodon 用 form 参数，但 Rails 同样接受 JSON body；本项目 HTTP 客户端
// 仅提供 SetJSONBody，故走 JSON。嵌套的 Poll 会被 Rails 解析成
// poll[options][] 等参数，与官方 form 语义一致。
type createStatusRequest struct {
	Status      string       `json:"status,omitempty"`
	InReplyToID string       `json:"in_reply_to_id,omitempty"`
	Visibility  string       `json:"visibility,omitempty"`
	SpoilerText string       `json:"spoiler_text,omitempty"`
	MediaIDs    []string     `json:"media_ids,omitempty"`
	Poll        *pollRequest `json:"poll,omitempty"`
	Language    string       `json:"language,omitempty"`
}

// pollRequest 是 createStatusRequest 的嵌套投票参数。
// ExpiresIn 单位为秒（Mastodon 规范），与 Misskey 的 expiresAt 毫秒时间戳不同。
type pollRequest struct {
	Options    []string `json:"options"`
	ExpiresIn  int      `json:"expires_in,omitempty"`
	Multiple   bool     `json:"multiple,omitempty"`
	HideTotals bool     `json:"hide_totals,omitempty"`
}

// createStatusResponse 是 POST /api/v1/statuses 的响应（完整 Status 子集）。
// 需要拿到 ID 与 Poll.ID 才能启动投票结果轮询。
type createStatusResponse struct {
	ID         string `json:"id"`
	URL        string `json:"url,omitempty"`
	Poll       *Poll  `json:"poll,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

// ----------------------------------------------------------------------------
// 实例信息 / 关系 类型
// ----------------------------------------------------------------------------

// instanceStatusesConfig 是 GET /api/v1/instance 里 statuses 配置段。
type instanceStatusesConfig struct {
	MaxCharacters int `json:"max_characters"`
}

// instancePollsConfig 是 GET /api/v1/instance 里 polls 配置段。
//
// 四项全是**实例级配置**：选项数量、单选项长度、投票最短/最长存活时间。
// 任何一项超限 Mastodon 都返回 422，而 LLM 生成的选项/题目长度不可控，
// 因此必须在建投票前探测并 clamp（否则 user_choice 直接失败、问题挂到超时）。
type instancePollsConfig struct {
	MaxOptions             int `json:"max_options"`
	MaxCharactersPerOption int `json:"max_characters_per_option"`
	MinExpiration          int `json:"min_expiration"`
	MaxExpiration          int `json:"max_expiration"`
}

// instanceConfiguration 是 GET /api/v1/instance 的 configuration 段。
type instanceConfiguration struct {
	Statuses instanceStatusesConfig `json:"statuses"`
	Polls    instancePollsConfig    `json:"polls"`
}

// instanceURLs 是 GET /api/v1/instance 的 urls 段。
type instanceURLs struct {
	// StreamingAPI 是流式端点基址（如 "wss://streaming.mastodon.social"）。
	//
	// 它可以与 API 域名**不同**：大实例普遍把 streaming 独立部署，而 gorilla
	// 不跟随 3xx 重定向。若硬拼 <api域名>/api/v1/streaming 且该实例没配代理，
	// 结果就是永久连不上、streamLoop 无限退避重连——没有任何报错提示。
	StreamingAPI string `json:"streaming_api"`
}

// instanceInfo 是 GET /api/v1/instance 的响应子集。
// 单条帖子长度上限是**实例级配置**（默认 500，不少实例放宽到 5000+），
// 硬编码 500 会在放宽实例上把长回复白白截断，故启动时拉一次。
type instanceInfo struct {
	URI           string                `json:"uri,omitempty"`
	Title         string                `json:"title,omitempty"`
	Configuration instanceConfiguration `json:"configuration"`
	URLs          instanceURLs          `json:"urls"`
}

// Relationship 是 GET /api/v1/accounts/relationships 的数组元素。
type Relationship struct {
	ID         string `json:"id"`
	Following  bool   `json:"following"`
	FollowedBy bool   `json:"followed_by"`
	Blocking   bool   `json:"blocking"`
	Muting     bool   `json:"muting"`
	Requested  bool   `json:"requested"`
}

// searchResult 是 GET /api/v2/search 的响应。
// accounts / statuses 互斥填充（取决于 type 参数）。
type searchResult struct {
	Accounts []Account `json:"accounts"`
	Statuses []Status  `json:"statuses"`
}
