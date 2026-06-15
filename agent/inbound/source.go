package inbound

import (
	"context"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Source — 消息输入源接口
// ============================================================================

// Source 定义消息输入源的统一接口。
// 每种消息来源（Webhook、WebSocket、Polling 等）都实现此接口。
type Source interface {
	// Name 返回输入源标识名称。
	Name() string

	// Start 启动消息源，接收到的消息通过 ch 发送给 Pipeline。
	// 实现应在 ctx 取消时优雅退出。
	// ch 由 Engine 创建和关闭，Source 只负责向其写入。
	Start(ctx context.Context, ch chan<- *core.Envelope) error

	// Stop 优雅关闭消息源。
	// ctx 提供关闭超时控制。
	Stop(ctx context.Context) error
}
