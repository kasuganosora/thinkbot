package misskey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// MisskeyChannel — Misskey 平台输入端适配器
// ============================================================================

const (
	// misskeyMaxNoteLength 单条帖子最大长度（rune 数）。
	misskeyMaxNoteLength = 3000
	// 指数退避重连参数。
	misskeyReconnectDelayMin = 5 * time.Second
	misskeyReconnectDelayMax = 5 * time.Minute
	// 去重缓存 TTL 和清理间隔。
	misskeyDedupTTL          = 2 * time.Minute
	misskeyDedupCleanupEvery = 30 * time.Second
)

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
	// botUsername 是 Bot 的用户名，用于从文本中剥离 @bot 提及。
	botUsername string
	// mentionRe 匹配 @botUsername 或 @botUsername@host 的正则表达式。
	// 确保不会误匹配更长的用户名（如 @botuser 不会匹配 @bot）。
	mentionRe *regexp.Regexp

	ingress *inbound.Ingress

	// 去重缓存：noteID -> 入时间。防止 mention+timeline 同时投递同一条帖子。
	dedupMu sync.Mutex
	dedup   map[string]time.Time

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
		return errs.Wrap(err, "misskey channel: token validation failed")
	}
	log.Logger.Infow("misskey channel started",
		"channel", c.name, "username", me.Username, "host", c.cfg.Host)

	c.botUserID = me.ID
	c.botUsername = me.Username
	c.dedup = make(map[string]time.Time)

	// 编译 @bot 正则：匹配 @username 或 @username@host，确保后面不跟字母数字或下划线
	c.mentionRe = regexp.MustCompile(`@` + regexp.QuoteMeta(me.Username) + `(?:@[\w.-]+)?\b`)

	// 派生可取消的 context
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// 启动去重缓存清理 goroutine
	c.wg.Add(1)
	go c.dedupCleanupLoop(runCtx)

	// 启动 streaming goroutine
	c.wg.Add(1)
	go c.streamLoop(runCtx)

	return nil
}

// streamLoop 维护 WebSocket 连接，断线自动重连（指数退避）。
func (c *MisskeyChannel) streamLoop(ctx context.Context) {
	defer c.wg.Done()

	connID := "main-1"       // main 通道连接 ID
	timelineConnID := "tl-1" // timeline 通道连接 ID（可选）

	delay := misskeyReconnectDelayMin
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now()
		err := c.connectAndServe(ctx, connID, timelineConnID)
		if ctx.Err() != nil {
			return // 主动关闭
		}

		if err != nil {
			log.Logger.Warnw("misskey stream disconnected",
				"channel", c.name, "err", err, "reconnect_delay", delay)
		}

		// 如果连接存活时间超过最大退避窗口，重置退避（可能是临时断线）。
		if time.Since(start) > misskeyReconnectDelayMax {
			delay = misskeyReconnectDelayMin
		} else if err == nil {
			// 干净断开（context 取消）无需退避。
			delay = misskeyReconnectDelayMin
		}

		// 重连前等待
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}

		// 指数退避：翻倍直到上限。
		delay *= 2
		if delay > misskeyReconnectDelayMax {
			delay = misskeyReconnectDelayMax
		}
	}
}

// dedupCleanupLoop 定期清理过期的去重缓存。
func (c *MisskeyChannel) dedupCleanupLoop(ctx context.Context) {
	defer c.wg.Done()
	ticker := time.NewTicker(misskeyDedupCleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.dedupMu.Lock()
			now := time.Now()
			for id, ts := range c.dedup {
				if now.Sub(ts) > misskeyDedupTTL {
					delete(c.dedup, id)
				}
			}
			c.dedupMu.Unlock()
		}
	}
}

