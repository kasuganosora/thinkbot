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

	// StreamReply 启用流式回复：收到消息先推一条「正在思考…」，
	// pipeline 出结果后用 finish=true **原位替换**成最终答案（docs §9.2.9）。
	//
	// 零值 = 关闭（走原来的整段 aibot_send_msg）。**任何一步失败都会自动回退**
	// 到整段发送，不会因此丢消息。
	StreamReply bool

	// PlaceholderDelay 推占位的延迟，0 = 1.5s（见 stream.go 的注释：
	// 快回复不推，避免「思考中」闪一下就被替换的劣质观感）。
	PlaceholderDelay time.Duration

	// StreamDeltaInterval 生成过程中**增量内容**的推送间隔，0 = 2s；<0 关闭增量推送。
	//
	// 为什么必须有：只有「正在思考…」的话，用户在长达几十秒的生成里
	// **根本不知道 bot 在往哪个方向写**，等结果出来才发现跑偏 ——
	// 那时再纠正已经白等一整轮。把增量推上去，用户能当场判断、当场引导。
	//
	// ⚠️ 推的是**已清洗**的增量（见 DeltaSourceFunc 的注释），不含思考块。
	// ⚠️ 实测 respond 连推 13 次零失败、不占 30 条/分钟配额，2s 一次很安全。
	StreamDeltaInterval time.Duration

	// PlaceholderRefresh 占位的刷新间隔，0 = 5s；<0 关闭刷新。
	//
	// 必须刷新：一轮生成实测 17~55s，占位推出来后就一动不动，
	// 用户分不清「还在跑」还是「卡死了」—— 观感上就是「停不下来」。
	// 带上已用秒数至少证明它活着（实测连推 13 次零失败，不占配额）。
	PlaceholderRefresh time.Duration

	// StreamMaxAge 一条流的最长存活，0 = 5min。到点强制收尾，
	// 避免 pipeline 静默时消息永远停在「正在思考…」。
	// 服务端硬上限是 10min，取一半留余量。
	StreamMaxAge time.Duration

	// InterruptPolicy bot 正在输出时，用户又发来一条，这条归谁？
	//   - "" / "append"（默认）：注入正在跑的那一轮，让它带着补充继续跑。
	//   - "restart"：打断旧轮 + 开新一轮。
	//
	// ⚠️ 默认 append 的**代价**：补充只在「一步生成结束后」才被编排层取走
	// （drainInterruptMessages），当前这轮会照旧跑完、再带着补充重跑一次 ——
	// 长任务总耗时接近翻倍。想立刻掐断就填 restart。
	// 无论哪种策略，「停止」这类终止指令都只打断、不开新轮（见 turn.go）。
	InterruptPolicy string

	// StopCommands 额外终止指令（在默认值之外追加）。
	//
	// ⚠️ 整句完全匹配才生效：「停一下再说」不是终止指令。
	// 否则用户一开口带「停」字就被掐断，体验灾难。
	StopCommands []string
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

// respondBody 是 aibot_respond_msg 的请求体（被动回复，含流式）。
//
// ⚠️ 与 aibot_send_msg 的根本区别：它不是「主动推一条新消息」，而是「对某一次
// 回调的回复」，req_id 必须透传那次回调的（见 callbackBody.ReqID）。
type respondBody struct {
	MsgType string      `json:"msgtype"`
	Stream  *streamBody `json:"stream,omitempty"`
}

// streamBody 是流式消息的内容。
//
// ⚠️ content 是**全量内容**不是增量：同 id 再推一次 = 客户端原位替换上一条。
// ⚠️ 首次推送起 10 分钟内必须 finish=true，否则服务端自动结束该流。
type streamBody struct {
	ID      string `json:"id"`
	Finish  bool   `json:"finish"`
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

	// ReqID 是本次回调**帧**的 headers.req_id（不在 body 里，由 dispatch 填进来）。
	//
	// ⚠️ 这是被动回复（aibot_respond_msg，含流式）的钥匙：
	// 服务端靠它把回复关联回某一次回调，且**同一次回调的所有流式刷新都用同一个**。
	// 早期版本在 dispatch 里只把 body 往下传，req_id 丢了 —— 流式根本无从下手。
	// 一次性凭据（内含语义），不进 metadata / 不落会话历史，用完即弃。
	ReqID string `json:"-"`

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
	// MsgTypeStream 流式消息。**只有 aibot_respond_msg 支持**（长连接模式），
	// aibot_send_msg 与 response_url 都不认它（后者实测返回 40008，见 docs §9.2.3）。
	MsgTypeStream = "stream"
)
