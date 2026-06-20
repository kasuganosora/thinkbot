package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/util/errs"
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

	// APIBaseURL Telegram API 基础地址。用于反向代理或中国大陆等无法直连 api.telegram.org 的场景。
	// 默认 "https://api.telegram.org"。
	APIBaseURL string

	// ParseMode 发送消息时使用的格式化模式："HTML" / "MarkdownV2" / ""（纯文本）。
	// 默认 ""。
	ParseMode string
}

// telegramMaxMessageLength Telegram 单条消息最大长度。
const telegramMaxMessageLength = 4096

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
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = apiURL
	}
	return &TelegramChannel{
		name:  name,
		botID: botID,
		cfg:   cfg,
		api:   newAPIClient(cfg.Token, cfg.PollTimeout, cfg.APIBaseURL),
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
		return errs.Wrap(err, "telegram channel: token validation failed")
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

	// 提取文本：优先 Text，其次 Caption（图片/文件附带的文字）
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	// 如果没有文本但有附件，构造描述性文本
	if text == "" {
		if msg.Photo != nil {
			text = "[图片]"
		} else if msg.Document != nil {
			text = fmt.Sprintf("[文件: %s]", msg.Document.FileName)
		} else if msg.Sticker != nil {
			text = fmt.Sprintf("[贴纸: %s]", msg.Sticker.Emoji)
		}
	}

	// 仍然无内容则跳过
	if text == "" {
		return
	}

	// 发送"正在输入..."状态（fire-and-forget，使用独立超时避免被主 ctx 取消影响）
	go func() {
		actionCtx, actionCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer actionCancel()
		if err := c.api.sendChatAction(actionCtx, msg.Chat.ID, "typing"); err != nil {
			log.Logger.Debugw("telegram: sendChatAction failed",
				"channel", c.name, "err", err)
		}
	}()

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
		"chat_id":      msg.Chat.ID,
		"message_id":   msg.MessageID,
		"reply_target": chatID, // outbound 回写目标（Telegram: chatID）
		"date":         msg.Date,
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
	// 附件信息
	if msg.Photo != nil {
		metadata["has_photo"] = true
	}
	if msg.Document != nil {
		metadata["has_document"] = true
		metadata["document_name"] = msg.Document.FileName
	}
	if msg.Sticker != nil {
		metadata["has_sticker"] = true
	}
	if msg.MediaGroupID != "" {
		metadata["media_group_id"] = msg.MediaGroupID
	}

	coreMsg := core.Message{
		ID:        fmt.Sprintf("%d", msg.MessageID),
		BotID:     c.botID,
		Source:    c.name,
		Channel:   chatID,
		ChatType:  chatType,
		UserID:    userID,
		Text:      text,
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
//
// 注意：Telegram API 中 entity 的 Offset/Length 使用 UTF-16 code unit 计量，
// 而非 Go 字符串的字节偏移，因此需要通过 utf16Extract 转换。
func (c *TelegramChannel) detectMention(msg *Message) bool {
	for _, ent := range msg.Entities {
		switch ent.Type {
		case "mention":
			// 提取实体文本，判断是否 @botUsername
			if c.botUsername != "" {
				mentionText := utf16Extract(msg.Text, ent.Offset, ent.Length)
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

// utf16Extract 从 Go 字符串中按 UTF-16 code unit 偏移和长度提取子串。
// Telegram Bot API 中所有 entity 的 offset/length 都是 UTF-16 code unit 计量。
// 对于 BMP 字符（U+0000~U+FFFF），1 个 code unit = 1 个 rune。
// 对于补充平面字符（如 emoji 😀），1 个 rune = 2 个 UTF-16 code unit（surrogate pair）。
func utf16Extract(s string, offset, length int) string {
	// 将 Go 字符串转为 UTF-16 code units
	utf16Units := utf16.Encode([]rune(s))
	end := offset + length
	if offset < 0 || end > len(utf16Units) {
		return "" // 越界保护
	}
	// 提取 UTF-16 子片段并解码回字符串
	sub := utf16Units[offset:end]
	return string(utf16.Decode(sub))
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

// Reply 向指定聊天回复消息。便捷方法，供 Pipeline Action 处理器调用。
// 如果文本超过 4096 字符，自动拆分为多条消息发送。
func (c *TelegramChannel) Reply(ctx context.Context, chatID int64, text string, replyToMessageID int64) error {
	return c.ReplyWithMode(ctx, chatID, text, c.cfg.ParseMode, replyToMessageID)
}

// ReplyWithMode 向指定聊天回复消息，指定 parseMode。
func (c *TelegramChannel) ReplyWithMode(ctx context.Context, chatID int64, text, parseMode string, replyToMessageID int64) error {
	chunks := splitMessage(text, telegramMaxMessageLength)
	for i, chunk := range chunks {
		// 只有第一条消息引用 replyToMessageID
		var replyTo int64
		if i == 0 {
			replyTo = replyToMessageID
		}
		_, err := c.api.sendMessageFull(ctx, chatID, chunk, parseMode, replyTo)
		if err != nil {
			return err
		}
	}
	return nil
}

// EditMessage 编辑已发送的文本消息。用于流式输出场景。
func (c *TelegramChannel) EditMessage(ctx context.Context, chatID, messageID int64, text string) error {
	return c.api.editMessageText(ctx, chatID, messageID, text, c.cfg.ParseMode)
}

// SendTyping 发送"正在输入..."状态指示。
func (c *TelegramChannel) SendTyping(ctx context.Context, chatID int64) error {
	return c.api.sendChatAction(ctx, chatID, "typing")
}

// Send 实现 bot.Sender / outbound.ChannelSender 接口。
// 根据 Action 的内容回写消息到 Telegram。
//
// Action 字段约定：
//   - Action.Channel：目标 chatID（字符串形式的 int64，来源于 Inbound 的 msg.Channel）
//   - Action.Payload：发送内容（string 类型的文本消息）
//   - Action.Metadata["reply_to_message_id"]：回复目标消息 ID（int64 或 float64，可选）
//   - Action.Metadata["parse_mode"]：格式化模式（"HTML"/"MarkdownV2"，可选，默认用 Config 中的值）
//
// 行为：
//   - ActionReply：回复消息（支持 reply_to_message_id 引用、自动拆分长文本）
//   - 其他 ActionType：当前也按回复处理（后续扩展 Forward/Broadcast）
func (c *TelegramChannel) Send(ctx context.Context, action core.Action) error {
	// 解析 chatID
	chatID, err := strconv.ParseInt(action.Channel, 10, 64)
	if err != nil {
		return errs.Wrapf(err, "telegram send: invalid chatID %q", action.Channel)
	}

	// 提取文本
	text, ok := action.Payload.(string)
	if !ok {
		return fmt.Errorf("telegram send: payload is %T, expected string", action.Payload)
	}
	if text == "" {
		return nil // 空消息不发送
	}

	// 解析可选的 Metadata 参数
	var replyToMessageID int64
	parseMode := c.cfg.ParseMode

	if action.Metadata != nil {
		// reply_to_message_id: 支持 int64、float64（JSON unmarshal 的数字默认类型）
		if v, ok := action.Metadata["reply_to_message_id"]; ok {
			switch id := v.(type) {
			case int64:
				replyToMessageID = id
			case float64:
				replyToMessageID = int64(id)
			case int:
				replyToMessageID = int64(id)
			}
		}

		// parse_mode: 覆盖 Config 默认值
		if v, ok := action.Metadata["parse_mode"]; ok {
			if pm, ok := v.(string); ok {
				parseMode = pm
			}
		}
	}

	// 执行发送
	return c.ReplyWithMode(ctx, chatID, text, parseMode, replyToMessageID)
}

// splitMessage 将长文本按 maxLen 拆分为多条消息。
// 优先在换行符处拆分，其次按 rune 边界拆分。
func splitMessage(text string, maxLen int) []string {
	if utf8.RuneCountInString(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	runes := []rune(text)
	for len(runes) > 0 {
		end := maxLen
		if end > len(runes) {
			end = len(runes)
		}

		// 尝试在换行符处拆分
		if end < len(runes) {
			bestSplit := -1
			for i := end - 1; i > end/2; i-- {
				if runes[i] == '\n' {
					bestSplit = i + 1
					break
				}
			}
			if bestSplit > 0 {
				end = bestSplit
			}
		}

		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}
