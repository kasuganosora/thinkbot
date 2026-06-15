package telegram

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/util/log"
)

// ============================================================================
// TelegramChannel — Telegram Bot 输入端适配器
// ============================================================================

// Config 配置 TelegramChannel。
type Config struct {
	// Token Telegram Bot Token（从 @BotFather 获取）。
	Token string

	// PollTimeout long polling 超时秒数（默认 30）。
	PollTimeout int

	// AllowedUpdates 限制接收的更新类型（为空则接收所有）。
	AllowedUpdates []string
}

// TelegramChannel 是 Telegram 平台的输入端实现。
//
// 它通过 Bot API 的 long polling 持续获取用户消息，
// 归一化为 core.Message 后注入 Ingress。
//
// 使用示例：
//
//	ch := telegram.NewChannel("my-tg-bot", "my-bot-id", telegram.Config{
//	    Token: "123456:ABC-DEF...",
//	})
//	bot, _ := bot.New(bot.BotParams{
//	    ID:       "my-bot-id",
//	    Channels: []bot.Channel{ch},
//	})
//	go bot.Run(ctx)
type TelegramChannel struct {
	name  string
	botID string
	cfg   Config
	api   *apiClient

	// botUserID 是 Bot 自身的 Telegram User ID（int64），在 Start 时通过 getMe 获取。
	// 用于在群聊中判断消息是否回复了 Bot（即 Mentioned）。
	botUserID int64
	// botUsername 是 Bot 的 Telegram 用户名（不含 @），用于检测 @botname 文本提及。
	botUsername string

	ingress *inbound.Ingress

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

// NewChannel 创建一个 TelegramChannel。
func NewChannel(name, botID string, cfg Config) *TelegramChannel {
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 30
	}
	return &TelegramChannel{
		name:  name,
		botID: botID,
		cfg:   cfg,
		api:   newAPIClient(cfg.Token, cfg.PollTimeout),
	}
}

// Name 返回 Channel 名称。
func (c *TelegramChannel) Name() string { return c.name }

// Type 返回 "telegram"。
func (c *TelegramChannel) Type() string { return "telegram" }

// BotID 返回所属 Bot ID。
func (c *TelegramChannel) BotID() string { return c.botID }

// Start 启动 long polling 循环（非阻塞）。
func (c *TelegramChannel) Start(ctx context.Context, ingress *inbound.Ingress) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("telegram channel: already stopped, cannot restart")
	}

	c.ingress = ingress

	// 验证 Token
	me, err := c.api.getMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram channel: token validation failed: %w", err)
	}
	log.Logger.Infow("telegram channel started",
		"channel", c.name, "bot_username", me.Username, "bot_id", me.ID)

	c.botUserID = me.ID
	c.botUsername = me.Username

	// 派生可取消的 context
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// 启动 polling goroutine
	c.wg.Add(1)
	go c.pollLoop(runCtx)

	return nil
}

// pollLoop 持续获取 Telegram 更新。
func (c *TelegramChannel) pollLoop(ctx context.Context) {
	defer c.wg.Done()

	var offset int64

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 使用带 timeout buffer 的 context 调用 getUpdates
		reqCtx, reqCancel := context.WithTimeout(ctx, apiTimeoutMultiplier(c.cfg.PollTimeout))
		updates, err := c.api.getUpdates(reqCtx, offset, c.cfg.PollTimeout, c.cfg.AllowedUpdates)
		reqCancel()

		if err != nil {
			if ctx.Err() != nil {
				return // 主动关闭
			}
			log.Logger.Warnw("telegram poll error",
				"channel", c.name, "err", err)
			// 避免疯狂重试
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}

		for _, upd := range updates {
			// 更新 offset
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}
			// 处理更新
			c.handleUpdate(ctx, upd)
		}
	}
}

