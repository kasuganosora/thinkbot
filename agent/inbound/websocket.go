package inbound

import (
	"context"
	"errors"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// WebSocketSource — WebSocket 消息源（骨架）
// ============================================================================

// WebSocketSource 通过 WebSocket 连接接收消息。
type WebSocketSource struct {
	name string
	url  string // WebSocket 服务端地址
}

// NewWebSocketSource 创建 WebSocket 消息源。
func NewWebSocketSource(name, url string) *WebSocketSource {
	return &WebSocketSource{name: name, url: url}
}

func (w *WebSocketSource) Name() string { return w.name }

func (w *WebSocketSource) Start(ctx context.Context, ch chan<- *core.Envelope) error {
	return errors.New("websocket source: not implemented")
}

func (w *WebSocketSource) Stop(ctx context.Context) error {
	return nil
}
