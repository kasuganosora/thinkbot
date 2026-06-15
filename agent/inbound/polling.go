package inbound

import (
	"context"
	"errors"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// PollingSource — 轮询消息源（骨架）
// ============================================================================

// PollingSource 定期轮询外部服务获取新消息。
type PollingSource struct {
	name     string
	interval time.Duration // 轮询间隔
}

// NewPollingSource 创建轮询消息源。
func NewPollingSource(name string, interval time.Duration) *PollingSource {
	return &PollingSource{name: name, interval: interval}
}

func (p *PollingSource) Name() string { return p.name }

func (p *PollingSource) Start(ctx context.Context, ch chan<- *core.Envelope) error {
	return errors.New("polling source: not implemented")
}

func (p *PollingSource) Stop(ctx context.Context) error {
	return nil
}