// handleUpdate 处理单个 Update，将其转换为 core.Message 注入 Ingress。
func (c *TelegramChannel) handleUpdate(ctx context.Context, upd Update) {
	// 只处理消息和编辑消息
	var msg *Message
	if upd.Message != nil {
		msg = upd.Message
	} else if upd.EditedMessage != nil {
		msg = upd.EditedMessage
	} else {
		return // 忽略非消息更新
	}

	// 只处理文本消息
	if msg.Text == "" {
		return
	}

	// 转换 chat ID 为字符串 channel
	chatID := fmt.Sprintf("%d", msg.Chat.ID)
	userID := ""
	if msg.From != nil {
		userID = fmt.Sprintf("%d", msg.From.ID)
	}

	// 判断 ChatType：Telegram 的 chat.Type 已经是 "private"/"group"/"supergroup"/"channel"，
	// 与 core 常量直接对齐。
	chatType := msg.Chat.Type

	// 判断是否 @提及了 Bot：
	// - 私聊中所有消息都视为 "被提及"
	// - 群聊/频道中通过以下方式判断：
	//   1. 回复了 Bot 的消息
	//   2. 文本中包含 @botusername 实体（entities 中 type=mention）
	//   3. 文本中包含 text_mention 实体指向 Bot 的 user ID
	//   4. 文本以 /command 开头（bot_command 实体在 offset=0）
	mentioned := false
	if chatType == core.ChatPrivate {
		mentioned = true
	} else {
		// 方式 1: 回复 Bot 的消息
		if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil && msg.ReplyToMessage.From.ID == c.botUserID {
			mentioned = true
		}
		// 方式 2-4: 解析 entities
		if !mentioned {
			mentioned = c.detectMention(msg)
		}
	}

	// 构建用户显示名
	displayName := ""
	if msg.From != nil {
		displayName = msg.From.FirstName
		if msg.From.LastName != "" {
			displayName += " " + msg.From.LastName
		}
		if msg.From.Username != "" {
			displayName += " (@" + msg.From.Username + ")"
		}
	}

	// 构建 Metadata
	metadata := map[string]any{
		"chat_id":    msg.Chat.ID,
		"message_id": msg.MessageID,
		"date":       msg.Date,
	}
	if displayName != "" {
		metadata["user_display_name"] = displayName
	}
	if msg.From != nil && msg.From.Username != "" {
		metadata["username"] = msg.From.Username
	}
	if msg.Chat.Title != "" {
		metadata["chat_title"] = msg.Chat.Title
	}
	if msg.ReplyToMessage != nil {
		metadata["reply_to_message_id"] = msg.ReplyToMessage.MessageID
		metadata["reply_to_text"] = msg.ReplyToMessage.Text
	}

	coreMsg := core.Message{
		ID:        fmt.Sprintf("%d", msg.MessageID),
		BotID:     c.botID,
		Source:    c.name,
		Channel:   chatID,
		ChatType:  chatType,
		UserID:    userID,
		Text:      msg.Text,
		Mentioned: mentioned,
		MediaType: "text/plain",
		Metadata:  metadata,
		CreatedAt: time.Unix(msg.Date, 0),
	}

	// 注入 Ingress
	if err := c.ingress.Receive(ctx, coreMsg); err != nil {
		log.Logger.Warnw("telegram ingress receive failed",
			"channel", c.name, "message_id", msg.MessageID, "err", err)
	}
}

// detectMention 通过解析消息 entities 判断是否 @提及了 Bot 或使用了 Bot 命令。
// 检测规则：
//   - mention: 文本含 @botUsername（如 "@mybot hello"）
//   - text_mention: entity 中 User.ID == botUserID（无 username 的用户提及）
//   - bot_command: offset=0 的 /command（群聊中命令视为直接对话 Bot）
func (c *TelegramChannel) detectMention(msg *Message) bool {
	for _, ent := range msg.Entities {
		switch ent.Type {
		case "mention":
			// 提取实体文本，判断是否 @botUsername
			if c.botUsername != "" && ent.Offset+ent.Length <= len(msg.Text) {
				mentionText := msg.Text[ent.Offset : ent.Offset+ent.Length] // 含 @ 前缀
				if mentionText == "@"+c.botUsername {
					return true
				}
			}
		case "text_mention":
			// text_mention 的 User 字段指向被提及的用户
			if ent.User != nil && ent.User.ID == c.botUserID {
				return true
			}
		case "bot_command":
			// 群聊中，offset=0 的命令视为直接发往 Bot（如 "/help"）
			if ent.Offset == 0 {
				return true
			}
		}
	}
	return false
}

// Stop 优雅停止 polling。
func (c *TelegramChannel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.stopped = true
	c.mu.Unlock()

	// 等待 goroutine 退出
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Logger.Infow("telegram channel stopped", "channel", c.name)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reply 向指定聊天回复消息。这是一个便捷方法，供 Pipeline Action 处理器调用。
func (c *TelegramChannel) Reply(ctx context.Context, chatID int64, text string, replyToMessageID int64) error {
	_, err := c.api.sendMessage(ctx, chatID, text, replyToMessageID)
	return err
}
