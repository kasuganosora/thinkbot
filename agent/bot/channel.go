package bot

import (
	"context"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
)

// ============================================================================
// Channel — 输入端接口（Bot 拥有的 Channel 实例）
// ============================================================================

// Channel 是 Bot 拥有的输入端实例。
// 每个 Channel 实例在创建时就绑定了所属 Bot，消息进来时天然知道目标 Bot。
//
// 典型的 Channel 实现：
//   - MisskeyChannel：接收 Misskey webhook 回调
//   - TelegramChannel：接收 Telegram Bot API 的 Update
//   - DiscordChannel：接收 Discord Gateway 事件
//   - MemoryChannel：内存通道（测试用）
//
// Channel 拥有自己的配置（如 webhook URL、token 等），
// 不同 Bot 的同类型 Channel 有不同的配置实例。
//
// 生命周期：
//
//	bot.Run()
//	  → channel.Start(ctx, ingress)   // 启动 Channel，拿到 Ingress 注入消息
//	  → ...                           // Channel 自行管理运行（HTTP server、WS 连接等）
//	bot.Stop()
//	  → channel.Stop(ctx)             // 优雅关闭
type Channel interface {
	// Name 返回 Channel 实例的唯一名称。
	// 例如 "misskey-customer-bot"、"telegram-code-review"。
	// 在同一个 Bot 中 Name 应唯一。
	Name() string

	// Type 返回 Channel 的类型标识。
	// 例如 "misskey"、"telegram"、"discord"、"slack"、"memory"。
	Type() string

	// BotID 返回此 Channel 所属的 Bot ID。
	// 在 Channel 创建时即确定，不可变。
	BotID() string

	// Start 启动 Channel。
	// Bot 在 Run 时调用此方法，传入 Bot 私有的 Ingress。
	// Channel 收到消息后，填充 msg.BotID 和 msg.Source，然后调用 ingress.Receive(ctx, msg)。
	//
	// Start 应是非阻塞的：启动内部 goroutine（如 HTTP server）后立即返回。
	// 如果启动失败（如端口占用），返回错误。
	Start(ctx context.Context, ingress *inbound.Ingress) error

	// Stop 优雅关闭 Channel。
	// 应停止接受新消息，等待正在处理的消息完成，然后释放资源。
	Stop(ctx context.Context) error
}

// ============================================================================
// Sender — 输出端接口（Channel 的回写能力）
// ============================================================================

// Sender 定义了 Channel 的输出端能力。
// 一个 Channel 如果同时实现了 Channel（输入端）和 Sender（输出端），
// 就具备双向通信能力：既能接收消息又能回写消息。
//
// Sender 由 outbound.ChannelReplyHandler 调用，桥接 Dispatcher → Channel 的回写路径。
//
// Action 参数约定：
//   - Action.Type：动作类型（reply/forward/broadcast），Sender 可据此决定行为。
//   - Action.Channel：outbound 回复目标标识符。
//     Stage 应从 env.Message.Metadata["reply_target"] 传递到 Action.Channel。
//     各平台语义：
//     Telegram → chatID（字符串形式的 int64）
//     Misskey → noteID（回复目标帖子的 ID）
//   - Action.Payload：发送内容，通常是 string（文本消息）。
//   - Action.Metadata：平台特有参数 + 路由参数，各 Sender 自行解析：
//     通用 → "source_channel"（必需，ChannelReplyHandler 路由用）
//     Telegram → "reply_to_message_id"（int64）, "parse_mode"（string）
//     Misskey → "visibility"（string）, "cw"（string）
//   - Action.UserID：目标用户（Forward/DM 场景使用）。
//
// 实现须知：
//   - Send 应该是同步的，在消息发送完成后才返回。
//   - 对于长文本，Sender 应负责拆分（如 Telegram 4096 字符限制）。
//   - Sender 应处理平台级别的错误（如频率限制），必要时重试。
type Sender interface {
	// Send 执行一个输出动作。
	// 根据 action.Type 决定行为（回复/转发/广播），根据 action.Channel 定位目标，
	// 根据 action.Payload 发送内容，根据 action.Metadata 应用平台特有参数。
	Send(ctx context.Context, action core.Action) error
}
