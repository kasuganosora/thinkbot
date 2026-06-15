package misskey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/log"
)

// ============================================================================
// MisskeyChannel — Misskey 平台输入端适配器
// ============================================================================

// Config 配置 MisskeyChannel。
type Config struct {
	// Host Misskey 实例 URL（如 "https://misskey.io"）。
	Host string

	// Token Misskey API Token（含 WebSocket streaming 和 HTTP API 权限）。
	Token string

	// WatchdogTimeout WebSocket 看门狗超时。0 = 使用 120s 默认值。
	WatchdogTimeout time.Duration

	// PingInterval WebSocket 自动 Ping 间隔。0 = 使用 30s 默认值。
	PingInterval time.Duration

	// ReconnectDelay 断线后重连间隔。0 = 使用 5s 默认值。
	ReconnectDelay time.Duration

	// SubscribeTimeline 是否同时订阅 homeTimeline 频道。
	// 启用后 Bot 会收到时间线上的所有帖子（不仅仅是 @提及），
	// Pipeline 可据此实现"旁听群聊"等场景。
	// 这些消息的 Mentioned 字段为 false。
	SubscribeTimeline bool
}

// MisskeyChannel 是 Misskey 平台的输入端实现。
//
// 它通过 WebSocket streaming 连接到 Misskey 实例的 main 通道，
// 监听 mention（提及）和 reply（回复）事件，
// 归一化为 core.Message 后注入 Ingress。
//
// 使用示例：
//
//	ch := misskey.NewChannel("my-mk-bot", "my-bot-id", misskey.Config{
//	    Host:  "https://misskey.example.com",
//	    Token: "8xxxxxxxxxxxxx...",
//	})
//	bot, _ := bot.New(bot.BotParams{
//	    ID:       "my-bot-id",
//	    Channels: []bot.Channel{ch},
//	})
//	go bot.Run(ctx)
type MisskeyChannel struct {
	name  string
	botID string
	cfg   Config
	api   *apiClient
	hc    *http.Client

	// botUserID 是 Bot 自身的 Misskey User ID，在 Start 时通过 getSelf 获取。
	// 用于在 timeline 模式下过滤自己发的帖。
	botUserID string

	ingress *inbound.Ingress

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

// NewChannel 创建一个 MisskeyChannel。
func NewChannel(name, botID string, cfg Config) *MisskeyChannel {
	if cfg.WatchdogTimeout <= 0 {
		cfg.WatchdogTimeout = 120 * time.Second
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = 5 * time.Second
	}
	return &MisskeyChannel{
		name:  name,
		botID: botID,
		cfg:   cfg,
		hc:    http.New(),
		api:   newAPIClient(cfg.Host, cfg.Token),
	}
}

// Name 返回 Channel 名称。
func (c *MisskeyChannel) Name() string { return c.name }

// Type 返回 "misskey"。
func (c *MisskeyChannel) Type() string { return "misskey" }

// BotID 返回所属 Bot ID。
func (c *MisskeyChannel) BotID() string { return c.botID }

// Start 启动 WebSocket streaming 循环（非阻塞）。
func (c *MisskeyChannel) Start(ctx context.Context, ingress *inbound.Ingress) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("misskey channel: already stopped, cannot restart")
	}

	c.ingress = ingress

	// 验证 Token
	me, err := c.api.getSelf(ctx)
	if err != nil {
		return fmt.Errorf("misskey channel: token validation failed: %w", err)
	}
	log.Logger.Infow("misskey channel started",
		"channel", c.name, "username", me.Username, "host", c.cfg.Host)

	c.botUserID = me.ID

	// 派生可取消的 context
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// 启动 streaming goroutine
	c.wg.Add(1)
	go c.streamLoop(runCtx)

	return nil
}

