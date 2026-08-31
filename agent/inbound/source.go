package inbound

// ============================================================================
// Channel — 输入端适配器接口（可选）
// ============================================================================

// Channel 定义输入端适配器的可选接口。
// 各输入端（Webhook handler、WebSocket 连接、Polling goroutine 等）
// 可以实现此接口来提供统一的元信息，但这不是必须的。
//
// 输入端的核心职责只有一个：调用 Ingress.Receive(ctx, msg) 注入消息。
// 输入端自行管理自己的生命周期（启停、重连等），Ingress 不关心。
//
// 此接口主要用于 Engine 可选地做统一注册和日志：
//
//	type MyWebhookChannel struct { ... }
//	func (c *MyWebhookChannel) Name() string { return "webhook" }
//	func (c *MyWebhookChannel) Type() string { return "http" }
type Channel interface {
	// Name 返回输入端标识名称（如 "misskey-ws"、"telegram-webhook"）。
	Name() string
	// Type 返回传输类型（如 "webhook"、"websocket"、"polling"、"memory"）。
	Type() string
}
