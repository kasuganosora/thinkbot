package telegram

import (
	"context"
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/errs"
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

// sendMessage 发送文本消息到指定聊天。
func (a *apiClient) sendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	return a.sendMessageFull(ctx, chatID, text, "", replyTo)
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
