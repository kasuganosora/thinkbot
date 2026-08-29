package misskey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ErrAlreadyReacted 表示目标帖子已被 bot 反应过（Misskey 返回 400
// ALREADY_REACTED）。属幂等成功语义，调用方据此视为「目标已达成」而非错误。
var ErrAlreadyReacted = errors.New("misskey: already reacted to that note")

// ErrSearchUnavailable 表示 notes/search 处于熔断冷却期，本次调用未真正发起请求。
// 调用方（工具层）按「搜索后端不可用」给出降级文案，引导 LLM 走其他路径。
var ErrSearchUnavailable = errors.New("misskey: note search circuit open (search backend down)")

const (
	// searchBreakerThreshold 连续失败多少次调用后熔断。
	// 一次调用内部已重试 5 次，2 次调用 = 12 次请求全败，足以判定后端是真挂而非抖动。
	searchBreakerThreshold = 2
	// searchBreakerCooldown 熔断冷却时长，覆盖 Meilisearch 重启/恢复窗口。
	searchBreakerCooldown = 5 * time.Minute
)

// searchBreaker 是 notes/search 的简易熔断器。
//
// 背景：实例搜索后端（Meilisearch）会长时间不可用——2026-08-29 全天 5 次 500，
// 每次耗尽 6 次重试耗时 13–20s，累计白烧 ~85 秒。对这种确定性的后端故障，
// 重试只是把延迟放大；熔断后直接快速失败，让 LLM 立刻转用
// users/search + users/notes（不依赖 Meilisearch）完成请求。
//
// 状态机：closed（放行）→ 连续失败达阈值 → open（冷却期内直接失败）
// → 冷却结束 → halfOpen（放行一次试探）→ 成功回 closed / 失败立刻回 open。
type searchBreaker struct {
	mu         sync.Mutex
	failures   int
	openUntil  time.Time
	halfOpen   bool // 冷却已过，本次放行的是试探请求
	lastReason string
}

// allow 返回本次调用是否放行。熔断冷却期内返回 false。
func (b *searchBreaker) allow(now time.Time) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.openUntil) {
		return false, b.lastReason
	}
	// 从未熔断过（openUntil 为零值）时是普通 closed 状态，不算试探。
	if !b.openUntil.IsZero() {
		b.halfOpen = true
	}
	return true, ""
}

// recordFailure 记录一次调用失败。达到阈值则打开熔断。
// 返回 true 表示本次失败触发/重新触发了熔断（供调用方打日志）。
func (b *searchBreaker) recordFailure(now time.Time, reason string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastReason = reason
	if b.halfOpen {
		// 半开试探失败：不必再等阈值，立刻重新熔断。
		b.halfOpen = false
		b.failures = searchBreakerThreshold
		b.openUntil = now.Add(searchBreakerCooldown)
		return true
	}
	b.failures++
	if b.failures >= searchBreakerThreshold {
		b.openUntil = now.Add(searchBreakerCooldown)
		return true
	}
	return false
}

// recordSuccess 重置连续失败计数并关闭熔断。
func (b *searchBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.halfOpen = false
	b.lastReason = ""
}

// ============================================================================
// API — Misskey HTTP API 客户端
// ============================================================================

// apiClient 封装了 Misskey HTTP API 调用。
type apiClient struct {
	client *http.Client
	// searchClient 专用于 notes/search：实例搜索后端（Meilisearch）偶发 5xx，
	// 用指数退避 + 抖动重试，让瞬时故障自愈，避免工具层直接失败。
	searchClient *http.Client
	// searchBreaker 是 notes/search 的熔断器，见类型注释。
	searchBreaker *searchBreaker
	host          string
	token         string
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
		searchBreaker: &searchBreaker{},
		host:          host,
		token:         token,
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
		body := resp.String()
		if resp.StatusCode == 400 && strings.Contains(body, "ALREADY_REACTED") {
			return fmt.Errorf("misskey createReaction: %w", ErrAlreadyReacted)
		}
		return fmt.Errorf("misskey createReaction: HTTP %d: %s", resp.StatusCode, body)
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

	// 熔断检查：冷却期内不发请求，直接快速失败，省下 6 次退避重试的 13–20s。
	if ok, reason := a.searchBreaker.allow(time.Now()); !ok {
		traceid.L(ctx).Infow("misskey searchNotes: circuit open, skipping request",
			"query", query, "reason", reason)
		return nil, ErrSearchUnavailable
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
		a.failSearch(ctx, err.Error())
		return nil, errs.Wrap(err, "misskey searchNotes")
	}
	// 5xx 是后端故障，计入熔断；4xx 是请求本身的问题（如参数非法），不熔断。
	if resp.StatusCode >= 500 {
		a.failSearch(ctx, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return nil, fmt.Errorf("misskey searchNotes: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("misskey searchNotes: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var notes []Note
	if err := resp.JSON(&notes); err != nil {
		return nil, errs.Wrap(err, "misskey searchNotes parse")
	}
	a.searchBreaker.recordSuccess()
	return notes, nil
}

// failSearch 记录一次搜索失败，并在触发熔断时打点。
func (a *apiClient) failSearch(ctx context.Context, reason string) {
	if a.searchBreaker.recordFailure(time.Now(), reason) {
		traceid.L(ctx).Warnw("misskey searchNotes: circuit opened after repeated failures",
			"reason", reason, "cooldown", searchBreakerCooldown,
			"threshold", searchBreakerThreshold)
	}
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
