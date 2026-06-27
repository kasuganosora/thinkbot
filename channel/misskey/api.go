package misskey

import (
	"context"
	"fmt"

	"github.com/kasuganosora/thinkbot/util/errs"
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

// deleteNote 删除自己发送的帖子。
func (a *apiClient) deleteNote(ctx context.Context, noteID string) error {
	resp, err := a.client.Post("notes/delete").
		SetContext(ctx).
		SetJSONBody(deleteNoteRequest{I: a.token, NoteID: noteID}).
		Do()
	if err != nil {
		return errs.Wrap(err, "misskey deleteNote")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("misskey deleteNote: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
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

// followUser 关注指定用户。
func (a *apiClient) followUser(ctx context.Context, userID string) error {
	resp, err := a.client.Post("following/create").
		SetContext(ctx).
		SetJSONBody(followRequest{I: a.token, UserID: userID}).
		Do()
	if err != nil {
		return errs.Wrap(err, "misskey followUser")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("misskey followUser: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// unfollowUser 取消关注指定用户。
func (a *apiClient) unfollowUser(ctx context.Context, userID string) error {
	resp, err := a.client.Post("following/delete").
		SetContext(ctx).
		SetJSONBody(unfollowRequest{I: a.token, UserID: userID}).
		Do()
	if err != nil {
		return errs.Wrap(err, "misskey unfollowUser")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("misskey unfollowUser: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// searchUser 搜索用户。
// query: 搜索关键词（用户名/显示名）。
// limit: 返回结果数量上限（默认 10）。
func (a *apiClient) searchUser(ctx context.Context, query string, limit int) ([]UserDetail, error) {
	if limit <= 0 {
		limit = 10
	}
	resp, err := a.client.Post("users/search").
		SetContext(ctx).
		SetJSONBody(searchUserRequest{
			I:     a.token,
			Query: query,
			Limit: limit,
		}).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "misskey searchUser")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey searchUser: HTTP %d: %s", resp.StatusCode, resp.String())
	}

	var users []UserDetail
	if err := resp.JSON(&users); err != nil {
		return nil, errs.Wrap(err, "misskey searchUser parse")
	}
	return users, nil
}

// getUserDetail 获取用户详细信息。
// 预留：计划用于未来的 misskey_get_user_detail 工具。
//
//nolint:unused // 预留 API 方法
func (a *apiClient) getUserDetail(ctx context.Context, userID string) (*UserDetail, error) {
	resp, err := a.client.Post("users/show").
		SetContext(ctx).
		SetJSONBody(getUserDetailRequest{I: a.token, UserID: userID}).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "misskey getUserDetail")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey getUserDetail: HTTP %d: %s", resp.StatusCode, resp.String())
	}

	var user UserDetail
	if err := resp.JSON(&user); err != nil {
		return nil, errs.Wrap(err, "misskey getUserDetail parse")
	}
	return &user, nil
}

// listFollowing 获取指定用户的关注列表。
func (a *apiClient) listFollowing(ctx context.Context, userID string, limit int) ([]FollowingUser, error) {
	if limit <= 0 {
		limit = 10 // Misskey 默认值
	}
	resp, err := a.client.Post("users/following").
		SetContext(ctx).
		SetJSONBody(followingListRequest{
			I:      a.token,
			UserID: userID,
			Limit:  limit,
		}).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "misskey listFollowing")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey listFollowing: HTTP %d: %s", resp.StatusCode, resp.String())
	}

	var following []FollowingUser
	if err := resp.JSON(&following); err != nil {
		return nil, errs.Wrap(err, "misskey listFollowing parse")
	}
	return following, nil
}
