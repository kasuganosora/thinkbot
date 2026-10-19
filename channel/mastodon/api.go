package mastodon

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// API — Mastodon HTTP API 客户端
// ============================================================================

// apiClient 封装了 Mastodon HTTP API 调用。
//
// Mastodon 全部端点走 Bearer Token（Authorization 头），不像 Misskey 把 token
// 放进请求体/query（?i=）。端点前缀是 /api/v1（搜索用 /api/v2）。
type apiClient struct {
	client *http.Client
	server string
	token  string

	// maxChars 是本实例单条帖子的字符上限，启动时由 GET /api/v1/instance 填充。
	// 0 表示尚未获取（或未拿到），此时回退到 defaultMaxStatusLength。
	// Mastodon 该值是**实例级配置**（默认 500，不少实例放宽到 5000），
	// 硬编码 500 会在放宽实例上白白截断长回复。
	maxChars atomic.Int32
}

// newAPIClient 创建一个 Mastodon API 客户端。
// server 是实例基础 URL（如 "https://mastodon.social"），token 是 OAuth access token。
func newAPIClient(server, token string) *apiClient {
	server = strings.TrimRight(server, "/")
	return &apiClient{
		client: http.New(
			http.WithBaseURL(server+"/api/v1"),
			// 429/5xx + 网络错误自动按 Retry-After 退避重试。
			// Mastodon 默认限流 300 req/5min，批量操作容易触发。
			http.WithRetrySimple(5, 500*time.Millisecond),
		),
		server: server,
		token:  token,
	}
}

// statusLimit 返回本实例的帖子长度上限（未探测到时为缺省值）。
func (a *apiClient) statusLimit() int {
	if n := a.maxChars.Load(); n > 0 {
		return int(n)
	}
	return defaultMaxStatusLength
}

// authRequest 给请求加上 Bearer 认证头。所有 Mastodon 端点都需要。
func (a *apiClient) authRequest(r *http.Request) *http.Request {
	if a.token != "" {
		r.BearerToken(a.token)
	}
	return r
}

// fetchInstanceLimits 拉取实例配置，填充帖子长度上限。
// 失败不阻断启动（只是退回默认长度），因为很多实例对 /api/v1/instance 有限流或反爬。
func (a *apiClient) fetchInstanceLimits(ctx context.Context) {
	var info instanceInfo
	resp, err := a.authRequest(a.client.Get("instance")).SetContext(ctx).Do()
	if err != nil {
		traceid.L(ctx).Debugw("mastodon: instance info fetch failed, using default status limit",
			"server", a.server, "err", err)
		return
	}
	if resp.StatusCode >= 400 {
		traceid.L(ctx).Debugw("mastodon: instance info returned error, using default status limit",
			"server", a.server, "status", resp.StatusCode)
		return
	}
	if err := resp.JSON(&info); err != nil {
		traceid.L(ctx).Debugw("mastodon: instance info parse failed, using default status limit",
			"server", a.server, "err", err)
		return
	}
	if n := info.Configuration.Statuses.MaxCharacters; n > 0 {
		a.maxChars.Store(int32(n))
		traceid.L(ctx).Infow("mastodon: instance status limit resolved",
			"server", a.server, "max_chars", n)
	}
}

// verifyCredentials 获取当前 Token 对应的账号（GET /api/v1/accounts/verify_credentials）。
// 等价于 Misskey 的 i（getSelf），用于 Start 时校验 token 并拿到 bot 自身 ID/acct。
func (a *apiClient) verifyCredentials(ctx context.Context) (*Account, error) {
	resp, err := a.authRequest(a.client.Get("accounts/verify_credentials")).SetContext(ctx).Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon verifyCredentials")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon verifyCredentials: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var acct Account
	if err := resp.JSON(&acct); err != nil {
		return nil, errs.Wrap(err, "mastodon verifyCredentials parse")
	}
	return &acct, nil
}

