package inbound

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// MemoryChannel — 内存输入端（用于测试和开发）
// ============================================================================

// MemoryChannel 是一个基于内存的输入端适配器。
// 它通过 Send() / TrySend() 方法注入消息到 Ingress，
// 适用于单元测试、集成测试和开发调试。
//
// 使用示例：
//
//	ingress := inbound.NewIngress(cfg, logger, tp)
//	mem := inbound.NewMemoryChannel("test", ingress)
//	mem.Send(ctx, core.Message{ID: "1", Text: "hello"})
type MemoryChannel struct {
	name    string
	ingress *Ingress
}

// NewMemoryChannel 创建内存输入端。
func NewMemoryChannel(name string, ingress *Ingress) *MemoryChannel {
	if name == "" {
		name = "memory"
	}
	return &MemoryChannel{
		name:    name,
		ingress: ingress,
	}
}

// Name 返回输入端名称。
func (m *MemoryChannel) Name() string { return m.name }

// Type 返回 "memory"。
func (m *MemoryChannel) Type() string { return "memory" }

// Send 注入一条消息到 Ingress（阻塞式）。
// 自动填充 Source 和 CreatedAt。
func (m *MemoryChannel) Send(ctx context.Context, msg core.Message) error {
	if msg.Source == "" {
		msg.Source = m.name
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	return m.ingress.Receive(ctx, msg)
}

// TrySend 尝试非阻塞地注入消息。
// 如果 Ingress 缓冲区已满，返回 false。
func (m *MemoryChannel) TrySend(msg core.Message) bool {
	if msg.Source == "" {
		msg.Source = m.name
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	return m.ingress.TryReceive(msg)
}
