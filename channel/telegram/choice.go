package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// choicePending 是一条 Telegram user_choice 消息的进行中状态（主要用于多选 toggle）。
type choicePending struct {
	ChatID    int64
	MessageID int64
	Multiple  bool
	Selected  map[string]bool // option IDs
	Question  string
	Options   []interaction.Option
}

const (
	choiceCallbackPrefix = "c|"
	choiceDoneToken      = "~"
	telegramButtonMaxLen = 64
)

// CreateChoiceMessage 实现 interaction.PollCreator：发一条带 inline keyboard 的问题消息。
// PollCreator 不传 chatID，目标会话从 interaction.Lookup(questionID).ChatID 读取
// （与入站 MessageMeta.ChatID / fmt.Sprintf("%d", chat.ID) 一致）。
func (c *TelegramChannel) CreateChoiceMessage(ctx context.Context, question, replyID string, options []string, multiple bool, timeoutSecs int, questionID string) (string, error) {
	q, err := interaction.Default().Lookup(questionID)
	if err != nil {
		return "", err
	}
	chatID, err := strconv.ParseInt(q.ChatID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("telegram CreateChoiceMessage: invalid chatID %q: %w", q.ChatID, err)
	}

	text := question
	if utf8.RuneCountInString(text) > telegramMaxMessageLength {
		r := []rune(text)
		text = string(r[:telegramMaxMessageLength])
	}

	markup := buildChoiceKeyboard(questionID, q.Options, multiple, nil)
	msgID, err := c.api.sendMessageWithMarkup(ctx, chatID, text, c.cfg.ParseMode, 0, markup)
	if err != nil {
		return "", err
	}

	c.choiceMu.Lock()
	if c.choicePending == nil {
		c.choicePending = make(map[string]*choicePending)
	}
	sel := make(map[string]bool)
	c.choicePending[questionID] = &choicePending{
		ChatID:    chatID,
		MessageID: msgID,
		Multiple:  multiple,
		Selected:  sel,
		Question:  question,
		Options:   append([]interaction.Option(nil), q.Options...),
	}
	c.choiceMu.Unlock()

	_ = options
	_ = replyID
	_ = timeoutSecs
	return strconv.FormatInt(msgID, 10), nil
}

// handleCallbackQuery 处理 inline keyboard 点击：立刻 ack，再 ResolveFrom。
// 不得把 callback 当聊天消息注入 Ingress（否则会误开一轮 LLM）。
func (c *TelegramChannel) handleCallbackQuery(ctx context.Context, cq *CallbackQuery) {
	if cq == nil {
		return
	}

	answered := false
	ack := func(text string) {
		if answered || c.api == nil {
			return
		}
		answered = true
		ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.api.answerCallbackQuery(ackCtx, cq.ID, text); err != nil {
			traceid.L(ctx).Debugw("telegram: answerCallbackQuery failed",
				"channel", c.name, "err", err)
		}
	}

	qid, token, ok := parseChoiceCallback(cq.Data)
	if !ok {
		ack("")
		return
	}
	if cq.Message == nil {
		ack("")
		return
	}
	chatID := cq.Message.Chat.ID
	messageID := cq.Message.MessageID
	chatIDStr := strconv.FormatInt(chatID, 10)

	q, err := interaction.Default().Lookup(qid)
	if err != nil {
		ack("")
		c.stripChoiceKeyboard(ctx, chatID, messageID, "")
		c.dropChoicePending(qid)
		return
	}

	if token == choiceDoneToken {
		c.choiceMu.Lock()
		p := c.choicePending[qid]
		var ids []string
		if p != nil {
			for _, opt := range p.Options {
				if p.Selected[opt.ID] {
					ids = append(ids, opt.ID)
				}
			}
		}
		c.choiceMu.Unlock()
		if len(ids) == 0 {
			ack("请至少选一项")
			return
		}
		ack("")
		indices, err := interaction.Default().IndicesForOptionIDs(qid, ids)
		if err != nil {
			traceid.L(ctx).Warnw("telegram: choice done indices",
				"channel", c.name, "question_id", qid, "err", err)
			return
		}
		c.finishChoice(ctx, qid, chatIDStr, chatID, messageID, q, indices)
		return
	}

	ack("")

	if q.Mode == interaction.ModeMulti || c.choiceIsMultiple(qid) {
		c.toggleChoice(ctx, qid, token, q, chatID, messageID)
		return
	}

	indices, err := interaction.Default().IndicesForOptionIDs(qid, []string{token})
	if err != nil {
		traceid.L(ctx).Debugw("telegram: unknown option id",
			"channel", c.name, "question_id", qid, "opt", token, "err", err)
		return
	}
	c.finishChoice(ctx, qid, chatIDStr, chatID, messageID, q, indices)
}

func (c *TelegramChannel) choiceIsMultiple(questionID string) bool {
	c.choiceMu.Lock()
	defer c.choiceMu.Unlock()
	p := c.choicePending[questionID]
	return p != nil && p.Multiple
}

