package misskey

import (
	"context"
	"fmt"

	"github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// API — Misskey HTTP API 客户端
// ============================================================================

// apiClient 封装了 Misskey HTTP API 调用。
type apiClient struct {
	client *http.Client
	host   string
	token  string
}

// newAPIClient 创建一个 Misskey API 客户端。
// host 是 Misskey 实例的基础 URL（如 https://misskey.io）。
// token 是用户 API Token。
func newAPIClient(host, token string, opts ...http.Option) *apiClient {
	opts = append([]http.Option{http.WithBaseURL(host + "/api")}, opts...)
	return &apiClient{
		client: http.New(opts...),
		host:   host,
		token:  token,
	}
}

// getSelf 获取当前 Token 对应的用户信息（用于验证 Token）。
func (a *apiClient) getSelf(ctx context.Context) (*User, error) {
	resp, err := a.client.Post("i").
		SetContext(ctx).
		SetJSONBody(getSelfRequest{I: a.token}).
		Do()
	if err != nil {
		return nil, fmt.Errorf("misskey getSelf: %w", err)
	}

	var user User
	if err := resp.JSON(&user); err != nil {
		return nil, fmt.Errorf("misskey getSelf parse: %w", err)
	}
	return &user, nil
}

// createNote 发布帖子。
func (a *apiClient) createNote(ctx context.Context, text, replyID, visibility string) (string, error) {
	if visibility == "" {
		visibility = VisibilityPublic
	}
	resp, err := a.client.Post("notes/create").
		SetContext(ctx).
		SetJSONBody(createNoteRequest{
			I:          a.token,
			Text:       text,
			ReplyID:    replyID,
			Visibility: visibility,
		}).
		Do()
	if err != nil {
		return "", fmt.Errorf("misskey createNote: %w", err)
	}

	var wrapper createNoteAPIResponse
	if err := resp.JSON(&wrapper); err != nil {
		return "", fmt.Errorf("misskey createNote parse: %w", err)
	}
	return wrapper.CreatedNote.ID, nil
}
