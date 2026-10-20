package mastodon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// API — Mastodon HTTP API 客户端
// ============================================================================
//
// ⚠️ 调用约定（与 channel/misskey/api.go 同源的血泪教训）：
// 本项目的 HTTP 客户端在 Do() 里对**任何非 2xx 都已经返回非 nil error**
// （见 util/http/client.go 的 `if !result.IsSuccess()`），错误文本里带状态码与
// 响应体。因此本文件**不要**再写 `if resp.StatusCode >= 400` —— 那个分支永远
// 走不到，写在里面的修复会静默失效（Misskey 2026-08-30 的 ALREADY_REACTED
// 幂等修复正因为写在 StatusCode 分支上而完全没生效）。
// 需要按状态码分支时，改成判断 err 是否为 errs.HTTPError 并取其状态码。

// maxPageLimit 是多数 Mastodon 列表端点（search / account statuses / following）
// 的单页上限，超出会被服务端静默收敛或报错，故调用前统一 clamp。
const maxPageLimit = 40

// ============================================================================
// 表态幂等哨兵错误
// ============================================================================
//
// ⚠️ 真实实例实测推翻了「Mastodon 重复表态天然幂等」这个想当然的前提。
// 2026-10-19 在 c.hime.at（4.3.0+community）实测矩阵：
//
//	端点            首次   重复/无效
//	favourite       200    400 {"error":"already favourited"}
//	unfavourite     200    400 {"error":"not favourited"}
//	reblog          200    400 {"error":"already reblogged"}
//	unreblog        200    400 {"error":"not reblogged"}
//
// 上游 Mastodon 对这些重复操作返回的确实是 200（幂等），但社区 fork 未必——
// 所以**两种行为都要兼容**：返回 200 时本来就是成功；返回 400 且命中上面
// 特定文案时，说明「目标状态已经达成」，同样应按成功处理。
//
// 与 Misskey 的 ErrAlreadyReacted / ErrNotReacted 完全对等：包装成哨兵错误，
// 由 channel 层（出站 Action）吞掉、工具层（LLM 可见）转成 success + 标记，
// 否则 LLM 会看到报错并反复重试 / 误判失败。
var (
	// ErrAlreadyFavourited 目标帖子已被本 bot 收藏过。
	ErrAlreadyFavourited = errors.New("mastodon: already favourited")
	// ErrNotFavourited 目标帖子本就没有本 bot 的收藏（常见于重复取消收藏）。
	ErrNotFavourited = errors.New("mastodon: not favourited")
	// ErrAlreadyReblogged 目标帖子已被本 bot 转发过。
	ErrAlreadyReblogged = errors.New("mastodon: already reblogged")
	// ErrNotReblogged 目标帖子本就没有本 bot 的转发。
	ErrNotReblogged = errors.New("mastodon: not reblogged")
)

// errHasMastodonError 判断 err 文本里是否含指定的 Mastodon error message。
//
// 只能在 HTTP 客户端返回 err 的分支上用：Do() 把 4xx 直接当 error 返回，
// 响应体只存在于 err.Error() 里（见文件头部的调用约定）。
// 大小写不敏感，避免不同实例/版本文案大小写差异导致漏判。
func errHasMastodonError(err error, msg string) bool {
	if err == nil || msg == "" {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), strings.ToLower(msg))
}

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

	// streamingURL 是实例声明的 streaming 基址，可与 API 域名不同
	// （mastodon.social 是 wss://streaming.mastodon.social）。
	// 由 fetchInstanceLimits 在 Start 里同步填充，早于任何 goroutine 启动。
	streamingURL string

	// 投票相关的实例级限制，同样由 fetchInstanceLimits 填充。
	// 未探测到时 pollLimits() 回退到官方默认值。
	pollsMaxOptions     atomic.Int32
	pollsMaxOptionChars atomic.Int32
	pollsMinExpiration  atomic.Int32
	pollsMaxExpiration  atomic.Int32
}

