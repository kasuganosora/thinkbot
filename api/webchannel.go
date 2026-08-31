package api

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/util/traceid"
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

	// chatHistory 用于「无人实时订阅时兜底落库」：当 bot 回复找不到对应的 SSE 订阅
	// （如 workflow 续跑时前端已断开/未 resume），把 assistant 回复写入 chat_messages，
	// 用户刷新或 resume 时即可看到，避免「bot 跑完没汇报」的体感。
	chatHistory *ChatHistoryService

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
	// Channel 类型，供 ToolSessionContext.SourceChannelType 使用（工具权限按平台匹配）。
	// 放在 maps.Copy 之后，确保不被 extraMetadata 覆盖。
	metadata["channel_type"] = "web"

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
func (c *WebChannel) Send(ctx context.Context, action core.Action) error {
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
		// 无人实时订阅（如 workflow 续跑时 SSE 已断开 / 前端尚未 resume）：
		// 兜底落库，用户刷新或 resume 时即可看到 bot 的回复，避免「bot 没继续干活」的体感。
		// 注意：正常路径（前端持有 SSE）走上面的 ok 分支并实时投递，由 SSE handler
		// 负责落库，二者互斥，不会重复写库。
		c.persistReplyFallback(ctx, action, traceID)
		return nil
	}

	select {
	case ch <- action:
	default:
		traceid.L(ctx).Warnw("webchannel: response channel full, action dropped",
			"channel", c.name, "trace_id", traceID, "action_type", action.Type)
	}
	return nil
}

// persistReplyFallback 在无人实时订阅（找不到对应 traceID 的 SSE 订阅）时，
// 把 bot 的 assistant 回复兜底落库到 chat_messages，确保用户刷新 / resume 后可见。
//
// 仅处理终态回复（ActionReply），忽略工具回调等其他 action 类型。
// 幂等：以 traceID 作为 UpsertAssistantByTrace 的幂等键，重复调用只会刷新同一行。
func (c *WebChannel) persistReplyFallback(ctx context.Context, action core.Action, traceID string) {
	if c.chatHistory == nil {
		return
	}
	if action.Type != core.ActionReply {
		return
	}
	content, _ := action.Payload.(string)
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	// 剥掉出站协议标记（@@REPLY_CONTROL@@{...}），与正常路径 saveAssistant 保持一致。
	content = stages.StripReplyControlBlock(content)
	if content == "" {
		return
	}

	// 定位会话：优先从 metadata 取 session_id，否则退回 traceID
	// （workflow 续跑以 sessionID 作 traceID 注入，二者相等）。
	sessionID := traceID
	if action.Metadata != nil {
		if sid, ok := action.Metadata["session_id"].(string); ok && sid != "" {
			sessionID = sid
		}
	}
	if sessionID == "" {
		sessionID = traceID
	}

	// 续跑无真实请求用户，沿用 continuation 命令的 "system" 归属，与 onWorkflowCompleted 保持对称。
	const userID = "system"

	c.chatHistory.logger.Infow("webchannel: reply persisted as fallback (no live subscriber)",
		"channel", c.name, "trace_id", traceID, "session_id", sessionID, "len", len(content))

	go func() {
		// 用独立 context，避免上游 SSE context 取消导致落库失败。
		if err := c.chatHistory.UpsertAssistantByTrace(c.botID, userID, content, traceID, "", "", sessionID, false); err != nil {
			traceid.L(context.Background()).Warnw("webchannel: fallback persist assistant reply failed",
				"channel", c.name, "trace_id", traceID, "session_id", sessionID, "err", err)
		}
	}()
}