// createStatus 发布一条帖子（POST /api/v1/statuses）。
// 返回新建帖子 ID。
func (a *apiClient) createStatus(ctx context.Context, text, inReplyToID, visibility, spoiler string, mediaIDs []string) (string, error) {
	res, err := a.createStatusFull(ctx, text, inReplyToID, visibility, spoiler, mediaIDs, nil)
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

// createStatusFull 发布帖子并返回完整响应（含 poll ID，供投票结果轮询使用）。
//
// 与 Misskey 的差异：Mastodon 的回复可见性由**服务端**强制不高于父帖
// （direct 帖只能 direct 回复），传错会 422；因此 visibility 为空时必须显式
// 给 unlisted 而不是让它缺省为 public，否则会把本该私密的回复发成公开帖。
func (a *apiClient) createStatusFull(ctx context.Context, text, inReplyToID, visibility, spoiler string, mediaIDs []string, poll *pollRequest) (*createStatusResponse, error) {
	if visibility == "" {
		visibility = VisibilityUnlisted
	}
	body := createStatusRequest{
		Status:      text,
		InReplyToID: inReplyToID,
		Visibility:  visibility,
		SpoilerText: spoiler,
		MediaIDs:    mediaIDs,
		Poll:        poll,
	}
	resp, err := a.authRequest(a.client.Post("statuses")).
		SetContext(ctx).
		SetJSONBody(body).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon createStatus")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon createStatus: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var out createStatusResponse
	if err := resp.JSON(&out); err != nil {
		return nil, errs.Wrap(err, "mastodon createStatus parse")
	}
	return &out, nil
}

// getStatus 获取单条帖子详情（GET /api/v1/statuses/:id）。
// 用于回复时取被回复者 acct 以拼接 @ 前缀，以及判断 bot 是否已表态。
func (a *apiClient) getStatus(ctx context.Context, statusID string) (*Status, error) {
	resp, err := a.authRequest(a.client.Get("statuses/" + url.PathEscape(statusID))).SetContext(ctx).Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon getStatus")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon getStatus: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var st Status
	if err := resp.JSON(&st); err != nil {
		return nil, errs.Wrap(err, "mastodon getStatus parse")
	}
	return &st, nil
}

// deleteStatus 删除自己发送的帖子（DELETE /api/v1/statuses/:id）。
func (a *apiClient) deleteStatus(ctx context.Context, statusID string) error {
	resp, err := a.authRequest(a.client.Delete("statuses/" + url.PathEscape(statusID))).SetContext(ctx).Do()
	if err != nil {
		return errs.Wrap(err, "mastodon deleteStatus")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mastodon deleteStatus: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// favourite 收藏（点赞）帖子（POST /api/v1/statuses/:id/favourite）。
//
// 对等 Misskey 的 notes/reactions/create，但 Mastodon **只有一个「收藏」动作**，
// 没有自定义 emoji 反应。且服务端对重复收藏返回 200（而非 400），
// 天然幂等，不需要 Misskey 那套 ALREADY_REACTED 哨兵错误。
func (a *apiClient) favourite(ctx context.Context, statusID string) error {
	resp, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/favourite")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon favourite")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mastodon favourite: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// unfavourite 取消收藏（POST /api/v1/statuses/:id/unfavourite）。
func (a *apiClient) unfavourite(ctx context.Context, statusID string) error {
	resp, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/unfavourite")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon unfavourite")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mastodon unfavourite: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// reblog 转发（boost）帖子（POST /api/v1/statuses/:id/reblog）。
// 对等 Misskey 的 notes/create 带 renoteId。重复 boost 服务端返回 200，幂等。
func (a *apiClient) reblog(ctx context.Context, statusID string) error {
	resp, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/reblog")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon reblog")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mastodon reblog: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// unreblog 撤销转发（POST /api/v1/statuses/:id/unreblog）。
func (a *apiClient) unreblog(ctx context.Context, statusID string) error {
	resp, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/unreblog")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon unreblog")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mastodon unreblog: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// followAccount 关注账号（POST /api/v1/accounts/:id/follow）。
// locked 账号会进入「等待批准」状态（服务端返回 requested=true），不报错。
func (a *apiClient) followAccount(ctx context.Context, accountID string) error {
	resp, err := a.authRequest(a.client.Post("accounts/" + url.PathEscape(accountID) + "/follow")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon followAccount")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mastodon followAccount: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// unfollowAccount 取消关注（POST /api/v1/accounts/:id/unfollow）。
func (a *apiClient) unfollowAccount(ctx context.Context, accountID string) error {
	resp, err := a.authRequest(a.client.Post("accounts/" + url.PathEscape(accountID) + "/unfollow")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon unfollowAccount")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mastodon unfollowAccount: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	return nil
}

// searchAccounts 搜索账号（GET /api/v2/search?type=accounts）。
// resolve=true 时对形如 "alice@example.social" 的远程账号做联邦查找。
func (a *apiClient) searchAccounts(ctx context.Context, query string, limit int, resolve bool) ([]Account, error) {
	if limit <= 0 {
		limit = 10
	}
	v := url.Values{}
	v.Set("q", query)
	v.Set("type", "accounts")
	v.Set("limit", fmt.Sprintf("%d", limit))
	if resolve {
		v.Set("resolve", "true")
	}
	resp, err := a.authRequest(a.searchRequest(v)).SetContext(ctx).Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon searchAccounts")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon searchAccounts: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var out searchResult
	if err := resp.JSON(&out); err != nil {
		return nil, errs.Wrap(err, "mastodon searchAccounts parse")
	}
	return out.Accounts, nil
}

// searchStatuses 按关键词搜索帖子（GET /api/v2/search?type=statuses）。
//
// 平台限制：多数实例未开启全文搜索，此时只能命中 bot 自己发的帖子与
// 已索引内容（Mastodon 返回的是「相关」，不是严格匹配）。工具层文案需说明这一点，
// 避免 LLM 搜索不到就反复重试。
func (a *apiClient) searchStatuses(ctx context.Context, query string, limit int) ([]Status, error) {
	if limit <= 0 {
		limit = 10
	}
	v := url.Values{}
	v.Set("q", query)
	v.Set("type", "statuses")
	v.Set("limit", fmt.Sprintf("%d", limit))
	resp, err := a.authRequest(a.searchRequest(v)).SetContext(ctx).Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon searchStatuses")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon searchStatuses: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var out searchResult
	if err := resp.JSON(&out); err != nil {
		return nil, errs.Wrap(err, "mastodon searchStatuses parse")
	}
	return out.Statuses, nil
}

// searchRequest 构造 /api/v2/search 请求（搜索端点在 v2 前缀下，不在 client 的 v1 baseURL）。
func (a *apiClient) searchRequest(v url.Values) *http.Request {
	return a.client.Get(a.server + "/api/v2/search").SetQueryValues(v)
}

// getAccountStatuses 获取指定账号最近发布的帖子（GET /api/v1/accounts/:id/statuses）。
// excludeReplies=false 时也包含其回复（对等 Misskey users/notes 的 includeReplies）。
func (a *apiClient) getAccountStatuses(ctx context.Context, accountID string, limit int, excludeReplies bool) ([]Status, error) {
	if limit <= 0 {
		limit = 10
	}
	v := url.Values{}
	v.Set("limit", fmt.Sprintf("%d", limit))
	if excludeReplies {
		v.Set("exclude_replies", "true")
	}
	resp, err := a.authRequest(a.client.Get("accounts/" + url.PathEscape(accountID) + "/statuses")).
		SetContext(ctx).
		SetQueryValues(v).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon getAccountStatuses")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon getAccountStatuses: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var out []Status
	if err := resp.JSON(&out); err != nil {
		return nil, errs.Wrap(err, "mastodon getAccountStatuses parse")
	}
	return out, nil
}

// listFollowing 获取指定账号的关注列表（GET /api/v1/accounts/:id/following）。
func (a *apiClient) listFollowing(ctx context.Context, accountID string, limit int) ([]Account, error) {
	if limit <= 0 {
		limit = 10
	}
	v := url.Values{}
	v.Set("limit", fmt.Sprintf("%d", limit))
	resp, err := a.authRequest(a.client.Get("accounts/" + url.PathEscape(accountID) + "/following")).
		SetContext(ctx).
		SetQueryValues(v).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon listFollowing")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon listFollowing: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var out []Account
	if err := resp.JSON(&out); err != nil {
		return nil, errs.Wrap(err, "mastodon listFollowing parse")
	}
	return out, nil
}

// backfillNotificationTypes 是 backfill 只关心的通知类型。
// Mastodon 的通知流里 mention 才是「别人 @ 了 bot」，对应 Misskey 的 notes/mentions。
var backfillNotificationTypes = []string{"mention"}

// notificationQuery 是 GET /api/v1/notifications 的查询条件。
//
// Mastodon 的分页是半开区间：
//   - SinceID：只返回比它更新的（> since_id）
//   - MaxID：只返回比它更旧的（< max_id）
//   - MinID：只返回比它更新的（> min_id，用于向前翻页）
//
// backfill 需要 since_id + max_id 组合翻页：since_id 锚定断连窗口下界，
// max_id 逐页往更旧处取，直到取完或触及封顶。
type notificationQuery struct {
	SinceID string
	MaxID   string
	MinID   string
	Limit   int
	Types   []string
}

// getNotifications 拉取通知（GET /api/v1/notifications），按时间倒序（最新在前）。
//
// 用于断连 backfill：Mastodon streaming 同样不重放断连窗口内的事件，
// 以锚点为 since_id（开区间）把错过的 mention 补回来。
// Types 为空时返回全部类型；调用方按需传 ["mention"]。
func (a *apiClient) getNotifications(ctx context.Context, q notificationQuery) ([]Notification, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	v := url.Values{}
	v.Set("limit", fmt.Sprintf("%d", limit))
	if q.SinceID != "" {
		v.Set("since_id", q.SinceID)
	}
	if q.MaxID != "" {
		v.Set("max_id", q.MaxID)
	}
	if q.MinID != "" {
		v.Set("min_id", q.MinID)
	}
	for _, t := range q.Types {
		v.Add("types[]", t)
	}
	resp, err := a.authRequest(a.client.Get("notifications")).
		SetContext(ctx).
		SetQueryValues(v).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon getNotifications")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon getNotifications: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var out []Notification
	if err := resp.JSON(&out); err != nil {
		return nil, errs.Wrap(err, "mastodon getNotifications parse")
	}
	return out, nil
}

// getPoll 获取投票结果（GET /api/v1/polls/:id）。
//
// Mastodon **没有** pollVoted 推送事件（Misskey 有），且 poll 结果里
// 只有每个选项的票数、不暴露投票人身份。因此 user_choice 的回填只能靠
// 轮询票数变化来推断，详见 channel.go 的 pollWatchLoop。
func (a *apiClient) getPoll(ctx context.Context, pollID string) (*Poll, error) {
	resp, err := a.authRequest(a.client.Get("polls/" + url.PathEscape(pollID))).SetContext(ctx).Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon getPoll")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mastodon getPoll: HTTP %d: %s", resp.StatusCode, resp.String())
	}
	var p Poll
	if err := resp.JSON(&p); err != nil {
		return nil, errs.Wrap(err, "mastodon getPoll parse")
	}
	return &p, nil
}