// 投票限制的官方默认兜底值（实例未声明 configuration.polls 时使用）。
const (
	defaultPollMaxOptions     = 4       // Mastodon 服务端硬上限
	defaultPollMaxOptionChars = 25      // 保守取官方默认，宁可截断也不要 422
	defaultPollMinExpiration  = 300     // 5 分钟，低于此值服务端 422
	defaultPollMaxExpiration  = 2629746 // 约 30.4 天
)

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
		// 探测失败一律退回默认值：这只是少了一点余量，不影响可用性。
		traceid.L(ctx).Debugw("mastodon: instance info fetch failed, using defaults",
			"server", a.server, "err", err)
		return
	}
	if err := resp.JSON(&info); err != nil {
		traceid.L(ctx).Debugw("mastodon: instance info parse failed, using defaults",
			"server", a.server, "err", err)
		return
	}
	if n := info.Configuration.Statuses.MaxCharacters; n > 0 {
		a.maxChars.Store(int32(n))
	}
	if u := strings.TrimSpace(info.URLs.StreamingAPI); u != "" {
		if safeStreamingURL(a.server, u) {
			a.streamingURL = u
		} else {
			traceid.L(ctx).Warnw("mastodon: ignoring untrusted streaming_api URL",
				"server", a.server, "streaming_api", u)
		}
	}
	if p := info.Configuration.Polls; p.MaxOptions > 0 {
		a.pollsMaxOptions.Store(int32(p.MaxOptions))
	}
	if p := info.Configuration.Polls; p.MaxCharactersPerOption > 0 {
		a.pollsMaxOptionChars.Store(int32(p.MaxCharactersPerOption))
	}
	if p := info.Configuration.Polls; p.MinExpiration > 0 {
		a.pollsMinExpiration.Store(int32(p.MinExpiration))
	}
	if p := info.Configuration.Polls; p.MaxExpiration > 0 {
		a.pollsMaxExpiration.Store(int32(p.MaxExpiration))
	}
	traceid.L(ctx).Infow("mastodon: instance limits resolved",
		"server", a.server,
		"max_chars", a.statusLimit(),
		"streaming", a.streamingEndpoint(),
		"poll_options", a.pollMaxOptions(),
		"poll_option_chars", a.pollMaxOptionChars())
}

// safeStreamingURL reports whether the instance-advertised streaming URL may
// receive our bearer token: it must use a secure scheme (unless the server
// itself is plain http) and its host must be the server host or a subdomain
// of it. Anything else would let a hostile instance response redirect the
// token or our connection to an arbitrary (possibly internal) host.
func safeStreamingURL(server, streaming string) bool {
	su, err := url.Parse(server)
	if err != nil || su.Hostname() == "" {
		return false
	}
	u, err := url.Parse(streaming)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
	case "ws", "http":
		if strings.ToLower(su.Scheme) != "http" {
			return false
		}
	default:
		return false
	}
	sh, h := strings.ToLower(su.Hostname()), strings.ToLower(u.Hostname())
	return h == sh || strings.HasSuffix(h, "."+sh)
}

// streamingEndpoint 返回 WebSocket streaming 的完整 URL。
// 优先用实例声明的独立 streaming 域名，否则回退到 API 域名下 /api/v1/streaming。
func (a *apiClient) streamingEndpoint() string {
	if a.streamingURL != "" {
		return strings.TrimRight(a.streamingURL, "/") + "/api/v1/streaming"
	}
	return a.server + "/api/v1/streaming"
}

// pollMaxOptions 返回单个投票允许的选项数上限。
func (a *apiClient) pollMaxOptions() int {
	if n := a.pollsMaxOptions.Load(); n > 0 {
		return int(n)
	}
	return defaultPollMaxOptions
}

// pollMaxOptionChars 返回单个投票选项的字符上限。
func (a *apiClient) pollMaxOptionChars() int {
	if n := a.pollsMaxOptionChars.Load(); n > 0 {
		return int(n)
	}
	return defaultPollMaxOptionChars
}

