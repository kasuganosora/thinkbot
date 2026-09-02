package telegram

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// API — Telegram Bot API 客户端
// ============================================================================

// apiURL 是 Telegram Bot API 的基础地址。
const apiURL = "https://api.telegram.org"

const (
	// pollTimeoutBuffer long polling 客户端超时在 pollTimeout 之上的固定余量。
	// Telegram 服务端会挂起请求等待至 timeout 秒才返回，客户端必须给足余量，
	// 否则正常的空轮询会被误判为超时。
	pollTimeoutBuffer = 15 * time.Second
	// pollCtxExtra context 超时在「客户端超时」之上再追加的余量。
	//
	// context 必须比客户端超时更晚到期：否则 context 会先取消请求，
	// 客户端层面的超时形同虚设，重试也无从生效（此前 reqCtx 用 +10s、
	// 客户端用 +15s，context 反而先到期，属于明确的不一致）。
	pollCtxExtra = 5 * time.Second
)

// apiClient 封装了 Telegram Bot API 的 HTTP 调用。
type apiClient struct {
	client *http.Client
	token  string

	// 出站限流（令牌桶式）：Telegram 对发送频率有严格限制（群聊约 1 条/秒、广播约 30 条/秒），
	// 无限制会导致 429 并造成拆分消息部分丢失。sendMu 保护 lastSend，sendInterval 为最小发送间隔。
	sendMu       sync.Mutex
	lastSend     time.Time
	sendInterval time.Duration
}

// newAPIClient 创建一个 Telegram Bot API 客户端。
// pollTimeout 是 long polling 超时秒数，用于将 HTTP 客户端超时设为足够大的值。
// baseURL 是 API 基础地址（默认 https://api.telegram.org）。
func newAPIClient(token string, pollTimeout int, baseURL string, opts ...http.Option) *apiClient {
	// HTTP 客户端超时需要覆盖 long polling 等待时间 + 网络余量。
	// 设为 0（无超时）让 context 级别的超时来控制。
	var httpTimeout time.Duration
	if pollTimeout > 0 {
		httpTimeout = time.Duration(pollTimeout)*time.Second + pollTimeoutBuffer
	}
	if baseURL == "" {
		baseURL = apiURL
	}
	defaultOpts := []http.Option{
		http.WithBaseURL(fmt.Sprintf("%s/bot%s", baseURL, token)),
		http.WithTimeout(httpTimeout),
		// 429/5xx + 网络错误按 Retry-After 退避重试（无 Retry-After 时指数退避+抖动），
		// 避免出站消息因限流而部分丢失。与 misskey searchClient 一致：util/http.WithRetry
		// 会读取 429 的 Retry-After（见 client.go:489），不再用固定间隔无视限流。
		http.WithRetry(retry.Config{
			MaxRetries: 5,
			Backoff: &retry.Backoff{
				Strategy: retry.StrategyExponential,
				Initial:  500 * time.Millisecond,
				Max:      8 * time.Second,
				Factor:   2.0,
				Jitter:   true,
			},
		}),
	}
	opts = append(defaultOpts, opts...)
	return &apiClient{
		client:       http.New(opts...),
		token:        token,
		sendInterval: 250 * time.Millisecond, // 约 4 条/秒，低于 Telegram 群聊限制且对私聊足够
	}
}

