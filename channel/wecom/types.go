package wecom

// 企业微信（WeCom）智能机器人 —— 长连接模式的帧与消息结构。
//
// 协议要点（2026-10-20 真实实例实测，详见 docs/wecom-channel-design.md §9）：
//   - 连接地址 wss://openws.work.weixin.qq.com，明文 JSON，无需加解密
//   - 帧统一 {cmd, headers:{req_id}, body}
//   - ⚠️ 请求响应（ACK）帧**不带 cmd**，只能靠 req_id 关联
//   - ⚠️ 错误 ACK 连 req_id 都不带，pending 表必须有超时兜底

import (
	"encoding/json"
	"time"
)

// Config 是 WeCom Channel 的配置。
type Config struct {
	// RobotID 企业微信智能机器人的 BotID。
	//
	// ⚠️ 不要叫 bot_id：thinkbot 里的 BotID 是 bot 标识，企业微信的 BotID 是机器人标识，
	// 两者完全不同，混用会让读代码的人当场糊涂。
	RobotID string

	// Secret 长连接专用密钥（与 URL 回调模式的 Token/EncodingAESKey 不同）。
	Secret string

	// BotName 机器人在群里的显示名（如 "mybot"），用于剥离 @ 前缀。
	// 留空时退化为「剥掉开头任意 @token」的通用规则。
	//
	// 为什么需要配置：实测回调 body 里只有 aibotid，**没有机器人显示名**，
	// 而群聊消息正文是 "@mybot 你好"，不剥掉会把 @ 前缀喂给 LLM。
	BotName string

	// Endpoint 长连接地址，私有部署时才需覆盖。留空用官方默认。
	Endpoint string

	// AllowCustomEndpoint 允许 Endpoint 指向非官方主机（仍要求 wss://）。
	// 默认关闭：Secret 会随订阅帧发往该地址，非官方地址等于把凭据交出去。
	AllowCustomEndpoint bool

	// PingInterval 应用层心跳间隔，0 = 30s（官方文档建议 30 秒）。
	PingInterval time.Duration

	// ReconnectDelay 断线后首次重连间隔，0 = 3s，之后指数退避封顶 30s。
	ReconnectDelay time.Duration

	// WatchdogTimeout 连接看门狗超时，0 = 120s。
	WatchdogTimeout time.Duration

	// CaptionWindow 「文字 + 文件」的合并窗口，0 = 3s，<0 表示关闭合并。
	//
	// 企微发文件时把配文拆成**另一条独立回调**（两个 msgid，无任何关联字段），
	// bot 会对每条各回一次 → 用户看到两条回复。本窗口把裸文本暂存这么久，
	// 期间同人同会话来了媒体消息就把文本并进去、只投递合并后的一条。
	// 代价：没有媒体跟随的纯文本消息，回复会被推迟整个窗口时长。
	// 实测两条回调间隔 0.3~2.5s，故默认 3s。
	CaptionWindow time.Duration
}

// frame 是长连接上所有帧的统一外层结构。
type frame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers frameHeaders    `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode int             `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

type frameHeaders struct {
	ReqID string `json:"req_id"`
}

// ackResult 是一次请求的响应。
type ackResult struct {
	ErrCode int
	ErrMsg  string
}

// OK 判断响应是否成功。
func (a ackResult) OK() bool { return a.ErrCode == 0 }

// subscribeBody 是 aibot_subscribe 的请求体。
type subscribeBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// sendMsgBody 是 aibot_send_msg 的请求体。
//
// ⚠️ chat_type 必须显式指定：1=单聊（chatid 填 userid）、2=群聊（chatid 填群 chatid）。
// 缺省（0）服务端会优先按群聊解析，导致**单聊静默发不出去**。
type sendMsgBody struct {
	ChatID   string        `json:"chatid"`
	ChatType int           `json:"chat_type"`
	MsgType  string        `json:"msgtype"`
	Markdown *markdownBody `json:"markdown,omitempty"`
}

type markdownBody struct {
	Content string `json:"content"`
}

