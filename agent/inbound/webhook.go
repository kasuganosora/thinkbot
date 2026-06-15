package inbound

import (
	"context"
	"errors"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// WebhookSource — HTTP Webhook 消息源（骨架）
// ============================================================================

// WebhookSource 接收 HTTP POST 推送的消息。
type WebhookSource struct {
	name string
	addr string // 监听地址，如 ":8080"
	path string // webhook 路径，如 "/webhook"
}

// NewWebhookSource 创建 Webhook 消息源。
func NewWebhookSource(name, addr, path string) *WebhookSource {
	return &WebhookSource{name: name, addr: addr, path: path}
}

func (w *WebhookSource) Name() string { return w.name }

func (w *WebhookSource) Start(ctx context.Context, ch chan<- *core.Envelope) error {
	return errors.New("webhook source: not implemented")
}

func (w *WebhookSource) Stop(ctx context.Context) error {
	return nil
}
