package telegram

import (
	"context"
	"fmt"
	"time"

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
func newAPIClient(token string, pollTimeout int, opts ...http.Option) *apiClient {
	// HTTP 客户端超时需要覆盖 long polling 等待时间 + 网络余量。
	// 设为 0（无超时）让 context 级别的超时来控制。
	httpTimeout := 0
	if pollTimeout > 0 {
		httpTimeout = pollTimeout + 15 // pollTimeout + 15s 余量
	}
	defaultOpts := []http.Option{
		http.WithBaseURL(fmt.Sprintf("%s/bot%s", apiURL, token)),
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
		return nil, fmt.Errorf("telegram getMe: %w", err)
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
		return nil, fmt.Errorf("telegram getUpdates: %w", err)
	}

	var apiResp apiResponse[[]Update]
	if err := resp.JSON(&apiResp); err != nil {
		return nil, fmt.Errorf("telegram getUpdates parse: %w", err)
	}
	if !apiResp.OK {
		return nil, fmt.Errorf("telegram getUpdates failed: [%d] %s", apiResp.ErrorCode, apiResp.Description)
	}
	return apiResp.Result, nil
}

// sendMessage 发送文本消息到指定聊天。
func (a *apiClient) sendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	req := a.client.Post("sendMessage").
		SetContext(ctx).
		SetJSONBody(sendMessageRequest{
			ChatID:           chatID,
			Text:             text,
			ReplyToMessageID: replyTo,
		})

	resp, err := req.Do()
	if err != nil {
		return 0, fmt.Errorf("telegram sendMessage: %w", err)
	}

	var apiResp apiResponse[sendMessageResult]
	if err := resp.JSON(&apiResp); err != nil {
		return 0, fmt.Errorf("telegram sendMessage parse: %w", err)
	}
	if !apiResp.OK {
		return 0, fmt.Errorf("telegram sendMessage failed: [%d] %s", apiResp.ErrorCode, apiResp.Description)
	}
	return apiResp.Result.MessageID, nil
}

// apiTimeoutMultiplier 将秒级 timeout 转为 context 超时时的缓冲余量。
// Telegram long polling 会在服务端等待 timeout 秒，客户端需要额外等待。
func apiTimeoutMultiplier(timeoutSec int) time.Duration {
	return time.Duration(timeoutSec+10) * time.Second
}