// streamLoop 维护 WebSocket 连接，断线自动重连。
func (c *MisskeyChannel) streamLoop(ctx context.Context) {
	defer c.wg.Done()

	connID := "main-1"        // main 通道连接 ID
	timelineConnID := "tl-1"  // timeline 通道连接 ID（可选）

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := c.connectAndServe(ctx, connID, timelineConnID)
		if ctx.Err() != nil {
			return // 主动关闭
		}

		if err != nil {
			log.Logger.Warnw("misskey stream disconnected",
				"channel", c.name, "err", err)
		}

		// 重连前等待
		select {
		case <-time.After(c.cfg.ReconnectDelay):
		case <-ctx.Done():
			return
		}
	}
}

// connectAndServe 建立 WebSocket 连接并持续处理消息。
// 阻塞直到连接断开或 ctx 被取消。
func (c *MisskeyChannel) connectAndServe(ctx context.Context, connID, timelineConnID string) error {
	// 构建 streaming URL: wss://{host}/streaming?i={token}
	host := strings.TrimPrefix(strings.TrimPrefix(c.cfg.Host, "https://"), "http://")
	wsURL := fmt.Sprintf("wss://%s/streaming", host)

	// 准备订阅消息
	var connectMsgs []string

	// 订阅 main 通道（mention/reply 等事件）
	mainMsg, _ := json.Marshal(streamMessage{
		Type: "connect",
		Body: mustJSON(connectBody{
			Channel: "main",
			ID:      connID,
		}),
	})
	connectMsgs = append(connectMsgs, string(mainMsg))

	// 可选：订阅 homeTimeline 通道（所有时间线帖子）
	if c.cfg.SubscribeTimeline {
		tlMsg, _ := json.Marshal(streamMessage{
			Type: "connect",
			Body: mustJSON(connectBody{
				Channel: "homeTimeline",
				ID:      timelineConnID,
			}),
		})
		connectMsgs = append(connectMsgs, string(tlMsg))
	}

	cfg := http.WSConfig{
		WatchdogTimeout: c.cfg.WatchdogTimeout,
		PingInterval:    c.cfg.PingInterval,
		OnConnect: func(conn *http.WSConn) {
			// 发送所有订阅消息
			for _, msg := range connectMsgs {
				if err := conn.WriteText(msg); err != nil {
					log.Logger.Warnw("misskey stream: failed to send connect message",
						"channel", c.name, "err", err)
				}
			}
			log.Logger.Debugw("misskey stream: subscribed",
				"channel", c.name, "channels", connectMsgs)
		},
		OnError: func(err error) {
			log.Logger.Debugw("misskey ws error",
				"channel", c.name, "err", err)
		},
		OnText: func(text string) error {
			return c.handleStreamMessage(ctx, text, connID, timelineConnID)
		},
	}

	// DoWS 会阻塞直到连接关闭
	err := c.hc.Get(wsURL).
		SetContext(ctx).
		SetQuery("i", c.cfg.Token).
		DoWS(cfg)

	return err
}

