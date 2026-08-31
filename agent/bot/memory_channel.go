package bot

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
)

// ============================================================================
// MemoryChannel — 内存输入/输出端（测试和开发用）
// ============================================================================

// MemoryChannel 是一个基于内存的 Channel 实现。
// 它不监听任何外部端口，而是通过 Inject() 方法手动注入消息（输入端），
// 通过 Send() 实现 Sender 接口记录输出（输出端）。
// 适用于单元测试、集成测试和开发调试。
//
// 同时实现 Channel（输入端）和 Sender（输出端）接口。
//
// 使用示例：
//
//	memCh := bot.NewMemoryChannel("mem", "test-bot")
//	bot, _ := bot.New(bot.BotParams{
//	    ID:       "test-bot",
//	    Channels: []bot.Channel{memCh},
//	    ...
//	})
//	go bot.Run(ctx)
//
//	// 注入消息（输入端）
//	memCh.Inject(ctx, core.Message{ID: "1", Text: "hello"})
//
//	// 检查输出（Sender 记录）
//	actions := memCh.SentActions()
type MemoryChannel struct {
	name    string
	botID   string
	ingress *inbound.Ingress

	mu          sync.Mutex
	sentActions []core.Action // 记录所有通过 Send 发出的 Action
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
	c.mu.Lock()
	c.ingress = ingress
	c.mu.Unlock()
	return nil
}

// Stop 停止 Channel（无需清理）。
func (c *MemoryChannel) Stop(_ context.Context) error {
	c.mu.Lock()
	c.ingress = nil
	c.mu.Unlock()
	return nil
}

// ============================================================================
// 输入端方法：向 Bot 注入消息
// ============================================================================

// Inject 向 Bot 注入一条消息（阻塞式）。
// 自动填充 BotID、Source 和 CreatedAt。
// 如果 Channel 尚未启动（ingress 为 nil），返回错误。
func (c *MemoryChannel) Inject(ctx context.Context, msg core.Message) error {
	c.mu.Lock()
	ingress := c.ingress
	c.mu.Unlock()
	if ingress == nil {
		return fmt.Errorf("memory channel %q: not started, ingress is nil", c.name)
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
	return ingress.Receive(ctx, msg)
}

// TryInject 非阻塞式注入消息。缓冲区满时返回 false。
func (c *MemoryChannel) TryInject(msg core.Message) bool {
	c.mu.Lock()
	ingress := c.ingress
	c.mu.Unlock()
	if ingress == nil {
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
	return ingress.TryReceive(msg)
}

// ============================================================================
// 输出端方法：Sender 接口实现
// ============================================================================

// Send 实现 Sender 接口。将 Action 记录到内存中供测试断言使用。
func (c *MemoryChannel) Send(_ context.Context, action core.Action) error {
	// 验证 payload 类型
	if action.Payload != nil {
		if _, ok := action.Payload.(string); !ok {
			return fmt.Errorf("memory send: payload is %T, expected string", action.Payload)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.sentActions = append(c.sentActions, action)
	return nil
}

// SentActions 返回所有通过 Send 发出的 Action 的副本。
func (c *MemoryChannel) SentActions() []core.Action {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]core.Action, len(c.sentActions))
	copy(out, c.sentActions)
	return out
}

// LastSentAction 返回最后一个发出的 Action，如果没有则返回 nil。
func (c *MemoryChannel) LastSentAction() *core.Action {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sentActions) == 0 {
		return nil
	}
	a := c.sentActions[len(c.sentActions)-1]
	return &a
}

// ClearSentActions 清空已记录的 Action（用于测试间重置）。
func (c *MemoryChannel) ClearSentActions() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sentActions = nil
}
