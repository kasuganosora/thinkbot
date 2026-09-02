package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/traceid"
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

const (
	// mediaGroupWindow 相册聚合窗口。Telegram 投递同一相册的多条消息间隔极短，
	// 2s 足够覆盖；窗口采用滑动语义（每条命中都续期），
	// 因此张数再多、只要连续到达就仍被视为同一组。
	mediaGroupWindow = 2 * time.Second
	// mediaGroupMaxEntries mediaGroupSeen 的容量上限，超出后清理过期项、
	// 仍超限则整体重置（聚合是尽力而为，不影响正确性）。
	mediaGroupMaxEntries = 500
)

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

	// recentChats 记录本 Bot 近期收到消息的会话（chatID → 标题），
	// 供自主心跳等场景枚举「可在哪些群 / 私聊主动发言」。
	// 仅追加记录，不入站逻辑依赖；读侧由 RecentChats 暴露。
	recentChats  map[int64]core.ChatRef
	recentChatMu sync.Mutex

	// editSeen 记录已处理的 edited_message（key = "chatID:messageID" → 上次见到的文本），
	// 用于去重：Telegram 同一编辑可能被重复投递，未去重会导致同一条编辑重复触发完整 LLM 编排。
	// 仅在「文本与上次相同」时跳过；用户真正再次编辑（文本变化）仍会正常处理。
	editSeen map[string]string
	editMu   sync.Mutex

	// mediaGroupSeen 记录相册（media_group）首条入站消息的时间，
	// 用于把一次发图聚合为一次消息：Telegram 把 N 图相册拆成 N 条独立 update，
	// 每条 message_id 各不相同，ingress 按 msg.ID 的去重拦不住，
	// 会导致同一次发图触发 N 次完整 LLM 编排并大概率回复 N 条
	// （与 8/24 misskey「同一条消息重复回复 N+1」同形态，只是触发源不同）。
	// 同组仅首条入站，窗口内的后续条直接跳过；说明文字（caption）由 Telegram 附在首条上。
	mediaGroupSeen map[string]time.Time
	mediaGroupMu   sync.Mutex

	// choicePending 追踪 user_choice 多选的进行中点选（questionID → 状态）。
	choiceMu      sync.Mutex
	choicePending map[string]*choicePending
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
	traceid.L(ctx).Infow("telegram channel started",
		"channel", c.name, "bot_username", me.Username, "bot_id", me.ID)

	c.botUserID = me.ID
	c.botUsername = me.Username

	interaction.RegisterPollCreator("telegram", c.CreateChoiceMessage)

	// 注册 Bot 自身用户 ID 到 Ingress，作为防止自回复循环的第二道防线。
	// （Telegram Bot API 天然不会通过 getUpdates 回传 Bot 自身消息，
	// 但注册后可作为防御性编程的保险）
	// 注册时机：在 pollLoop 启动前，确保零竞态。
	ingress.RegisterSelfUserID(fmt.Sprintf("%d", me.ID))

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
			traceid.L(ctx).Warnw("telegram poll error",
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
	if upd.CallbackQuery != nil {
		c.handleCallbackQuery(ctx, upd.CallbackQuery)
		return
	}

	// 只处理消息和编辑消息
	var msg *Message
	if upd.Message != nil {
		msg = upd.Message
	} else if upd.EditedMessage != nil {
		msg = upd.EditedMessage
	} else {
		return // 忽略非消息更新
	}

	// 提取文本：优先 Text，其次 Caption（图片/文件附带的文字），实体与文本按来源配对。
	text, entities := c.mentionTextAndEntities(msg)

	// 如果没有文本但有附件，构造描述性文本（占位文本不带实体，无法被提及）
	if text == "" {
		entities = nil
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

	// 相册聚合：同一次发图的后续条直接跳过，避免 N 图触发 N 次编排与重复回复。
	// 放在发 typing 与入站之前，后续条连「正在输入」都不该触发。
	//
	// 仅对新消息聚合：编辑相册说明文字时 edited_message 同样携带 media_group_id，
	// 若一并参与聚合，窗口内的正常编辑会被误吞（编辑走下方 editSeen 独立去重）。
	if upd.EditedMessage == nil && c.mediaGroupAlreadySeen(msg.MediaGroupID) {
		return
	}

	// edited_message 去重：同一编辑可能被重复投递，未去重会让同一条编辑重复触发完整 LLM 编排。
	// 仅当「文本与上次处理时相同」才跳过；用户真正再次编辑（文本变化）仍会正常处理。
	if upd.EditedMessage != nil && c.editSeenAlready(msg.Chat.ID, msg.MessageID, text) {
		return
	}

	// 发送"正在输入..."状态（fire-and-forget，使用独立超时避免被主 ctx 取消影响）
	go func() {
		actionCtx, actionCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer actionCancel()
		if err := c.api.sendChatAction(actionCtx, msg.Chat.ID, "typing"); err != nil {
			traceid.L(ctx).Debugw("telegram: sendChatAction failed",
				"channel", c.name, "err", err)
		}
	}()

	// 转换 chat ID 为字符串 channel
	chatID := fmt.Sprintf("%d", msg.Chat.ID)
	// 记录近期会话，供心跳等自主发帖场景枚举目标（无副作用，失败不影响消息处理）。
	c.recordChat(msg.Chat.ID, msg.Chat.Title)
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
		// 方式 2-4: 解析 entities（文本与实体按来源配对）
		if !mentioned {
			mentioned = c.detectMention(text, entities)
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
		"channel_type": "telegram", // Channel 类型，供 ToolSessionContext 使用
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
		traceid.L(ctx).Warnw("telegram ingress receive failed",
			"channel", c.name, "message_id", msg.MessageID, "err", err)
	}
}

// mentionTextAndEntities 返回用于提及检测的「文本 + 实体」配对。
//
// Telegram 把正文的实体放在 Entities（对应 Text），把图片/文件等附件说明的实体放在
// CaptionEntities（对应 Caption）。二者必须与对应文本配对取用：若固定取 Text+Entities，
// 则带 "@bot 看看这个" 说明的图片消息实体为空，群里的图片互动会完全漏判提及。
//
// 无正文时降级取 Caption；两者都为空返回空串与 nil（此时由调用方构造附件占位文本）。
func (c *TelegramChannel) mentionTextAndEntities(msg *Message) (string, []MessageEntity) {
	if msg.Text != "" {
		return msg.Text, msg.Entities
	}
	return msg.Caption, msg.CaptionEntities
}

// detectMention 通过解析消息 entities 判断是否 @提及了 Bot 或使用了 Bot 命令。
// 检测规则：
//   - mention: 文本含 @botUsername（如 "@mybot hello"）
//   - text_mention: entity 中 User.ID == botUserID（无 username 的用户提及）
//   - bot_command: offset=0 的 /command（群聊中命令视为直接对话 Bot）
//
// text 与 entities 必须是**配对**传入的：Telegram 把正文实体放在 entities（对应 text），
// 而图片/文件等附件的说明文字实体放在 caption_entities（对应 caption）。
// 若固定取 msg.Text + msg.Entities，则带 "@bot 看看" 说明的图片消息实体为空，
// 会漏判提及导致 bot 在群里对图片互动毫无反应（详见 handleUpdate 中的配对逻辑）。
//
// 注意：Telegram API 中 entity 的 Offset/Length 使用 UTF-16 code unit 计量，
// 而非 Go 字符串的字节偏移，因此需要通过 utf16Extract 转换。
func (c *TelegramChannel) detectMention(text string, entities []MessageEntity) bool {
	for _, ent := range entities {
		switch ent.Type {
		case "mention":
			// 提取实体文本，判断是否 @botUsername
			if c.botUsername != "" {
				mentionText := utf16Extract(text, ent.Offset, ent.Length)
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
			// 群聊中，offset=0 的命令视为直接发往 Bot（如 "/help"）。
			// 但命令可能指向其他 bot：/cmd@otherbot —— 仅当不含 @ 或 @ 后用户名等于自身才触发，
			// 避免被 /start@otherbot 这类指向他人 bot 的命令误触发（历史 misskey 同类 5007）。
			if ent.Offset == 0 {
				cmd := utf16Extract(text, ent.Offset, ent.Length)
				if at := strings.Index(cmd, "@"); at >= 0 {
					target := strings.ToLower(strings.TrimPrefix(cmd[at:], "@"))
					if c.botUsername != "" && target != strings.ToLower(c.botUsername) {
						continue
					}
				}
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
		traceid.L(ctx).Infow("telegram channel stopped", "channel", c.name)
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

// recordChat 记录一个近期活跃会话（供 RecentChats 枚举）。
// 线程安全；容量上限 50，超出时淘汰最早插入的一项（仅用于展示枚举，无一致性要求）。
func (c *TelegramChannel) recordChat(id int64, title string) {
	if id == 0 {
		return
	}
	c.recentChatMu.Lock()
	defer c.recentChatMu.Unlock()
	if c.recentChats == nil {
		c.recentChats = make(map[int64]core.ChatRef)
	}
	c.recentChats[id] = core.ChatRef{ID: id, Title: title}
	if len(c.recentChats) > 50 {
		for k := range c.recentChats { // 删第一个即足够（map 遍历顺序随机，无影响）
			delete(c.recentChats, k)
			break
		}
	}
}

// editSeenAlready 记录并返回该 edited_message 是否已以相同文本处理过（用于去重）。
// key 为 "chatID:messageID"，值为上次处理的文本：若本次文本与上次相同则视为重复投递，返回 true；
// 若文本不同（用户再次编辑）则更新记录并返回 false，保证真实编辑仍会被处理。
func (c *TelegramChannel) editSeenAlready(chatID, messageID int64, text string) bool {
	key := fmt.Sprintf("%d:%d", chatID, messageID)
	c.editMu.Lock()
	defer c.editMu.Unlock()
	if c.editSeen == nil {
		c.editSeen = make(map[string]string)
	}
	if prev, ok := c.editSeen[key]; ok && prev == text {
		return true
	}
	// 防止 map 无限增长：超过上限时整体重置（去重是尽力而为，不影响正确性）。
	if len(c.editSeen) > 5000 {
		c.editSeen = make(map[string]string)
	}
	c.editSeen[key] = text
	return false
}

// mediaGroupAlreadySeen 判断该相册是否已有消息入站，用于把一次发图聚合成一条消息。
// 返回 true 表示本条属于「同组后续条」，调用方应跳过（首条已入站并携带 caption 与附件信息）。
// groupID 为空（非相册消息）直接返回 false，不参与聚合。
//
// 已知局限：只保留首条。updates 按 update_id 递增处理，而官方客户端把 caption 附在
// 相册首条上，故绝大多数情况下说明文字不会丢；若某客户端把 caption 附在后续条，
// 那条文字会被跳过（仅剩「[图片]」占位）。真要修得引入延迟缓冲窗口攒齐全组再入站，
// 代价是所有相册消息都被推迟，权衡后选择保留现方案。
func (c *TelegramChannel) mediaGroupAlreadySeen(groupID string) bool {
	if groupID == "" {
		return false
	}
	now := time.Now()
	c.mediaGroupMu.Lock()
	defer c.mediaGroupMu.Unlock()
	if c.mediaGroupSeen == nil {
		c.mediaGroupSeen = make(map[string]time.Time)
	}

	// 顺带清理过期项，防止 map 无限增长
	for k, t := range c.mediaGroupSeen {
		if now.Sub(t) > mediaGroupWindow {
			delete(c.mediaGroupSeen, k)
		}
	}
	if len(c.mediaGroupSeen) > mediaGroupMaxEntries {
		c.mediaGroupSeen = make(map[string]time.Time)
	}

	if t, ok := c.mediaGroupSeen[groupID]; ok && now.Sub(t) <= mediaGroupWindow {
		// 滑动续期：相册张数多时仍视为同一组
		c.mediaGroupSeen[groupID] = now
		return true
	}
	c.mediaGroupSeen[groupID] = now
	return false
}

// RecentChats 返回近期活跃会话列表（实现 core.RecentChatLister），
// 供心跳等自主发帖场景枚举目标。最多返回近 20 个，无则返回 nil。
func (c *TelegramChannel) RecentChats() []core.ChatRef {
	c.recentChatMu.Lock()
	defer c.recentChatMu.Unlock()
	if len(c.recentChats) == 0 {
		return nil
	}
	refs := make([]core.ChatRef, 0, len(c.recentChats))
	for _, r := range c.recentChats {
		refs = append(refs, r)
	}
	if len(refs) > 20 {
		refs = refs[len(refs)-20:]
	}
	return refs
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
