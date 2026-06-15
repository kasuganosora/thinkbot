package bot

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
)

// ============================================================================
// MemoryChannel — 内存输入端（测试和开发用）
// ============================================================================

// MemoryChannel 是一个基于内存的 Channel 实现。
// 它不监听任何外部端口，而是通过 Send() 方法手动注入消息。
// 适用于单元测试、集成测试和开发调试。
//
// 使用示例：
//
//	bot, _ := bot.New(bot.BotParams{
//	    ID:       "test-bot",
//	    Channels: []bot.Channel{bot.NewMemoryChannel("mem", "test-bot")},
//	    ...
//	})
//	go bot.Run(ctx)
//	// 通过 MemoryChannel 注入消息
//	memCh := bot.Channels()[0].(*bot.MemoryChannel)
//	memCh.Send(ctx, core.Message{ID: "1", Text: "hello"})
type MemoryChannel struct {
	name    string
	botID   string
	ingress *inbound.Ingress
}

// NewMemoryChannel 创建内存 Channel。
func NewMemoryChannel(name, botID string) *MemoryChannel {
	if name == "" {
		name = "memory"
	}
	return &MemoryChannel{
		name:  name,
		botID: botID,
	}
}

// Name 返回 Channel 名称。
func (c *MemoryChannel) Name() string { return c.name }

// Type 返回 "memory"。
func (c *MemoryChannel) Type() string { return "memory" }

// BotID 返回所属 Bot ID。
func (c *MemoryChannel) BotID() string { return c.botID }

// Start 启动 Channel（保存 Ingress 引用）。
func (c *MemoryChannel) Start(_ context.Context, ingress *inbound.Ingress) error {
	c.ingress = ingress
	return nil
}

// Stop 停止 Channel（无需清理）。
func (c *MemoryChannel) Stop(_ context.Context) error {
	c.ingress = nil
	return nil
}

// Send 向 Bot 注入一条消息（阻塞式）。
// 自动填充 BotID、Source 和 CreatedAt。
func (c *MemoryChannel) Send(ctx context.Context, msg core.Message) error {
	if msg.BotID == "" {
		msg.BotID = c.botID
	}
	if msg.Source == "" {
		msg.Source = c.name
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	return c.ingress.Receive(ctx, msg)
}

// TrySend 非阻塞式注入消息。缓冲区满时返回 false。
func (c *MemoryChannel) TrySend(msg core.Message) bool {
	if c.ingress == nil {
		return false
	}
	if msg.BotID == "" {
		msg.BotID = c.botID
	}
	if msg.Source == "" {
		msg.Source = c.name
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	return c.ingress.TryReceive(msg)
}
