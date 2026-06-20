package api

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
)

// ============================================================================
// WebChannel — Web 页面聊天用的内存 Channel
//
// 实现 bot.Channel（输入端）+ bot.Sender（输出端）接口。
// 与 MemoryChannel 的区别：
//   - WebChannel 按 traceID 路由回写，而非全局记录
//   - 每条消息的回复通过 per-traceID channel 传递给 SSE handler
// ============================================================================

// WebChannel 是 Web 页面聊天用的 Channel。
// 每个 Bot 实例持有一个 WebChannel，负责注入用户消息和接收 Bot 回复。
type WebChannel struct {
	name    string
	botID   string
	ingress *inbound.Ingress

	mu        sync.RWMutex
	responses map[string]chan core.Action // traceID → response channel
}

// NewWebChannel 创建 WebChannel。
// name 通常是 "web-{botID}"。
func NewWebChannel(name, botID string) *WebChannel {
	return &WebChannel{
		name:      name,
		botID:     botID,
		responses: make(map[string]chan core.Action),
	}
}

// Name 返回 Channel 名称。
func (c *WebChannel) Name() string { return c.name }

// Type 返回 "web"。
func (c *WebChannel) Type() string { return "web" }

// BotID 返回所属 Bot ID。
func (c *WebChannel) BotID() string { return c.botID }

// Start 保存 Ingress 引用。
func (c *WebChannel) Start(_ context.Context, ingress *inbound.Ingress) error {
	c.mu.Lock()
	c.ingress = ingress
	c.mu.Unlock()
	return nil
}

// Stop 清理资源。
func (c *WebChannel) Stop(_ context.Context) error {
	c.mu.Lock()
	c.ingress = nil
	c.responses = make(map[string]chan core.Action)
	c.mu.Unlock()
	return nil
}

// --- 输入端 ---

// Inject 向 Bot 注入一条 Web 消息。
// traceID 用于关联回复。extraMetadata 中的键值会合并到 Message.Metadata 中。
func (c *WebChannel) Inject(ctx context.Context, traceID, userID, text string, extraMetadata map[string]any) error {
	c.mu.RLock()
	ingress := c.ingress
	c.mu.RUnlock()
	if ingress == nil {
		return fmt.Errorf("web channel %q: not started", c.name)
	}

	metadata := map[string]any{
		"source_channel": c.name,
	}
	maps.Copy(metadata, extraMetadata)

	msg := core.Message{
		ID:        traceID,
		TraceID:   traceID,
		BotID:     c.botID,
		Source:    c.name,
		Channel:   "web:" + userID,
		ChatType:  core.ChatPrivate,
		UserID:    userID,
		Text:      text,
		Mentioned: true,
		CreatedAt: time.Now(),
		Metadata:  metadata,
	}
	return ingress.Receive(ctx, msg)
}

// RegisterResponse 为指定 traceID 注册一个回复等待 channel。
// 返回的 channel 在 Bot 回复时收到 Action，或超时后关闭。
func (c *WebChannel) RegisterResponse(traceID string, buf int) chan core.Action {
	ch := make(chan core.Action, buf)
	c.mu.Lock()
	c.responses[traceID] = ch
	c.mu.Unlock()
	return ch
}

// UnregisterResponse 注销回复 channel。
func (c *WebChannel) UnregisterResponse(traceID string) {
	c.mu.Lock()
	if ch, ok := c.responses[traceID]; ok {
		delete(c.responses, traceID)
		close(ch)
	}
	c.mu.Unlock()
}

// --- 输出端 (Sender 接口) ---

// Send 实现 bot.Sender / outbound.ChannelSender。
// 将 Bot 的回复路由到对应的 traceID response channel。
func (c *WebChannel) Send(_ context.Context, action core.Action) error {
	// 从 Action.Metadata 提取 traceID（由 pipeline 设置）
	traceID := ""
	if action.Metadata != nil {
		if v, ok := action.Metadata["trace_id"]; ok {
			if s, ok := v.(string); ok {
				traceID = s
			}
		}
	}

	// 如果 Action 没有携带 traceID，尝试从 Channel 字段提取
	// （格式 "web:<userID>"，此时无法关联，直接丢弃）
	if traceID == "" {
		return nil
	}

	c.mu.RLock()
	ch, ok := c.responses[traceID]
	c.mu.RUnlock()
	if !ok {
		return nil
	}

	select {
	case ch <- action:
	default:
		// channel 满，丢弃（SSE 可能已断开）
	}
	return nil
}