// throttle 在每次出站发送前做最小间隔限流，平滑发送速率以规避 429。
// 返回 ctx 被取消时立即报错；否则等待到允许发送的下一个时间槽。
func (a *apiClient) throttle(ctx context.Context) error {
	a.sendMu.Lock()
	elapsed := time.Since(a.lastSend)
	if elapsed < a.sendInterval {
		wait := a.sendInterval - elapsed
		a.lastSend = a.lastSend.Add(wait)
		a.sendMu.Unlock()
		select {
		case <-time.After(wait):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.lastSend = time.Now()
	a.sendMu.Unlock()
	return nil
}

// getMe 获取当前 Bot 的信息。常用于验证 token 是否有效。
func (a *apiClient) getMe(ctx context.Context) (*User, error) {
	var resp apiResponse[User]
	err := a.client.GetJSON(ctx, "getMe", &resp)
	if err != nil {
		return nil, errs.Wrap(err, "telegram getMe")
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getMe failed: [%d] %s", resp.ErrorCode, resp.Description)
	}
	return &resp.Result, nil
}

// getUpdates 使用 long polling 获取更新。timeout 为秒数。
func (a *apiClient) getUpdates(ctx context.Context, offset int64, timeout int, allowedUpdates []string) ([]Update, error) {
	allowedUpdates = mergeCallbackQueryUpdate(allowedUpdates)
	req := a.client.Post("getUpdates").
		SetContext(ctx).
		SetJSONBody(getUpdatesRequest{
			Offset:         offset,
			Limit:          100,
			Timeout:        timeout,
			AllowedUpdates: allowedUpdates,
		})

	resp, err := req.Do()
	if err != nil {
		return nil, errs.Wrap(err, "telegram getUpdates")
	}

	var apiResp apiResponse[[]Update]
	if err := resp.JSON(&apiResp); err != nil {
		return nil, errs.Wrap(err, "telegram getUpdates parse")
	}
	if !apiResp.OK {
		return nil, fmt.Errorf("telegram getUpdates failed: [%d] %s", apiResp.ErrorCode, apiResp.Description)
	}
	return apiResp.Result, nil
}

// defaultAllowedUpdates 是 getUpdates 未指定时的默认类型；必须含 callback_query，
// 否则 inline keyboard 点击永远不会送达。
func defaultAllowedUpdates() []string {
	return []string{"message", "edited_message", "my_chat_member", "callback_query"}
}

// mergeCallbackQueryUpdate 保证 allowed updates 含 callback_query。
// 空列表 → 默认集合；调用方自定义列表则追加（已有则不动）。
func mergeCallbackQueryUpdate(updates []string) []string {
	if len(updates) == 0 {
		return defaultAllowedUpdates()
	}
	for _, u := range updates {
		if u == "callback_query" {
			return updates
		}
	}
	out := make([]string, len(updates)+1)
	copy(out, updates)
	out[len(updates)] = "callback_query"
	return out
}

// sendMessageFull 发送文本消息，支持 parseMode。
func (a *apiClient) sendMessageFull(ctx context.Context, chatID int64, text, parseMode string, replyTo int64) (int64, error) {
	return a.sendMessageWithMarkup(ctx, chatID, text, parseMode, replyTo, nil)
}

// sendMessageWithMarkup 发送带 inline keyboard 的文本消息。markup 为 nil 时与 sendMessageFull 等价。
func (a *apiClient) sendMessageWithMarkup(ctx context.Context, chatID int64, text, parseMode string, replyTo int64, markup *InlineKeyboardMarkup) (int64, error) {
	if err := a.throttle(ctx); err != nil {
		return 0, errs.Wrap(err, "telegram sendMessage throttle")
	}
	req := a.client.Post("sendMessage").
		SetContext(ctx).
		SetJSONBody(sendMessageRequest{
			ChatID:           chatID,
			Text:             text,
			ParseMode:        parseMode,
			ReplyToMessageID: replyTo,
			ReplyMarkup:      markup,
		})

	resp, err := req.Do()
	if err != nil {
		return 0, errs.Wrap(err, "telegram sendMessage")
	}

	var apiResp apiResponse[sendMessageResult]
	if err := resp.JSON(&apiResp); err != nil {
		return 0, errs.Wrap(err, "telegram sendMessage parse")
	}
	if !apiResp.OK {
		return 0, fmt.Errorf("telegram sendMessage failed: [%d] %s", apiResp.ErrorCode, apiResp.Description)
	}
	return apiResp.Result.MessageID, nil
}

// sendChatAction 发送聊天状态（如"正在输入..."）。
func (a *apiClient) sendChatAction(ctx context.Context, chatID int64, action string) error {
	if err := a.throttle(ctx); err != nil {
		return errs.Wrap(err, "telegram sendChatAction throttle")
	}
	req := a.client.Post("sendChatAction").
		SetContext(ctx).
		SetJSONBody(sendChatActionRequest{
			ChatID: chatID,
			Action: action,
		})

	resp, err := req.Do()
	if err != nil {
		return errs.Wrap(err, "telegram sendChatAction")
	}

	var apiResp apiResponse[any]
	if err := resp.JSON(&apiResp); err != nil {
		return errs.Wrap(err, "telegram sendChatAction parse")
	}
	if !apiResp.OK {
		return fmt.Errorf("telegram sendChatAction failed: [%d] %s", apiResp.ErrorCode, apiResp.Description)
	}
	return nil
}

// editMessageText 编辑已发送的文本消息。
func (a *apiClient) editMessageText(ctx context.Context, chatID, messageID int64, text, parseMode string) error {
	return a.editMessageTextWithMarkup(ctx, chatID, messageID, text, parseMode, nil)
}

func (a *apiClient) editMessageTextWithMarkup(ctx context.Context, chatID, messageID int64, text, parseMode string, markup *InlineKeyboardMarkup) error {
	if err := a.throttle(ctx); err != nil {
		return errs.Wrap(err, "telegram editMessageText throttle")
	}
	req := a.client.Post("editMessageText").
		SetContext(ctx).
		SetJSONBody(editMessageTextRequest{
			ChatID:      chatID,
			MessageID:   messageID,
			Text:        text,
			ParseMode:   parseMode,
			ReplyMarkup: markup,
		})

	resp, err := req.Do()
	if err != nil {
		return errs.Wrap(err, "telegram editMessageText")
	}

	var apiResp apiResponse[any]
	if err := resp.JSON(&apiResp); err != nil {
		return errs.Wrap(err, "telegram editMessageText parse")
	}
	if !apiResp.OK {
		return fmt.Errorf("telegram editMessageText failed: [%d] %s", apiResp.ErrorCode, apiResp.Description)
	}
	return nil
}

// apiTimeoutMultiplier 将秒级 timeout 转为 long polling 请求的 context 超时。
// 取「客户端超时 + pollCtxExtra」，确保 context 不会先于客户端超时取消请求
// （客户端超时见 newAPIClient，同样是 pollTimeout + pollTimeoutBuffer）。
//
// 注意：getUpdates 挂在带重试的 HTTP client 上，重试累计耗时可能超过本 context，
// 故 long polling 的重试主要依赖 pollLoop 自身的 3s 退避，而非 HTTP 层的重试。
func apiTimeoutMultiplier(timeoutSec int) time.Duration {
	return time.Duration(timeoutSec)*time.Second + pollTimeoutBuffer + pollCtxExtra
}

// banChatMember 踢出群成员（封禁）。
// untilDate: Unix 时间戳，届时自动解封。0 表示永久。
// revokeMessages: 是否同时删除该用户的所有消息。
func (a *apiClient) banChatMember(ctx context.Context, chatID, userID int64, untilDate int64, revokeMessages bool) error {
	return a.simplePost(ctx, "banChatMember", banChatMemberRequest{
		ChatID:         chatID,
		UserID:         userID,
		UntilDate:      untilDate,
		RevokeMessages: revokeMessages,
	})
}

// unbanChatMember 解除群成员封禁。
// onlyIfBanned: 仅当用户当前处于被封状态时才执行，避免对正常成员误操作。
func (a *apiClient) unbanChatMember(ctx context.Context, chatID, userID int64, onlyIfBanned bool) error {
	return a.simplePost(ctx, "unbanChatMember", unbanChatMemberRequest{
		ChatID:       chatID,
		UserID:       userID,
		OnlyIfBanned: onlyIfBanned,
	})
}

// getChat 获取聊天详情。
func (a *apiClient) getChat(ctx context.Context, chatID int64) (*getChatResponse, error) {
	var resp apiResponse[getChatResponse]
	if err := a.client.GetJSON(ctx, fmt.Sprintf("getChat?chat_id=%d", chatID), &resp); err != nil {
		return nil, errs.Wrap(err, "telegram getChat")
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getChat failed: [%d] %s", resp.ErrorCode, resp.Description)
	}
	return &resp.Result, nil
}

// pinChatMessage 置顶消息。
// disableNotification: true 时不向全体成员发送通知。
func (a *apiClient) pinChatMessage(ctx context.Context, chatID, messageID int64, disableNotification bool) error {
	return a.simplePost(ctx, "pinChatMessage", pinChatMessageRequest{
		ChatID:              chatID,
		MessageID:           messageID,
		DisableNotification: disableNotification,
	})
}

// deleteMessage 删除消息。
func (a *apiClient) deleteMessage(ctx context.Context, chatID, messageID int64) error {
	return a.simplePost(ctx, "deleteMessage", deleteMessageRequest{
		ChatID:    chatID,
		MessageID: messageID,
	})
}

// getChatMemberCount 获取群组成员数（独立 API，返回纯整数）。
func (a *apiClient) getChatMemberCount(ctx context.Context, chatID int64) (int, error) {
	var resp apiResponse[int]
	if err := a.client.GetJSON(ctx, fmt.Sprintf("getChatMemberCount?chat_id=%d", chatID), &resp); err != nil {
		return 0, errs.Wrap(err, "telegram getChatMemberCount")
	}
	if !resp.OK {
		return 0, fmt.Errorf("telegram getChatMemberCount failed: [%d] %s", resp.ErrorCode, resp.Description)
	}
	return resp.Result, nil
}

// getChatAdministrators 获取群组管理员列表。
func (a *apiClient) getChatAdministrators(ctx context.Context, chatID int64) ([]ChatMember, error) {
	var resp apiResponse[[]ChatMember]
	if err := a.client.GetJSON(ctx, fmt.Sprintf("getChatAdministrators?chat_id=%d", chatID), &resp); err != nil {
		return nil, errs.Wrap(err, "telegram getChatAdministrators")
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getChatAdministrators failed: [%d] %s", resp.ErrorCode, resp.Description)
	}
	return resp.Result, nil
}

// sendPhoto 通过 URL 发送图片。
// 预留：计划用于未来的 telegram_send_photo 工具。
//
//nolint:unused // 预留 API 方法
func (a *apiClient) sendPhoto(ctx context.Context, chatID int64, photoURL, caption string) error {
	return a.simplePost(ctx, "sendPhoto", sendPhotoRequest{
		ChatID:  chatID,
		Photo:   photoURL,
		Caption: caption,
	})
}

// answerCallbackQuery 停止客户端按钮 spinner。text 可空。
func (a *apiClient) answerCallbackQuery(ctx context.Context, callbackQueryID, text string) error {
	return a.simplePost(ctx, "answerCallbackQuery", answerCallbackQueryRequest{
		CallbackQueryID: callbackQueryID,
		Text:            text,
	})
}

// editMessageReplyMarkup 更新或清空 inline keyboard。
func (a *apiClient) editMessageReplyMarkup(ctx context.Context, chatID, messageID int64, markup *InlineKeyboardMarkup) error {
	return a.simplePost(ctx, "editMessageReplyMarkup", editMessageReplyMarkupRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		ReplyMarkup: markup,
	})
}

// simplePost 发送带 JSON body 的 POST 请求并检查 OK 状态。
func (a *apiClient) simplePost(ctx context.Context, endpoint string, body any) error {
	if err := a.throttle(ctx); err != nil {
		return errs.Wrapf(err, "telegram %s throttle", endpoint)
	}
	resp, err := a.client.Post(endpoint).
		SetContext(ctx).
		SetJSONBody(body).
		Do()
	if err != nil {
		return errs.Wrapf(err, "telegram %s", endpoint)
	}

	var apiResp apiResponse[any]
	if err := resp.JSON(&apiResp); err != nil {
		return errs.Wrapf(err, "telegram %s parse", endpoint)
	}
	if !apiResp.OK {
		return fmt.Errorf("telegram %s failed: [%d] %s", endpoint, apiResp.ErrorCode, apiResp.Description)
	}
	return nil
}