// clampPollExpiration 把投票存活秒数收敛到实例允许的区间。
// Mastodon 对该字段上下限都做校验（默认 300 ~ 2629746 秒），越界即 422。
func (a *apiClient) clampPollExpiration(secs int) int {
	minExp := defaultPollMinExpiration
	if n := a.pollsMinExpiration.Load(); n > 0 {
		minExp = int(n)
	}
	maxExp := defaultPollMaxExpiration
	if n := a.pollsMaxExpiration.Load(); n > 0 {
		maxExp = int(n)
	}
	if secs <= 0 {
		return minExp
	}
	if secs < minExp {
		return minExp
	}
	if secs > maxExp {
		return maxExp
	}
	return secs
}

// verifyCredentials 获取当前 Token 对应的账号（GET /api/v1/accounts/verify_credentials）。
// 等价于 Misskey 的 i（getSelf），用于 Start 时校验 token 并拿到 bot 自身 ID/acct。
func (a *apiClient) verifyCredentials(ctx context.Context) (*Account, error) {
	resp, err := a.authRequest(a.client.Get("accounts/verify_credentials")).SetContext(ctx).Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon verifyCredentials")
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
	// Idempotency-Key：POST /api/v1/statuses 是**非幂等**写操作，而本项目的 HTTP
	// 客户端会对网络错误 / 5xx / 429 自动重试。一旦首次请求已被服务端处理但响应
	// 丢失，重试就会再发一条一模一样的帖子——用户会看到重复内容，且这是最难排查
	// 的一类重复外发。Mastodon 官方为此提供该头（去重窗口 1 小时）；重试复用同一个
	// Request，因此同一个 key 会随每次重试一起重发，正好命中官方的去重语义。
	resp, err := a.authRequest(a.client.Post("statuses")).
		SetContext(ctx).
		SetHeader("Idempotency-Key", idgen.New("mdst")).
		SetJSONBody(body).
		Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon createStatus")
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
	var st Status
	if err := resp.JSON(&st); err != nil {
		return nil, errs.Wrap(err, "mastodon getStatus parse")
	}
	return &st, nil
}

// deleteStatus 删除自己发送的帖子（DELETE /api/v1/statuses/:id）。
func (a *apiClient) deleteStatus(ctx context.Context, statusID string) error {
	_, err := a.authRequest(a.client.Delete("statuses/" + url.PathEscape(statusID))).SetContext(ctx).Do()
	if err != nil {
		return errs.Wrap(err, "mastodon deleteStatus")
	}
	return nil
}

// favourite 收藏（点赞）帖子（POST /api/v1/statuses/:id/favourite）。
//
// 对等 Misskey 的 notes/reactions/create，但 Mastodon **只有一个「收藏」动作**，
// 没有自定义 emoji 反应。
//
// ⚠️ 重复收藏**并不天然幂等**：上游 Mastodon 返回 200，但社区 fork 会返回
// 400 {"error":"already favourited"}（2026-10-19 实测，见文件顶部矩阵）。
// 因此这里同样要 Misskey 那套哨兵错误，由调用方按「目标已达成」处理。
func (a *apiClient) favourite(ctx context.Context, statusID string) error {
	_, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/favourite")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		// 幂等语义：目标状态已达成（如重复收藏 / 取消一个不存在的收藏）。
		// 见文件头部「表态幂等哨兵错误」的实测矩阵——上游 Mastodon 返回 200，
		// 社区 fork 返回 400 + "already favourited"，两者都要兼容。
		if errHasMastodonError(err, "already favourited") {
			return fmt.Errorf("mastodon favourite: %w", ErrAlreadyFavourited)
		}
		return errs.Wrap(err, "mastodon favourite")
	}
	return nil
}

// unfavourite 取消收藏（POST /api/v1/statuses/:id/unfavourite）。
func (a *apiClient) unfavourite(ctx context.Context, statusID string) error {
	_, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/unfavourite")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		// 幂等语义：目标状态已达成（如重复收藏 / 取消一个不存在的收藏）。
		// 见文件头部「表态幂等哨兵错误」的实测矩阵——上游 Mastodon 返回 200，
		// 社区 fork 返回 400 + "not favourited"，两者都要兼容。
		if errHasMastodonError(err, "not favourited") {
			return fmt.Errorf("mastodon unfavourite: %w", ErrNotFavourited)
		}
		return errs.Wrap(err, "mastodon unfavourite")
	}
	return nil
}