// mustJSON 将 v 序列化为 json.RawMessage，序列化失败时返回空（仅用于已知可安全序列化的值）。
func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// handleStreamMessage 处理一条来自 streaming 的文本消息。
func (c *MisskeyChannel) handleStreamMessage(ctx context.Context, text, connID, timelineConnID string) error {
	var base streamMessage
	if err := json.Unmarshal([]byte(text), &base); err != nil {
		log.Logger.Debugw("misskey stream: failed to parse message",
			"channel", c.name, "raw", text, "err", err)
		return nil // 不中断连接
	}

	// 只处理 type=channel 的消息（服务端推送）
	if base.Type != "channel" {
		return nil
	}

	var chMsg channelMessage
	if err := json.Unmarshal(base.Body, &chMsg); err != nil {
		return nil
	}

	// main 通道事件：mention / reply（Bot 被明确提及）
	if chMsg.ID == connID {
		switch chMsg.Type {
		case "mention", "reply":
			var note Note
			if err := json.Unmarshal(chMsg.Body, &note); err != nil {
				log.Logger.Debugw("misskey stream: failed to parse note",
					"channel", c.name, "type", chMsg.Type, "err", err)
				return nil
			}
			c.handleNote(ctx, note, chMsg.Type, true) // Mentioned = true
		default:
			// 忽略其他 main 事件（follow, renote 等）
		}
		return nil
	}

	// timeline 通道事件：时间线上的所有帖子（Bot 未被提及）
	if c.cfg.SubscribeTimeline && chMsg.ID == timelineConnID {
		switch chMsg.Type {
		case "note":
			var note Note
			if err := json.Unmarshal(chMsg.Body, &note); err != nil {
				log.Logger.Debugw("misskey stream: failed to parse timeline note",
					"channel", c.name, "err", err)
				return nil
			}
			// 忽略 Bot 自己发的帖
			if note.User.ID == c.botUserID {
				return nil
			}
			// 忽略没有文本的帖（纯图片、Renote 等）
			if note.Text == "" {
				return nil
			}
			c.handleNote(ctx, note, "timeline", false) // Mentioned = false
		default:
			// 忽略其他 timeline 事件
		}
		return nil
	}

	return nil
}

// handleNote 将一条 Misskey Note 转换为 core.Message 并注入 Ingress。
// mentioned 参数指示此 Note 是否明确 @提及了 Bot。
func (c *MisskeyChannel) handleNote(ctx context.Context, note Note, eventType string, mentioned bool) {
	if note.Text == "" {
		return
	}

	// 构建用户全名（@username@host 或 @username）
	username := "@" + note.User.Username
	if note.User.Host != "" {
		username += "@" + note.User.Host
	}

	displayName := note.User.Name
	if displayName == "" {
		displayName = note.User.Username
	}

	// 解析 createdAt
	createdAt := time.Now()
	if t, err := time.Parse(time.RFC3339, note.CreatedAt); err == nil {
		createdAt = t
	}

	metadata := map[string]any{
		"note_id":      note.ID,
		"username":     note.User.Username,
		"host":         note.User.Host,
		"visibility":   note.Visibility,
		"event_type":   eventType,
		"reply_id":     note.ReplyID,
		"renote_id":    note.RenoteID,
		"display_name": displayName,
		"acct":         username,
	}

	// Mentioned 由调用方传入：mention/reply 事件为 true，timeline 事件为 false。
	// ChatType 方面，Misskey 没有传统意义上的 "群组" 概念：
	// - visibility=specified → 类似私聊（仅指定对象可见）
	// - visibility=followers → 类似受限群组
	// - visibility=public/home → 公开社交时间线
	// 我们将 specified 映射为 private，其余映射为 group（社交场景）。
	chatType := core.ChatGroup
	if note.Visibility == VisibilitySpecified {
		chatType = core.ChatPrivate
	}

	coreMsg := core.Message{
		ID:        note.ID,
		BotID:     c.botID,
		Source:    c.name,
		Channel:   note.ID,
		ChatType:  chatType,
		UserID:    note.User.ID,
		Text:      note.Text,
		Mentioned: mentioned,
		MediaType: "text/plain",
		Metadata:  metadata,
		CreatedAt: createdAt,
	}

	if err := c.ingress.Receive(ctx, coreMsg); err != nil {
		log.Logger.Warnw("misskey ingress receive failed",
			"channel", c.name, "note_id", note.ID, "err", err)
	}
}

// Stop 优雅停止 streaming。
func (c *MisskeyChannel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.stopped = true
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Logger.Infow("misskey channel stopped", "channel", c.name)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reply 向指定帖子回复。便捷方法，供 Pipeline Action 处理器调用。
func (c *MisskeyChannel) Reply(ctx context.Context, noteID, text string) error {
	_, err := c.api.createNote(ctx, text, noteID, VisibilityPublic)
	return err
}