// dedupSeen 检查 noteID 是否在去重窗口内已处理过，如果未处理则标记为已处理。
func (c *MisskeyChannel) dedupSeen(noteID string) bool {
	if noteID == "" {
		return false
	}
	c.dedupMu.Lock()
	defer c.dedupMu.Unlock()
	if ts, seen := c.dedup[noteID]; seen && time.Since(ts) < misskeyDedupTTL {
		return true
	}
	c.dedup[noteID] = time.Now()
	return false
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
			// 去重：防止 timeline 先投递了同一条帖子
			if c.dedupSeen(note.ID) {
				return nil
			}
			// 忽略 Bot 自己发的帖
			if note.UserID == c.botUserID || (note.UserID == "" && note.User.ID == c.botUserID) {
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
			// 去重
			if c.dedupSeen(note.ID) {
				return nil
			}
			// 忽略 Bot 自己发的帖
			if note.UserID == c.botUserID || (note.UserID == "" && note.User.ID == c.botUserID) {
				return nil
			}
			// 忽略 DM（timeline 不处理私聊）
			if note.Visibility == VisibilitySpecified {
				return nil
			}
			// 忽略没有文本且没有文件和 renote 的帖
			if note.Text == "" && len(note.Files) == 0 && note.Renote == nil {
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
	text := strings.TrimSpace(note.Text)

	// 如果是纯 Renote（无自己的文字），回退到被 Renote 的帖子文本
	renoteFallback := false
	if text == "" && note.Renote != nil {
		if rt := strings.TrimSpace(note.Renote.Text); rt != "" {
			text = rt
			renoteFallback = true
		}
	}

	// 没有文本也没有附件，跳过
	if text == "" && len(note.Files) == 0 {
		return
	}

	// 从文本中剥离 @bot 提及（Bot 不需要看到自己被 @）
	// 使用正则确保精确匹配 @username 或 @username@host，不误匹配更长的用户名
	if c.mentionRe != nil {
		text = strings.TrimSpace(c.mentionRe.ReplaceAllString(text, ""))
	}

	// 剥离后如果为空但原来是 renote，再次回退
	if text == "" && note.Renote != nil {
		if rt := strings.TrimSpace(note.Renote.Text); rt != "" {
			text = rt
			renoteFallback = true
		}
	}

	// 如果仍然为空且没有附件，跳过
	if text == "" && len(note.Files) == 0 {
		return
	}

	// 构建回复/转发上下文，让 Bot 能看到用户回复了什么或转发了什么
	text = noteContext(note, text, renoteFallback)

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

	// 分类帖子类型
	noteType := classifyNoteType(note)

	metadata := map[string]any{
		"note_id":      note.ID,
		"reply_target": note.ID, // outbound 回写时使用的精确目标（noteID）
		"username":     note.User.Username,
		"host":         note.User.Host,
		"visibility":   note.Visibility,
		"event_type":   eventType,
		"reply_id":     note.ReplyID,
		"renote_id":    note.RenoteID,
		"display_name": displayName,
		"acct":         username,
		"note_type":    noteType,
	}
	if len(note.Files) > 0 {
		metadata["file_count"] = len(note.Files)
		for i, f := range note.Files {
			metadata[fmt.Sprintf("file_%d_url", i)] = f.URL
			metadata[fmt.Sprintf("file_%d_name", i)] = f.Name
		}
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

	// timeline 消息加上来源前缀
	if eventType == "timeline" {
		text = fmt.Sprintf("[Timeline] @%s: %s", note.User.Username, text)
	}

	coreMsg := core.Message{
		ID:        note.ID,
		BotID:     c.botID,
		Source:    c.name,
		Channel:   note.User.ID, // 会话空间标识：用户 ID（同一用户的帖子视为一个对话流）
		ChatType:  chatType,
		UserID:    note.User.ID,
		Text:      text,
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

// classifyNoteType 判断帖子交互类型："note"（原创）/ "reply"（回复）/ "renote"（纯转发）/ "quote"（引用转发）。
func classifyNoteType(note Note) string {
	if note.RenoteID != "" {
		if strings.TrimSpace(note.Text) == "" && len(note.Files) == 0 {
			return "renote" // 纯转发
		}
		return "quote" // 引用转发（带评论）
	}
	if note.ReplyID != "" {
		return "reply"
	}
	return "note"
}

// noteContext 为帖子文本添加回复和转发上下文，让 Bot 能看到用户回复了什么或引用了什么。
// 回复/转发原文截断到 200 rune 以保持 prompt 简洁。
func noteContext(note Note, text string, skipRenote bool) string {
	// 回复上下文
	if note.Reply != nil && note.Reply.Text != "" {
		quoted := truncateRunes(strings.TrimSpace(note.Reply.Text), 200)
		sender := note.Reply.User.Name
		if sender == "" {
			sender = note.Reply.User.Username
		}
		if sender != "" && quoted != "" {
			if text == "" {
				text = fmt.Sprintf("[Reply to %s: %s]", sender, quoted)
			} else {
				text = fmt.Sprintf("[Reply to %s: %s]\n%s", sender, quoted, text)
			}
		}
	}

	// 转发上下文（如果 skipRenote 则跳过，因为转发文本已被用作主文本）
	if !skipRenote && note.Renote != nil && note.Renote.Text != "" {
		quoted := truncateRunes(strings.TrimSpace(note.Renote.Text), 200)
		sender := note.Renote.User.Name
		if sender == "" {
			sender = note.Renote.User.Username
		}
		if sender != "" && quoted != "" {
			if text == "" {
				text = fmt.Sprintf("[Renote from %s: %s]", sender, quoted)
			} else {
				text = fmt.Sprintf("[Renote from %s: %s]\n%s", sender, quoted, text)
			}
		}
	}

	return text
}

// truncateRunes 委托给 strutil.Truncate，保持包内调用简洁。
func truncateRunes(s string, maxRunes int) string {
	return strutil.Truncate(s, maxRunes)
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
// 文本超长时自动截断到 3000 rune。回复使用 home 可见性。
// 如果回复目标帖子已被删除，放弃回复。
func (c *MisskeyChannel) Reply(ctx context.Context, noteID, text string) error {
	return c.ReplyWithVisibility(ctx, noteID, text, VisibilityHome)
}

// ReplyWithVisibility 向指定帖子回复，使用指定可见性。
func (c *MisskeyChannel) ReplyWithVisibility(ctx context.Context, noteID, text, visibility string) error {
	text = truncateRunes(strings.TrimSpace(text), misskeyMaxNoteLength)

	_, err := c.api.createNoteFull(ctx, text, noteID, "", visibility, "", nil)
	if err != nil {
		log.Logger.Warnw("misskey: reply failed, target note may be deleted",
			"channel", c.name, "note_id", noteID, "err", err)
	}
	return err
}

// React 对帖子添加 emoji 反应。
func (c *MisskeyChannel) React(ctx context.Context, noteID, emoji string) error {
	// Misskey 反应格式：自定义 emoji 用 :name:，unicode emoji 直接使用
	if !strings.HasPrefix(emoji, ":") && !isUnicodeEmoji(emoji) {
		emoji = ":" + emoji + ":"
	}
	return c.api.createReaction(ctx, noteID, emoji)
}

// Unreact 移除对帖子的反应。
func (c *MisskeyChannel) Unreact(ctx context.Context, noteID string) error {
	return c.api.deleteReaction(ctx, noteID)
}

// Send 实现 bot.Sender / outbound.ChannelSender 接口。
// 根据 Action 的内容回写消息到 Misskey。
//
// Action 字段约定：
//   - Action.Channel：回复目标的 noteID（来源于 Inbound 的 msg.Channel）
//   - Action.Payload：发送内容（string 类型的文本消息）
//   - Action.Metadata["visibility"]：帖子可见性（"public"/"home"/"followers"/"specified"，可选，默认 "home"）
//   - Action.Metadata["cw"]：CW 折叠文本（可选）
//
// 行为：
//   - ActionReply：回复目标帖子（文本超长自动截断到 3000 rune）
//   - 其他 ActionType：当前也按回复处理（后续扩展）
func (c *MisskeyChannel) Send(ctx context.Context, action core.Action) error {
	noteID := action.Channel
	if noteID == "" {
		return fmt.Errorf("misskey send: empty noteID in action.Channel")
	}

	// 提取文本
	text, ok := action.Payload.(string)
	if !ok {
		return fmt.Errorf("misskey send: payload is %T, expected string", action.Payload)
	}
	if text == "" {
		return nil // 空消息不发送
	}

	// 解析可选的 Metadata 参数
	visibility := VisibilityHome
	cw := ""

	if action.Metadata != nil {
		if v, ok := action.Metadata["visibility"]; ok {
			if vis, ok := v.(string); ok && vis != "" {
				visibility = vis
			}
		}
		if v, ok := action.Metadata["cw"]; ok {
			if cwText, ok := v.(string); ok {
				cw = cwText
			}
		}
	}

	// 截断长文本
	text = truncateRunes(strings.TrimSpace(text), misskeyMaxNoteLength)

	// 构建回复
	_, err := c.api.createNoteFull(ctx, text, noteID, "", visibility, cw, nil)
	if err != nil {
		log.Logger.Warnw("misskey send: reply failed",
			"channel", c.name, "note_id", noteID, "err", err)
		return errs.Wrapf(err, "misskey send: reply to %q failed", noteID)
	}

	return nil
}

// isUnicodeEmoji 粗略判断字符串是否为 unicode emoji（非 ASCII 字符）。
func isUnicodeEmoji(s string) bool {
	for _, r := range s {
		if r > 0x2B00 { // CJK 及 emoji 范围
			return true
		}
	}
	return false
}
