package misskey

import (
	"context"
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// API — Misskey HTTP API 客户端
// ============================================================================

// apiClient 封装了 Misskey HTTP API 调用。
type apiClient struct {
	client *http.Client
	// searchClient 专用于 notes/search：实例搜索后端（Meilisearch）偶发 5xx，
	// 用指数退避 + 抖动重试，让瞬时故障自愈，避免工具层直接失败。
	searchClient *http.Client
	host         string
	token        string
}

// newAPIClient 创建一个 Misskey API 客户端。
// host 是 Misskey 实例的基础 URL（如 https://misskey.io）。
// token 是用户 API Token。
func newAPIClient(host, token string, opts ...http.Option) *apiClient {
	opts = append([]http.Option{
		http.WithBaseURL(host + "/api"),
		// 429/5xx + 网络错误自动按 Retry-After 退避重试；批量删帖不会再因限流硬失败。
		http.WithRetrySimple(5, 500*time.Millisecond),
	}, opts...)
	return &apiClient{
		client: http.New(opts...),
		// 搜索后端是实例 Meilisearch，偶发 5xx。单独给一个客户端走指数退避（500ms→上限 8s）+ 抖动，
		// 让瞬时故障自愈，避免每次搜索都直接失败。不继承 opts 里的 WithRetrySimple，避免双重重试层。
		// 不设 ShouldRetry：http 客户端默认仅对 5xx/429/网络错误重试，4xx 不重试。
		searchClient: http.New(
			http.WithBaseURL(host+"/api"),
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
		),
		host:  host,
		token: token,
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

// getUserNotes 获取指定用户最近发布的帖子列表。
// userId 可通过 misskey_search_user 解析得到。includeReplies 同时返回该用户的回复。
func (a *apiClient) getUserNotes(ctx context.Context, userID string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 10
	}
	resp, err := a.client.Post("users/notes").
		SetContext(ctx).
		SetJSONBody(userNotesRequest{
			I:              a.token,
			UserID:         userID,
			Limit:          limit,
			IncludeReplies: true,
		}).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "misskey getUserNotes")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey getUserNotes: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var notes []Note
	if err := resp.JSON(&notes); err != nil {
		return nil, errs.Wrap(err, "misskey getUserNotes parse")
	}
	return notes, nil
}

// searchNotes 按关键词在实例内搜索帖子。
// 走专用 searchClient（指数退避 + 抖动重试），对 Meilisearch 后端瞬时 5xx 更鲁棒。
func (a *apiClient) searchNotes(ctx context.Context, query string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 10
	}
	resp, err := a.searchClient.Post("notes/search").
		SetContext(ctx).
		SetJSONBody(noteSearchRequest{
			I:     a.token,
			Query: query,
			Limit: limit,
		}).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "misskey searchNotes")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey searchNotes: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var notes []Note
	if err := resp.JSON(&notes); err != nil {
		return nil, errs.Wrap(err, "misskey searchNotes parse")
	}
	return notes, nil
}

// mentionRequest 对应 notes/mentions（获取提及当前用户的所有帖子）。
type mentionRequest struct {
	I       string `json:"i"`
	SinceID string `json:"sinceId,omitempty"` // 仅返回该 ID 之后创建的帖子（不含该 ID 本身）
	UntilID string `json:"untilId,omitempty"` // 仅返回该 ID 之前创建的帖子（不含该 ID 本身）
	Limit   int    `json:"limit,omitempty"`
}

// getMentions 获取提及当前用户（Bot）的帖子，按时间倒序（最新在前）。
// sinceID/untilID 用于断线重连后的 backfill：传 sinceID 可拉取断连窗口内错过的 @提及/回复。
// 该端点直接查实例数据库，不依赖 Meilisearch，即使 notes/search 不可用时也能用。
func (a *apiClient) getMentions(ctx context.Context, sinceID, untilID string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 100
	}
	resp, err := a.client.Post("notes/mentions").
		SetContext(ctx).
		SetJSONBody(mentionRequest{
			I:       a.token,
			SinceID: sinceID,
			UntilID: untilID,
			Limit:   limit,
		}).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "misskey getMentions")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey getMentions: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var notes []Note
	if err := resp.JSON(&notes); err != nil {
		return nil, errs.Wrap(err, "misskey getMentions parse")
	}
	return notes, nil
}

// getNote 获取单条帖子详情（notes/show）。
// 用于回复时提取被回复者的 @ 信息，自动拼接 @ 前缀。
func (a *apiClient) getNote(ctx context.Context, noteID string) (*Note, error) {
	resp, err := a.client.Post("notes/show").
		SetContext(ctx).
		SetJSONBody(map[string]any{"i": a.token, "noteId": noteID}).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "misskey getNote")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey getNote: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var note Note
	if err := resp.JSON(&note); err != nil {
		return nil, errs.Wrap(err, "misskey getNote parse")
	}
	return &note, nil
}