// reblog 转发（boost）帖子（POST /api/v1/statuses/:id/reblog）。
// 对等 Misskey 的 notes/create 带 renoteId。
// ⚠️ 重复 boost 同样不天然幂等（fork 会 400 {"error":"already reblogged"}），
// 由哨兵错误 ErrAlreadyReblogged 兜住。
func (a *apiClient) reblog(ctx context.Context, statusID string) error {
	_, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/reblog")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		// 幂等语义：目标状态已达成（如重复收藏 / 取消一个不存在的收藏）。
		// 见文件头部「表态幂等哨兵错误」的实测矩阵——上游 Mastodon 返回 200，
		// 社区 fork 返回 400 + "already reblogged"，两者都要兼容。
		if errHasMastodonError(err, "already reblogged") {
			return fmt.Errorf("mastodon reblog: %w", ErrAlreadyReblogged)
		}
		return errs.Wrap(err, "mastodon reblog")
	}
	return nil
}

// unreblog 撤销转发（POST /api/v1/statuses/:id/unreblog）。
func (a *apiClient) unreblog(ctx context.Context, statusID string) error {
	_, err := a.authRequest(a.client.Post("statuses/" + url.PathEscape(statusID) + "/unreblog")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		// 幂等语义：目标状态已达成（如重复收藏 / 取消一个不存在的收藏）。
		// 见文件头部「表态幂等哨兵错误」的实测矩阵——上游 Mastodon 返回 200，
		// 社区 fork 返回 400 + "not reblogged"，两者都要兼容。
		if errHasMastodonError(err, "not reblogged") {
			return fmt.Errorf("mastodon unreblog: %w", ErrNotReblogged)
		}
		return errs.Wrap(err, "mastodon unreblog")
	}
	return nil
}

// followAccount 关注账号（POST /api/v1/accounts/:id/follow）。
// locked 账号会进入「等待批准」状态（服务端返回 requested=true），不报错。
func (a *apiClient) followAccount(ctx context.Context, accountID string) error {
	_, err := a.authRequest(a.client.Post("accounts/" + url.PathEscape(accountID) + "/follow")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon followAccount")
	}
	return nil
}

// unfollowAccount 取消关注（POST /api/v1/accounts/:id/unfollow）。
func (a *apiClient) unfollowAccount(ctx context.Context, accountID string) error {
	_, err := a.authRequest(a.client.Post("accounts/" + url.PathEscape(accountID) + "/unfollow")).
		SetContext(ctx).
		SetJSONBody(map[string]string{}).
		Do()
	if err != nil {
		return errs.Wrap(err, "mastodon unfollowAccount")
	}
	return nil
}

// searchAccounts 搜索账号（GET /api/v2/search?type=accounts）。
// resolve=true 时对形如 "alice@example.social" 的远程账号做联邦查找。
func (a *apiClient) searchAccounts(ctx context.Context, query string, limit int, resolve bool) ([]Account, error) {
	if limit <= 0 {
		limit = 10
	}
	// 多数列表端点单页上限 40，LLM 可能传更大的值
	if limit > maxPageLimit {
		limit = maxPageLimit
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
	// 多数列表端点单页上限 40，LLM 可能传更大的值
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	v := url.Values{}
	v.Set("q", query)
	v.Set("type", "statuses")
	v.Set("limit", fmt.Sprintf("%d", limit))
	resp, err := a.authRequest(a.searchRequest(v)).SetContext(ctx).Do()
	if err != nil {
		return nil, errs.Wrap(err, "mastodon searchStatuses")
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
	// 多数列表端点单页上限 40，LLM 可能传更大的值
	if limit > maxPageLimit {
		limit = maxPageLimit
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
	// 多数列表端点单页上限 40，LLM 可能传更大的值
	if limit > maxPageLimit {
		limit = maxPageLimit
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
	var p Poll
	if err := resp.JSON(&p); err != nil {
		return nil, errs.Wrap(err, "mastodon getPoll parse")
	}
	return &p, nil
}
