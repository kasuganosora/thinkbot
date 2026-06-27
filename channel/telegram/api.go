package telegram

import (
	"context"
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// API — Telegram Bot API 客户端
// ============================================================================

// apiURL 是 Telegram Bot API 的基础地址。
const apiURL = "https://api.telegram.org"

// apiClient 封装了 Telegram Bot API 的 HTTP 调用。
type apiClient struct {
	client *http.Client
	token  string
}

// newAPIClient 创建一个 Telegram Bot API 客户端。
// pollTimeout 是 long polling 超时秒数，用于将 HTTP 客户端超时设为足够大的值。
// baseURL 是 API 基础地址（默认 https://api.telegram.org）。
func newAPIClient(token string, pollTimeout int, baseURL string, opts ...http.Option) *apiClient {
	// HTTP 客户端超时需要覆盖 long polling 等待时间 + 网络余量。
	// 设为 0（无超时）让 context 级别的超时来控制。
	httpTimeout := 0
	if pollTimeout > 0 {
		httpTimeout = pollTimeout + 15 // pollTimeout + 15s 余量
	}
	if baseURL == "" {
		baseURL = apiURL
	}
	defaultOpts := []http.Option{
		http.WithBaseURL(fmt.Sprintf("%s/bot%s", baseURL, token)),
		http.WithTimeout(time.Duration(httpTimeout) * time.Second),
	}
	opts = append(defaultOpts, opts...)
	return &apiClient{
		client: http.New(opts...),
		token:  token,
	}
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
	if len(allowedUpdates) == 0 {
		allowedUpdates = []string{"message", "edited_message", "my_chat_member"}
	}
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

// sendMessageFull 发送文本消息，支持 parseMode。
func (a *apiClient) sendMessageFull(ctx context.Context, chatID int64, text, parseMode string, replyTo int64) (int64, error) {
	req := a.client.Post("sendMessage").
		SetContext(ctx).
		SetJSONBody(sendMessageRequest{
			ChatID:           chatID,
			Text:             text,
			ParseMode:        parseMode,
			ReplyToMessageID: replyTo,
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
	req := a.client.Post("editMessageText").
		SetContext(ctx).
		SetJSONBody(editMessageTextRequest{
			ChatID:    chatID,
			MessageID: messageID,
			Text:      text,
			ParseMode: parseMode,
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

// apiTimeoutMultiplier 将秒级 timeout 转为 context 超时时的缓冲余量。
// Telegram long polling 会在服务端等待 timeout 秒，客户端需要额外等待。
func apiTimeoutMultiplier(timeoutSec int) time.Duration {
	return time.Duration(timeoutSec+10) * time.Second
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

// simplePost 发送带 JSON body 的 POST 请求并检查 OK 状态。
func (a *apiClient) simplePost(ctx context.Context, endpoint string, body any) error {
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
