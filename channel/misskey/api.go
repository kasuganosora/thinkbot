package misskey

import (
	"context"
	"fmt"

	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/errs"
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
		return nil, errs.Wrap(err, "misskey getSelf")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey getSelf: HTTP %d: %s", resp.StatusCode, resp.String())
	}

	var user User
	if err := resp.JSON(&user); err != nil {
		return nil, errs.Wrap(err, "misskey getSelf parse")
	}
	return &user, nil
}

// createNoteFull 发布帖子，支持 replyID、renoteID、CW 和文件附件。
func (a *apiClient) createNoteFull(ctx context.Context, text, replyID, renoteID, visibility, cw string, fileIDs []string) (string, error) {
	if visibility == "" {
		visibility = VisibilityPublic
	}
	resp, err := a.client.Post("notes/create").
		SetContext(ctx).
		SetJSONBody(createNoteRequest{
			I:          a.token,
			Text:       text,
			ReplyID:    replyID,
			RenoteID:   renoteID,
			Visibility: visibility,
			CW:         cw,
			FileIDs:    fileIDs,
		}).
		Do()
	if err != nil {
		return "", errs.Wrap(err, "misskey createNote")
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("misskey createNote: HTTP %d: %s", resp.StatusCode, resp.String())
	}

	var wrapper createNoteAPIResponse
	if err := resp.JSON(&wrapper); err != nil {
		return "", errs.Wrap(err, "misskey createNote parse")
	}
	return wrapper.CreatedNote.ID, nil
}

// createReaction 对帖子添加 emoji 反应。
func (a *apiClient) createReaction(ctx context.Context, noteID, reaction string) error {
	resp, err := a.client.Post("notes/reactions/create").
		SetContext(ctx).
		SetJSONBody(reactionRequest{
			I:        a.token,
			NoteID:   noteID,
			Reaction: reaction,
		}).
		Do()
	if err != nil {
		return errs.Wrap(err, "misskey createReaction")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("misskey createReaction: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// deleteReaction 移除对帖子的反应。
func (a *apiClient) deleteReaction(ctx context.Context, noteID string) error {
	resp, err := a.client.Post("notes/reactions/delete").
		SetContext(ctx).
		SetJSONBody(reactionRequest{
			I:      a.token,
			NoteID: noteID,
		}).
		Do()
	if err != nil {
		return errs.Wrap(err, "misskey deleteReaction")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("misskey deleteReaction: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}