// callbackBody 是 aibot_msg_callback 的消息体。
type callbackBody struct {
	MsgID    string `json:"msgid"`
	AIBotID  string `json:"aibotid"`
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"` // "single" / "group"
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	MsgType string `json:"msgtype"`

	Text  *textContent  `json:"text,omitempty"`
	Image *mediaContent `json:"image,omitempty"`
	Voice *mediaContent `json:"voice,omitempty"`
	File  *mediaContent `json:"file,omitempty"`
	Video *mediaContent `json:"video,omitempty"`
	Mixed *mixedContent `json:"mixed,omitempty"`

	// ResponseURL 是一次性的 HTTP 被动回复通道（见 docs §9.2.3）。
	// 一期未使用，先记录下来备用。
	ResponseURL string `json:"response_url,omitempty"`

	// Compat 存在时表示本次回调非普通用户消息（智能助理调用 / 频道）。
	Compat json.RawMessage `json:"compat_info,omitempty"`
}

type textContent struct {
	Content string `json:"content"`
}

// mediaContent 是图片/语音/文件/视频的载荷。
//
// ⚠️ AESKey 是 43 字符的**无 padding** base64，解码前必须补 "="（否则非法 base64）。
// ⚠️ 实测 file 消息**没有文件名/大小字段**。
type mediaContent struct {
	URL    string `json:"url,omitempty"`
	AESKey string `json:"aeskey,omitempty"`
}

// mixedContent 是图文混排。实测群聊发图走的就是它（不是 image），
// 且**顶层没有 text 字段**，必须逐项遍历 msg_item。
type mixedContent struct {
	Items []mixedItem `json:"msg_item"`
}

type mixedItem struct {
	MsgType string        `json:"msgtype"`
	Text    *textContent  `json:"text,omitempty"`
	Image   *mediaContent `json:"image,omitempty"`
}

// eventBody 是 aibot_event_callback 的消息体。
//
// ⚠️ 实测事件类型嵌套在 `event` 子对象里（不是平铺的 eventtype）。
type eventBody struct {
	Event *struct {
		EventType string `json:"eventtype"`
	} `json:"event,omitempty"`
}

// 会话类型与出站 chat_type 常量。
const (
	// ChatTypeSingle 单聊（chatid 填 userid）。
	ChatTypeSingle = 1
	// ChatTypeGroup 群聊（chatid 填群 chatid）。
	ChatTypeGroup = 2
)

// 会话标识前缀。
// Channel 值同时承担三个职责：会话空间标识 + 出站寻址（含 chat_type）+ 会话 external_key。
//
// ⚠️ 导出是因为 api 层要用它判会话归属（`wecomSessionTarget`），那里没有别的办法
// 知道前缀；不导出就只能在两处各写一份字面量，改一处漏一处会让会话静默分裂。
const (
	// PrefixDM 单聊前缀，后接 userid。
	PrefixDM = "wecom:dm:"
	// PrefixChat 群聊前缀，后接 chatid。
	PrefixChat = "wecom:chat:"
)

// cmd 常量（开发者 → 企微）。
const (
	cmdSubscribe = "aibot_subscribe"
	cmdPing      = "ping"
	cmdSendMsg   = "aibot_send_msg"
	cmdRespond   = "aibot_respond_msg"
)

// cmd 常量（企微 → 开发者）。
const (
	cmdMsgCallback   = "aibot_msg_callback"
	cmdEventCallback = "aibot_event_callback"
)

// 事件类型。
const (
	// EventEnterChat 用户当天首次进入机器人单聊会话。
	EventEnterChat = "enter_chat"
	// EventTemplateCard 用户点击模板卡片按钮（必须 5s 内响应，否则断连）。
	EventTemplateCard = "template_card_event"
	// EventFeedback 用户对回复点赞/点踩（只能回空包）。
	EventFeedback = "feedback_event"
	// EventDisconnected 有新连接建立，本连接被踢。
	// ⚠️ 收到后**必须停止自动重连**，否则两个实例互相踢、谁也连不上。
	EventDisconnected = "disconnected_event"
)

// 消息类型。
//
// 注意 Markdown 是**出站**用的 msgtype（aibot_send_msg 的 body.msgtype），
// 它同时也是 body 里内容对象的 key：`{"msgtype":"markdown","markdown":{"content":...}}`。
// 入站永远不会出现 msgtype=markdown，别把它当入站分支写。
const (
	MsgTypeText     = "text"
	MsgTypeImage    = "image"
	MsgTypeMixed    = "mixed"
	MsgTypeVoice    = "voice"
	MsgTypeFile     = "file"
	MsgTypeVideo    = "video"
	MsgTypeMarkdown = "markdown"
)