func (c *TelegramChannel) toggleChoice(ctx context.Context, qid, optID string, q interaction.Question, chatID, messageID int64) {
	c.choiceMu.Lock()
	if c.choicePending == nil {
		c.choicePending = make(map[string]*choicePending)
	}
	p := c.choicePending[qid]
	if p == nil {
		p = &choicePending{
			ChatID:    chatID,
			MessageID: messageID,
			Multiple:  true,
			Selected:  map[string]bool{},
			Question:  q.Question,
			Options:   append([]interaction.Option(nil), q.Options...),
		}
		c.choicePending[qid] = p
	}
	if p.Selected[optID] {
		delete(p.Selected, optID)
	} else {
		p.Selected[optID] = true
	}
	selected := make(map[string]bool, len(p.Selected))
	for k, v := range p.Selected {
		selected[k] = v
	}
	opts := append([]interaction.Option(nil), p.Options...)
	c.choiceMu.Unlock()

	if c.api == nil {
		return
	}
	markup := buildChoiceKeyboard(qid, opts, true, selected)
	editCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := c.api.editMessageReplyMarkup(editCtx, chatID, messageID, markup); err != nil {
		traceid.L(ctx).Debugw("telegram: edit choice keyboard failed",
			"channel", c.name, "err", err)
	}
}

func (c *TelegramChannel) finishChoice(ctx context.Context, qid, chatIDStr string, chatID, messageID int64, q interaction.Question, indices []int) {
	ans := interaction.Answer{Selected: indices, Via: interaction.ViaTelegram}
	err := interaction.Default().ResolveFrom(qid, chatIDStr, ans)
	if err != nil {
		if errors.Is(err, interaction.ErrQuestionNotFound) {
			traceid.L(ctx).Debugw("telegram: choice resolve rejected (wrong chat or gone)",
				"channel", c.name, "question_id", qid, "chat_id", chatIDStr)
			return
		}
		if errors.Is(err, interaction.ErrAlreadyResolved) {
			c.stripChoiceKeyboard(ctx, chatID, messageID, "")
			c.dropChoicePending(qid)
			return
		}
		traceid.L(ctx).Warnw("telegram: choice resolve failed",
			"channel", c.name, "question_id", qid, "err", err)
		return
	}

	summary := formatChoiceResolvedText(q, indices)
	c.stripChoiceKeyboard(ctx, chatID, messageID, summary)
	c.dropChoicePending(qid)
}

func (c *TelegramChannel) stripChoiceKeyboard(ctx context.Context, chatID, messageID int64, text string) {
	if c.api == nil || chatID == 0 || messageID == 0 {
		return
	}
	empty := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}
	editCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if text != "" {
		if err := c.api.editMessageTextWithMarkup(editCtx, chatID, messageID, text, c.cfg.ParseMode, empty); err != nil {
			traceid.L(ctx).Debugw("telegram: strip choice keyboard (edit text) failed",
				"channel", c.name, "err", err)
		}
		return
	}
	if err := c.api.editMessageReplyMarkup(editCtx, chatID, messageID, empty); err != nil {
		traceid.L(ctx).Debugw("telegram: strip choice keyboard failed",
			"channel", c.name, "err", err)
	}
}

func (c *TelegramChannel) dropChoicePending(questionID string) {
	c.choiceMu.Lock()
	delete(c.choicePending, questionID)
	c.choiceMu.Unlock()
}

func formatChoiceResolvedText(q interaction.Question, indices []int) string {
	var b strings.Builder
	b.WriteString("✓ ")
	b.WriteString(q.Question)
	if len(indices) > 0 {
		b.WriteByte('\n')
		for i, idx := range indices {
			if idx < 0 || idx >= len(q.Options) {
				continue
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(q.Options[idx].Label)
		}
	}
	text := b.String()
	if utf8.RuneCountInString(text) > telegramMaxMessageLength {
		r := []rune(text)
		text = string(r[:telegramMaxMessageLength])
	}
	return text
}

// parseChoiceCallback 解析 callback_data：c|{questionID}|{optID}，optID 为 o0..o7 或 ~（确认）。
func parseChoiceCallback(data string) (questionID, optID string, ok bool) {
	if !strings.HasPrefix(data, choiceCallbackPrefix) {
		return "", "", false
	}
	rest := data[len(choiceCallbackPrefix):]
	i := strings.LastIndex(rest, "|")
	if i <= 0 || i >= len(rest)-1 {
		return "", "", false
	}
	qid := rest[:i]
	tok := rest[i+1:]
	if qid == "" || tok == "" {
		return "", "", false
	}
	return qid, tok, true
}

func encodeChoiceCallback(questionID, optID string) string {
	return choiceCallbackPrefix + questionID + "|" + optID
}

// buildChoiceKeyboard 构造选项按钮。selected 为已选 option ID 集合（多选打 ✓）；nil 表示全未选。
func buildChoiceKeyboard(questionID string, options []interaction.Option, multiple bool, selected map[string]bool) *InlineKeyboardMarkup {
	rows := make([][]InlineKeyboardButton, 0, len(options)+1)
	for _, opt := range options {
		label := opt.Label
		if selected[opt.ID] {
			label = "✓ " + label
		}
		rows = append(rows, []InlineKeyboardButton{{
			Text:         truncButtonText(label, telegramButtonMaxLen),
			CallbackData: encodeChoiceCallback(questionID, opt.ID),
		}})
	}
	if multiple {
		rows = append(rows, []InlineKeyboardButton{{
			Text:         "确认",
			CallbackData: encodeChoiceCallback(questionID, choiceDoneToken),
		}})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

func truncButtonText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return string(r[:1])
	}
	return string(r[:max-1]) + "…"
}
