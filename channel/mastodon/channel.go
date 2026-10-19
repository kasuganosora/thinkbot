package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// MastodonChannel — Mastodon（长毛象）平台适配器
// ============================================================================
//
// 与 channel/misskey 对等：WebSocket streaming 入站 + REST 出站 + 平台工具。
// 平台差异（不可对等的点）在各自函数注释里标注，汇总见 README.md。

const (
	// defaultMaxStatusLength 未探测到实例配置时的帖子长度上限。
	// Mastodon 官方默认 500，但实例可放宽（有实例到 5000），
	// 故 Start 时会用 GET /api/v1/instance 覆盖此值。
	defaultMaxStatusLength = 500
	// 指数退避重连参数（与 Misskey 一致）。
	mastodonReconnectDelayMin = 5 * time.Second
	mastodonReconnectDelayMax = 5 * time.Minute
	// 去重缓存 TTL 和清理间隔。
	mastodonDedupTTL          = 2 * time.Minute
	mastodonDedupCleanupEvery = 30 * time.Second
	// replyContextTTL 被回复帖缓存的有效期（避免同一父帖被反复拉取）。
	replyContextTTL = 10 * time.Minute
	// pollWatchInterval 投票结果的轮询间隔。
	// Mastodon 没有 pollVoted 推送事件，只能轮询票数变化来推断投票。
	//
	// 5 秒是配额算出来的：Mastodon 标准限流 300 req/5min（≈1 req/s），
	// 而一次 user_choice 最短也要存活 300 秒。轮询是**共享**的单 ticker，
	// 单跳只花 O(在途投票数) 个请求，故稳态占用 = 0.2 req/s，
	// 给发帖/点赞/取父帖留足余量。别再往下调——早期 3 秒 + 每条投票一个
	// goroutine 的组合下，3 条并发投票就打满配额，其余 API 连带 429。
	pollWatchInterval = 5 * time.Second
	// pollMultiDebounce 多选投票的合并窗口（与 Misskey 一致）。
	pollMultiDebounce = 3 * time.Second
)

// mentionAnyRe 匹配文本中的 @username 或 @username@host 提及（不分用户）。
// Mastodon 的 acct 形如 "alice@example.social"，域含点，故字符集比 Misskey 更宽。
var mentionAnyRe = regexp.MustCompile(`@[A-Za-z0-9_]+(@[A-Za-z0-9.\-]+)?`)

// Config 配置 MastodonChannel。
type Config struct {
	// Server Mastodon 实例 URL（如 "https://mastodon.social"）。
	Server string

	// Token OAuth access token（需含 read/write/statuses 等 scope）。
	Token string

	// WatchdogTimeout WebSocket 看门狗超时。0 = 使用 120s 默认值。
	WatchdogTimeout time.Duration

	// PingInterval WebSocket 自动 Ping 间隔。0 = 使用 30s 默认值。
	PingInterval time.Duration

	// ReconnectDelay 断线后重连间隔。0 = 使用 5s 默认值。
	ReconnectDelay time.Duration

	// Streams 需要额外订阅的公共时间线流。
	// 合法值：user（关注时间线）/ public（联邦）/ public:local（本站）/ public:remote / direct（私信）。
	// 留空则仅通过通知流接收 @提及 / 回复 / 收藏。
	Streams []string

	// ResolveReplyContext 是否在入站回复时拉取父帖构造 [Reply to X] 上下文。
	// Mastodon 的 streaming 事件**不内联父帖**（Misskey 内联 reply 对象），
	// 想知道别人在回复什么必须额外查一次 API。关闭可省 API 配额（默认 true）。
	ResolveReplyContext bool
}

// validStreams 是本 channel 允许订阅的公共时间线流。
// 通知流（user:notification）始终强制订阅，不接受配置——那是 @提及/回复的唯一入口。
var validStreams = map[string]bool{
	"user":          true, // 关注（主页）时间线
	"public":        true, // 联邦时间线
	"public:local":  true, // 本站时间线
	"public:remote": true, // 联邦远端时间线
	"direct":        true, // 私信
}

// notificationStream 是强制订阅的通知流：mention / 回复 / 收藏都从这里来。
const notificationStream = "user:notification"

// MastodonChannel 是 Mastodon 平台的输入端与输出端实现。
//
// 入站：WebSocket streaming（通知流 + 可选公共时间线流）→ 归一化为 core.Message → Ingress
// 出站：REST API（发贴/回复/收藏/转发/投票），实现 bot.Sender
//
// 使用示例：
//
//	ch := mastodon.NewChannel("my-md-bot", "my-bot-id", mastodon.Config{
//	    Server: "https://mastodon.social",
//	    Token:  "xxxxxxxx...",
//	})
//	bot, _ := bot.New(bot.BotParams{ID: "my-bot-id", Channels: []bot.Channel{ch}})
//	go bot.Run(ctx)
type MastodonChannel struct {
	name  string
	botID string
	cfg   Config
	api   *apiClient
	hc    *http.Client

	// botAccountID 是 Bot 自身的账号 ID，在 Start 时通过 verifyCredentials 获取。
	botAccountID string
	// botAcct 是 Bot 的完整账号标识（"bot" 或 "bot@example.social"）。
	botAcct string
	// botUsername 是 Bot 的本地用户名（不含域）。
	botUsername string
	// mentionRe 匹配 @botAcct / @botUsername 或带域形式的提及。
	mentionRe *regexp.Regexp

	ingress *inbound.Ingress

	// 去重缓存：statusID -> 入时间。防止 notification 与 timeline 双路径投递同一帖。
	// 注意锚点用的是 notification ID（见 lastNotificationID），两者不可混用。
	dedupMu sync.Mutex
	dedup   map[string]time.Time

	// 锚点：最近一次成功处理的 mention 通知 ID。
	// Mastodon streaming 同样不重放断连窗口内的事件，重连后以它为 since_id backfill。
	// 必须是 notification ID —— notifications 端点的 since_id 只认通知 ID，
	// 拿 status ID 当锚点会拉到完全不相干的区间。
	anchorMu           sync.Mutex
	lastNotificationID string

	// replyCache 缓存已拉取过的父帖，避免同一帖被反复查询。
	// Mastodon 限流 300 req/5min，公共时间线流量大时额外查询很容易触顶。
	replyMu    sync.Mutex
	replyCache map[string]*replyCacheEntry

	// mu 保护身份字段（botAccountID/botAcct/botUsername/mentionRe）、ingress 与 stopped。
	//
	// ⚠️ 这些字段在 Start（持 c.mu）里写，但被**工具执行 goroutine**、**投票轮询
	// goroutine** 和 **streaming 读循环**并发读。裸读会构成数据竞争（-race 可复现），
	// 且工具可能在 Start 完成前就被调用从而读到空身份。统一走下面这几个只读访问器。
	mu      sync.RWMutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool

	// 注：Start 派生的 runCtx 只以参数形式传给各个长生命周期 goroutine，
	// 不存字段——存了就得加锁读，而目前没有任何路径需要外部再取它。
	// 投票结果回填用的是共享轮询循环的 ctx（= runCtx），天然长生命周期，
	// 不会跟着某一轮 LLM 调用的 ctx 一起被取消。

	// streamConnects 累计成功建立的 streaming 连接次数。
	streamConnects atomic.Int64

	// polls 追踪 bot 发出的投票帖及其轮询 goroutine。
	pollsMu sync.Mutex
	polls   map[string]*pollState
}

// replyCacheEntry 是父帖缓存项。
type replyCacheEntry struct {
	status *Status
	at     time.Time
}

// pollState 是一条 bot 投票帖的进行中状态。
//
// 与 Misskey 的差异：Mastodon 不推送 pollVoted 事件，也没有「谁投了」信息，
// 所以这里多了一个后台轮询 goroutine（watchCancel）与票数基线（baseline），
// 靠「票数比基线多了多少」推断有人投票。
type pollState struct {
	QuestionID string
	Multiple   bool
	OptionN    int
	PollID     string
	// baseline 是各选项的初始票数快照，用于算增量。
	baseline []int
	Selected []int
	timer    *time.Timer // 多选 debounce
	expire   *time.Timer // 无人投票时回收，避免泄漏
}

// NewChannel 创建一个 MastodonChannel。
func NewChannel(name, botID string, cfg Config) *MastodonChannel {
	if cfg.WatchdogTimeout <= 0 {
		cfg.WatchdogTimeout = 120 * time.Second
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = 5 * time.Second
	}
	cfg.Streams = normalizeStreams(cfg)
	return &MastodonChannel{
		name:       name,
		botID:      botID,
		cfg:        cfg,
		hc:         http.New(),
		api:        newAPIClient(cfg.Server, cfg.Token),
		polls:      make(map[string]*pollState),
		replyCache: make(map[string]*replyCacheEntry),
	}
}

// normalizeStreams 过滤非法流名并去重，保持稳定顺序。
func normalizeStreams(cfg Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range cfg.Streams {
		if validStreams[s] && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Name 返回 Channel 名称。
func (c *MastodonChannel) Name() string { return c.name }

// Type 返回 "mastodon"。
func (c *MastodonChannel) Type() string { return "mastodon" }

// BotID 返回所属 Bot ID。
func (c *MastodonChannel) BotID() string { return c.botID }

// Start 启动 WebSocket streaming 循环（非阻塞）。
func (c *MastodonChannel) Start(ctx context.Context, ingress *inbound.Ingress) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("mastodon channel: already stopped, cannot restart")
	}

	c.ingress = ingress

	// 验证 Token 并拿到 Bot 自身身份
	me, err := c.api.verifyCredentials(ctx)
	if err != nil {
		return errs.Wrap(err, "mastodon channel: token validation failed")
	}
	traceid.L(ctx).Infow("mastodon channel started",
		"channel", c.name, "acct", me.Acct, "server", c.cfg.Server)
	c.botAccountID = me.ID
	c.botAcct = me.Acct
	c.botUsername = me.Username
	c.dedup = make(map[string]time.Time)

	// 帖子长度上限是实例级配置，启动时探测一次；失败则退回 500。
	c.api.fetchInstanceLimits(ctx)

	// 种子锚点：启动即拉最新一条 mention 通知，作为后续重连 backfill 的基准。
	if notes, err := c.api.getNotifications(ctx, notificationQuery{Limit: 1, Types: backfillNotificationTypes}); err != nil {
		traceid.L(ctx).Warnw("mastodon: seed mention anchor failed (backfill baseline unavailable until first live mention)",
			"channel", c.name, "err", err)
	} else if len(notes) > 0 {
		c.setNotificationAnchor(notes[0].ID)
	}

	// 注册 Bot 自身账号 ID 到 Ingress，作为防止自回复循环的第二道防线。
	ingress.RegisterSelfUserID(me.ID)

	// 编译 @bot 正则：同时匹配 @username 与 @acct（带域）两种写法，
	// 保证不会误匹配更长的用户名（@botuser 不匹配 @bot）。
	c.mentionRe = buildMentionRe(me.Username, me.Acct)

	// 注册 Mastodon 原生投票创建器，供 user_choice 工具在本平台使用。
	interaction.RegisterPollCreator("mastodon", c.CreatePollStatus)

	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	c.wg.Add(1)
	go c.cleanupLoop(runCtx)

	c.wg.Add(1)
	go c.streamLoop(runCtx)

	// 投票轮询是共享的单个 ticker，随 channel 生命周期走，不按投票数扩 goroutine。
	c.wg.Add(1)
	go c.pollWatchLoop(runCtx)

	return nil
}

// buildMentionRe 构造匹配「@本 bot」的正则，覆盖 @username 与 @acct@host 两种形态。
func buildMentionRe(username, acct string) *regexp.Regexp {
	var alts []string
	if username != "" {
		alts = append(alts, regexp.QuoteMeta(username))
	}
	if acct != "" && acct != username {
		alts = append(alts, regexp.QuoteMeta(acct))
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`@(?:` + strings.Join(alts, "|") + `)\b`)
}

// streamLoop 维护 WebSocket 连接，断线自动重连（指数退避）。
func (c *MastodonChannel) streamLoop(ctx context.Context) {
	defer c.wg.Done()

	delay := mastodonReconnectDelayMin
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now()
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return // 主动关闭
		}

		if err != nil {
			traceid.L(ctx).Warnw("mastodon stream disconnected",
				"channel", c.name, "err", err, "reconnect_delay", delay)
		}

		if time.Since(start) > mastodonReconnectDelayMax {
			delay = mastodonReconnectDelayMin
		} else if err == nil {
			delay = mastodonReconnectDelayMin
		}

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}

		delay *= 2
		if delay > mastodonReconnectDelayMax {
			delay = mastodonReconnectDelayMax
		}
	}
}

// cleanupLoop 定期清理过期的去重缓存与父帖缓存。
func (c *MastodonChannel) cleanupLoop(ctx context.Context) {
	defer c.wg.Done()
	ticker := time.NewTicker(mastodonDedupCleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			c.dedupMu.Lock()
			for id, ts := range c.dedup {
				if now.Sub(ts) > mastodonDedupTTL {
					delete(c.dedup, id)
				}
			}
			c.dedupMu.Unlock()

			c.replyMu.Lock()
			for id, e := range c.replyCache {
				if now.Sub(e.at) > replyContextTTL {
					delete(c.replyCache, id)
				}
			}
			c.replyMu.Unlock()
		}
	}
}

// dedupSeen 检查 statusID 是否在去重窗口内已处理过，未处理则标记为已处理。
func (c *MastodonChannel) dedupSeen(statusID string) bool {
	if statusID == "" {
		return false
	}
	c.dedupMu.Lock()
	defer c.dedupMu.Unlock()
	if ts, seen := c.dedup[statusID]; seen && time.Since(ts) < mastodonDedupTTL {
		return true
	}
	c.dedup[statusID] = time.Now()
	return false
}

// getNotificationAnchor 返回当前锚点（最近一次成功处理的 mention 通知 ID）。
func (c *MastodonChannel) getNotificationAnchor() string {
	c.anchorMu.Lock()
	defer c.anchorMu.Unlock()
	return c.lastNotificationID
}

// setNotificationAnchor 推进锚点（空值不更新）。
//
// 单调递增守卫：Mastodon ID 与 Misskey aid 一样是定长雪花 ID、按时间字典序递增，
// 锚点只允许向更新的 ID 推进。若允许回退，一条乱序到达的旧通知会把锚点拉回去，
// 下次 backfill 就会重复拉取已处理的 mention——这正是 Misskey 2026-09-01 事故的
// 根因（2 分钟 dedup 窗口挡不住几十分钟后的重放，导致重复回复外发）。
func (c *MastodonChannel) setNotificationAnchor(id string) {
	if id == "" {
		return
	}
	c.anchorMu.Lock()
	defer c.anchorMu.Unlock()
	if c.lastNotificationID != "" && !newerID(id, c.lastNotificationID) {
		return
	}
	c.lastNotificationID = id
}

// newerID 判断雪花 ID a 是否严格新于 b。
// 两者都是十进制数字字符串：先比长度（位数多者更大），长度相同再按字典序。
// 非数字（理论上不应出现）退化为字典序比较。
func newerID(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// backfillMentions 重连后补发断连窗口内错过的 @提及/回复。
//
// Mastodon streaming 与 Misskey 一样不重放历史消息。本方法以锚点为 since_id
// （开区间）翻页拉 notifications?types[]=mention 补齐。
// 翻页：结果按时间倒序（最新在前），用 max_id 取更旧的一页，封顶 maxTotal。
func (c *MastodonChannel) backfillMentions(ctx context.Context) {
	anchor := c.getNotificationAnchor()
	if anchor == "" {
		traceid.L(ctx).Debugw("mastodon backfill: skipped (no anchor yet)", "channel", c.name)
		return
	}
	// pageSize 与服务端单次上限（maxPageLimit=40）对齐，避免请求被静默截断后
	// 误以为已经翻完（len(notes) < pageSize 是终止条件之一）。
	//
	// maxTotal 是有意的封顶：每条 mention 还会额外触发一次 getStatus 取父帖上下文
	// （ResolveReplyContext）。断连窗口若积压几百条，这里会在几十毫秒内打出
	// 数百个请求，直接撞 300 req/5min 的限流，把 backfill 自己打挂，
	// 连累同进程其它 API。超出部分**有意丢弃**——它们仍在时间线流里可见，
	// 且锚点已推进到最新，不会重复处理。
	const pageSize = 40
	const maxTotal = 100
	total := 0
	maxID := ""
	for total < maxTotal {
		notes, err := c.api.getNotifications(ctx, notificationQuery{
			SinceID: anchor, // 只取断连窗口内（比锚点更新）的通知
			MaxID:   maxID,  // 逐页往更旧处翻
			Limit:   pageSize,
			Types:   backfillNotificationTypes,
		})
		if err != nil {
			traceid.L(ctx).Warnw("mastodon backfill: notifications failed",
				"channel", c.name, "anchor", anchor, "err", err)
			return
		}
		if len(notes) == 0 {
			break
		}
		// 倒序返回，按时间正序（最旧→最新）处理，锚点自然推进到最新。
		for i := len(notes) - 1; i >= 0; i-- {
			n := notes[i]
			if n.Status == nil {
				c.setNotificationAnchor(n.ID)
				continue
			}
			if c.dedupSeen(n.Status.ID) {
				c.setNotificationAnchor(n.ID)
				continue
			}
			if n.Account.ID == c.selfAccountID() {
				c.setNotificationAnchor(n.ID)
				continue
			}
			c.handleStatus(ctx, n.Status, "mention", true, n.ID)
			c.setNotificationAnchor(n.ID)
		}
		total += len(notes)
		if len(notes) < pageSize {
			break
		}
		maxID = notes[len(notes)-1].ID
	}
	if total > 0 {
		traceid.L(ctx).Infow("mastodon backfill: recovered missed mentions after reconnect",
			"channel", c.name, "count", total, "anchor", anchor)
		return
	}
	traceid.L(ctx).Infow("mastodon backfill: no missed mentions in disconnect window",
		"channel", c.name, "anchor", anchor)
}

// connectAndServe 建立 WebSocket 连接并持续处理消息。
// 阻塞直到连接断开或 ctx 被取消。
func (c *MastodonChannel) connectAndServe(ctx context.Context) error {
	// 订阅流：通知流（强制）+ 配置的公共时间线流。
	//
	// ⚠️ 不能靠重复 `?stream=a&stream=b` 表达多订阅：Mastodon streaming 服务端
	// 用 firstParam(query.stream) **只取第一个** stream 参数，其余被静默丢弃
	// ——连接正常、心跳正常、无 4xx，但配置的时间线/私信一条都收不到。
	// 官方的多流做法是在连接建立后发 `{"type":"subscribe","stream":"..."}` 帧。
	//
	// 因此：第一个流（通知流）仍走 query 参数作兼容性兜底——即使对端不支持
	// subscribe 帧，@提及这条命脉也还在；其余流一律走 subscribe 帧。
	streams := append([]string{notificationStream}, c.cfg.Streams...)
	q := url.Values{}
	q.Add("access_token", c.cfg.Token)
	q.Add("stream", streams[0])

	cfg := http.WSConfig{
		WatchdogTimeout: c.cfg.WatchdogTimeout,
		PingInterval:    c.cfg.PingInterval,
		OnConnect: func(conn *http.WSConn) {
			// 订阅其余流（streams[0] 已由 query 参数订阅）。
			for _, s := range streams[1:] {
				msg := fmt.Sprintf(`{"type":"subscribe","stream":%q}`, s)
				if err := conn.WriteText(msg); err != nil {
					traceid.L(ctx).Warnw("mastodon stream: failed to subscribe",
						"channel", c.name, "stream", s, "err", err)
					continue
				}
				traceid.L(ctx).Debugw("mastodon stream: subscribed", "channel", c.name, "stream", s)
			}

			n := c.streamConnects.Add(1)
			traceid.L(ctx).Infow("mastodon stream connected",
				"channel", c.name, "connect_seq", n, "reconnect", n > 1, "streams", streams)
			// 重连后补发断连窗口内错过的 mention（streaming 不重放历史）。
			c.backfillMentions(ctx)
		},
		OnClose: func(code int, text string) {
			traceid.L(ctx).Debugw("mastodon stream closed", "channel", c.name, "code", code)
		},
		OnError: func(err error) {
			traceid.L(ctx).Debugw("mastodon ws error", "channel", c.name, "err", err)
		},
		OnText: func(text string) error {
			return c.handleStreamMessage(ctx, text)
		},
	}

	// DoWS 会阻塞直到连接关闭。实例可能把 streaming 部署在独立域名上
	// （mastodon.social 就是 wss://streaming.mastodon.social），故用探测到的
	// streaming endpoint，探测不到才回退到 API 域名下的 /api/v1/streaming。
	return c.api.authRequest(c.hc.Get(c.api.streamingEndpoint())).
		SetContext(ctx).
		SetQueryValues(q).
		DoWS(cfg)
}

// handleStreamMessage 处理一条来自 streaming 的文本消息。
//
// Mastodon 的事件格式是 {event, payload}，payload 是 JSON 编码的字符串；
// 另有以 ":" 开头的纯文本心跳（":ping" / ":thump"），不是 JSON，需先挡掉。
func (c *MastodonChannel) handleStreamMessage(ctx context.Context, text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	// 非 JSON 的心跳帧（":ping" / ":thump"）
	if strings.HasPrefix(trimmed, ":") {
		return nil
	}

	var ev streamEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		traceid.L(ctx).Debugw("mastodon stream: failed to parse message",
			"channel", c.name, "raw", truncateForLog(trimmed), "err", err)
		return nil // 不中断连接
	}

	switch ev.Event {
	case "", "ping", "thump", "filters_changed":
		// 心跳与过滤规则变更：无内容，忽略。
		return nil
	case "delete":
		// 帖子被删除：payload 是纯 ID 文本。当前不做撤回处理，仅记日志。
		traceid.L(ctx).Debugw("mastodon stream: status deleted",
			"channel", c.name, "status_id", payloadID(ev.Payload))
		return nil
	case "notification":
		var n Notification
		if err := decodePayload(ev.Payload, &n); err != nil {
			traceid.L(ctx).Debugw("mastodon stream: failed to decode notification",
				"channel", c.name, "err", err)
			return nil
		}
		c.handleNotification(ctx, &n)
		return nil
	case "update":
		var st Status
		if err := decodePayload(ev.Payload, &st); err != nil {
			traceid.L(ctx).Debugw("mastodon stream: failed to decode update",
				"channel", c.name, "err", err)
			return nil
		}
		kind, mentioned := streamKind(ev.Stream)
		c.handleTimelineStatus(ctx, &st, kind, mentioned)
		return nil
	default:
		return nil
	}
}

// streamKind 根据事件所属流判定入站场景。
// 返回 (eventType, mentioned)：
//   - direct 流：私信，视为被直接点名（mentioned=true）
//   - user / public* 流：公共时间线，是否被点名由 timelineMentioned 另行判定
func streamKind(streams []string) (string, bool) {
	for _, s := range streams {
		switch {
		case strings.HasPrefix(s, "direct"):
			return "direct", true
		case s == "user" || strings.HasPrefix(s, "public"):
			return "timeline", false
		}
	}
	return "timeline", false
}

// handleTimelineStatus 处理公共时间线/私信流上的一条帖子。
func (c *MastodonChannel) handleTimelineStatus(ctx context.Context, st *Status, kind string, directMentioned bool) {
	if st == nil {
		return
	}
	if c.dedupSeen(st.ID) {
		return
	}
	// 公共时间线上自己的帖子不入站（私信流理论上也不含自己，保险起见一并挡）。
	if st.Account.ID == c.selfAccountID() {
		return
	}
	// 空帖（纯媒体且无正文、也无被转发内容）不产生对话内容，跳过。
	if isBlankContent(st.Content) && len(st.MediaAttachments) == 0 && st.Reblog == nil {
		return
	}

	mentioned := directMentioned || c.timelineMentioned(st)
	c.handleStatus(ctx, st, kind, mentioned, "")
}

// timelineMentioned 判断一条时间线帖子是否明确指向本 Bot。
//
// 与 Misskey 的三个判定条件对等，但 Mastodon 的 mention 信息形态不同：
//  1. 正文（HTML 转纯文本后）字面出现 @botAcct / @botUsername；
//  2. 回复目标就是 Bot 的帖子（Mastodon 直接给 in_reply_to_account_id，
//     不需要像 Misskey 那样先拿到父帖对象再比对作者）；
//  3. mentions 数组含 Bot 的账号 ID。
func (c *MastodonChannel) timelineMentioned(st *Status) bool {
	if re := c.selfMentionRe(); re != nil && re.MatchString(htmlToText(st.Content)) {
		return true
	}
	if c.selfAccountID() == "" {
		return false
	}
	if st.InReplyToAccountID == c.selfAccountID() {
		return true
	}
	for _, m := range st.Mentions {
		if m.ID == c.selfAccountID() {
			return true
		}
	}
	return false
}

// handleNotification 处理通知流事件。
//
// 对等 Misskey 的两类入站：
//   - mention：被 @ / 被回复 → 走正常 handleStatus，Mentioned=true，并推进锚点
//   - favourite：别人收藏了 bot 的帖子 → awareness-only（空 Text + InjectContext），
//     出站由 api 层 reaction-ack enricher 硬抑制（读 metadata["ack_only"]）
//
// 其余通知类型（follow / reblog / poll / follow_request）一律忽略：
// 与 Misskey 一样，关注/转发不产生对话，避免无谓唤醒。
func (c *MastodonChannel) handleNotification(ctx context.Context, n *Notification) {
	if n == nil {
		return
	}
	switch n.Type {
	case "mention":
		// ⚠️ 锚点必须在**任何提前 return 之前**推进，且与 backfillMentions 的推进
		// 条件完全一致（nil Status / 自己发的 / 去重命中 三条路径都推进）。
		//
		// 否则锚点停滞后，断线重连的 backfillMentions 会用旧 since_id 把这条通知
		// 重新拉回来；dedupSeen 只有 2 分钟窗口，挡不住几十分钟后的重放 →
		// 同一条 mention 被二次处理并**二次外发回复**。这正是 Misskey
		// 2026-09-01 重复外发事故的同款根因。
		//
		// setNotificationAnchor 单调递增，末尾再调一次是冗余保护，无害。
		// 注意：只对 mention 推进——backfill 的 Types 只有 mention，若被一条
		// favourite 通知把锚点顶到更后面，会跳过尚未处理的更早 mention。
		c.setNotificationAnchor(n.ID)
		if n.Status == nil {
			return
		}
		if n.Account.ID == c.selfAccountID() {
			return
		}
		if c.dedupSeen(n.Status.ID) {
			return
		}
		c.handleStatus(ctx, n.Status, "mention", true, n.ID)
		// 推进锚点：该 mention 已成功处理，后续断连以此 ID 为基准 backfill。
		c.setNotificationAnchor(n.ID)
	case "favourite":
		c.handleFavouriteNotification(ctx, n)
	default:
	}
}

// handleFavouriteNotification 把「别人收藏了 bot 的帖子」归一化为仅感知消息。
//
// 与 Misskey 的 reaction 通知完全对等：空 Text（不污染 L0 记忆）+ InjectContext
// 提示模型「知道了就行，别回复、别回赞、别调工具」；metadata 带 ack_only=true
// 且**故意不设 reply_target**，避免误串接到被收藏的帖子。
func (c *MastodonChannel) handleFavouriteNotification(ctx context.Context, n *Notification) {
	if n.Account.ID == c.selfAccountID() {
		return // 忽略自赞
	}
	if c.dedupSeen(n.ID) {
		return
	}

	handle := n.Account.handle()
	displayName := n.Account.DisplayName
	if displayName == "" {
		displayName = n.Account.Username
	}
	guidance := "只需要知道这件事即可：不要回复、不要转发、不要回赞、也不要为此调用工具，除非对方同时还发了文字在找你。"
	inject := fmt.Sprintf("[Mastodon 收藏] %s 收藏了你的帖子。%s", handle, guidance)
	if n.Status != nil {
		inject += fmt.Sprintf("\n[status_id: %s]", n.Status.ID)
	}

	createdAt := time.Now()
	if t, err := time.Parse(time.RFC3339, n.CreatedAt); err == nil {
		createdAt = t
	}

	metadata := map[string]any{
		"event_type":      "favourite",
		"channel_type":    "mastodon",
		"notification_id": n.ID,
		"username":        n.Account.Username,
		"display_name":    displayName,
		"acct":            handle,
		"ack_only":        true,
	}
	if n.Status != nil {
		metadata["status_id"] = n.Status.ID
	}

	msg := core.Message{
		ID:            n.ID,
		BotID:         c.botID,
		Source:        c.name,
		Channel:       n.Account.ID, // 1:1 会话
		ChatType:      core.ChatPrivate,
		UserID:        n.Account.ID,
		Text:          "", // 空 Text：不污染 L0（同心跳契约）
		InjectContext: inject,
		Mentioned:     false,
		FromIsBot:     n.Account.Bot,
		MediaType:     "text/plain",
		Metadata:      metadata,
		CreatedAt:     createdAt,
	}

	ing := c.getIngress()
	if ing == nil {
		return
	}
	if err := ing.Receive(ctx, msg); err != nil {
		traceid.L(ctx).Warnw("mastodon favourite notification ingress receive failed",
			"channel", c.name, "notification_id", n.ID, "err", err)
	}
}

// handleStatus 将一条 Mastodon Status 转换为 core.Message 并注入 Ingress。
//
// notifyID 是来源通知 ID（mention 场景），用于推进 backfill 锚点；
// 时间线场景传空。消息本身始终以 status.ID 作为 ID 与去重键。
func (c *MastodonChannel) handleStatus(ctx context.Context, st *Status, eventType string, mentioned bool, notifyID string) {
	text := htmlToText(st.Content)

	// 纯 boost（Reblog 非空且自己无正文）：回退到被转发的正文。
	// 让 bot「看得到、能思考」这条转发，而不是收到一条空消息。
	reblogFallback := false
	if text == "" && st.Reblog != nil {
		if rt := htmlToText(st.Reblog.Content); rt != "" {
			text = rt
			reblogFallback = true
		}
	}
	if text == "" && len(st.MediaAttachments) == 0 {
		return
	}

	// 从文本中剥离 @bot 提及（Bot 不需要看到自己被 @）
	if re := c.selfMentionRe(); re != nil {
		text = strings.TrimSpace(re.ReplaceAllString(text, ""))
	}
	// 剥离后为空但原来是 boost，再次回退
	if text == "" && st.Reblog != nil {
		if rt := htmlToText(st.Reblog.Content); rt != "" {
			text = rt
			reblogFallback = true
		}
	}
	if text == "" && len(st.MediaAttachments) == 0 {
		return
	}

	// 构建回复/转发上下文，让 Bot 能看到用户回复了什么或转发了什么
	text = c.statusContext(ctx, st, text, reblogFallback)

	handle := st.Account.handle()
	displayName := st.Account.DisplayName
	if displayName == "" {
		displayName = st.Account.Username
	}

	createdAt := time.Now()
	if t, err := time.Parse(time.RFC3339, st.CreatedAt); err == nil {
		createdAt = t
	}

	statusType := classifyStatus(st)
	// 纯 boost 标记：Mastodon **允许**回复 boost（与 Misskey 的
	// CANNOT_REPLY_TO_A_PURE_RENOTE 不同），故这里只作信息记录，
	// 不写 core.MetaIsPureRenote、不触发硬抑制。
	isPureReblog := st.IsPureReblog()

	var reblogID string
	if st.Reblog != nil {
		reblogID = st.Reblog.ID
	}

	metadata := map[string]any{
		"status_id":    st.ID,
		"reply_target": st.ID, // outbound 回写使用的精确目标（statusID）
		"username":     st.Account.Username,
		"acct":         handle,
		"visibility":   st.Visibility,
		"event_type":   eventType,
		"reply_id":     st.InReplyToID,
		"reblog_id":    reblogID,
		"display_name": displayName,
		"status_type":  statusType,
		"channel_type": "mastodon", // Channel 类型，供 ToolSessionContext 使用
		// 纯 boost 只作信息记录：Mastodon 允许回复 boost，
		// 不写 core.MetaIsPureRenote（那是 Misskey 的硬抑制标记）。
		"is_pure_reblog": isPureReblog,
	}
	if notifyID != "" {
		metadata["notification_id"] = notifyID
	}

	// 入站附件归一化为 core.Attachment（与 Misskey / Telegram 对称）：
	// 使图片对主模型可见（多模态直送或 vision 转写）。
	var attachments []core.Attachment
	if len(st.MediaAttachments) > 0 {
		metadata["file_count"] = len(st.MediaAttachments)
		attachments = statusAttachments(st.MediaAttachments)
		for i, m := range st.MediaAttachments {
			metadata[fmt.Sprintf("file_%d_url", i)] = m.URL
		}
	}

	// ChatType：被 @ / 私信 → 单聊；时间线普通帖 → 群聊/社交广播。
	chatType := core.ChatGroup
	if mentioned || st.Visibility == VisibilityDirect {
		chatType = core.ChatPrivate
	}

	if eventType == "timeline" && !mentioned {
		text = fmt.Sprintf("[Timeline] @%s: %s", st.Account.Acct, text)
	}
	if st.Account.Bot {
		text = fmt.Sprintf("[对方是 Bot 账号 %s] %s", handle, text)
	}
	// 把当前帖 ID 附在末尾供 LLM 调用收藏/引用类工具，省掉一次搜索。
	text = fmt.Sprintf("%s\n[status_id: %s]", text, st.ID)

	channelID := mastodonIngressChannelID(eventType, mentioned, st.Visibility, st.Account.ID)

	msg := core.Message{
		ID:        st.ID,
		BotID:     c.botID,
		Source:    c.name,
		Channel:   channelID,
		ChatType:  chatType,
		UserID:    st.Account.ID,
		Text:      text,
		Mentioned: mentioned,
		FromIsBot: st.Account.Bot,
		MediaType: "text/plain",
		Metadata:  metadata,
		CreatedAt: createdAt,
	}
	if len(attachments) > 0 {
		core.SetAttachments(&msg, attachments)
	}

	ing := c.getIngress()
	if ing == nil {
		return
	}
	if err := ing.Receive(ctx, msg); err != nil {
		traceid.L(ctx).Warnw("mastodon ingress receive failed",
			"channel", c.name, "status_id", st.ID, "err", err)
	}
}

// classifyStatus 判断帖子交互类型：status / reply / reblog（纯转发）/ quote（带评转发）。
// Mastodon 4.x 起才支持带评引用转发（quote），判定方式与 Misskey 一致。
func classifyStatus(st *Status) string {
	if st.Reblog != nil {
		if isBlankContent(st.Content) {
			return "reblog"
		}
		return "quote"
	}
	if st.InReplyToID != "" {
		return "reply"
	}
	return "status"
}

// statusAttachments 把媒体附件归一化为 core.Attachment 列表。
//
// Mastodon 的 type 是分类枚举（image/gifv/video/audio/unknown）而非 MIME，
// 且真正的 MIME 常缺省，故按分类枚举推断 core 类型即可。
func statusAttachments(medias []MediaAttachment) []core.Attachment {
	if len(medias) == 0 {
		return nil
	}
	atts := make([]core.Attachment, 0, len(medias))
	for _, m := range medias {
		atts = append(atts, core.Attachment{
			Type:     attachmentTypeFromMedia(m.Type),
			MimeType: mediaMIME(m.Type),
			URL:      m.URL,
			Filename: m.Description,
		})
	}
	return atts
}

// attachmentTypeFromMedia 把 Mastodon 媒体分类映射到 core.Attachment.Type。
func attachmentTypeFromMedia(t string) string {
	switch strings.ToLower(t) {
	case "image":
		return core.AttachmentTypeImage
	case "gifv", "video":
		return core.AttachmentTypeVideo
	case "audio":
		return core.AttachmentTypeAudio
	default:
		return core.AttachmentTypeFile
	}
}

// mediaMIME 给出媒体分类对应的典型 MIME（Mastodon 响应里常缺省该字段）。
func mediaMIME(t string) string {
	switch strings.ToLower(t) {
	case "image":
		return "image/*"
	case "gifv":
		return "video/mp4"
	case "video":
		return "video/mp4"
	case "audio":
		return "audio/*"
	default:
		return "application/octet-stream"
	}
}

// statusContext 为帖子文本添加回复和转发上下文。
//
// 与 Misskey 的关键差异：Mastodon 的 streaming 事件**不内联父帖**，
// 想知道「用户在回复什么」必须再查一次 API。这里走带缓存的 getStatus，
// 并由 Config.ResolveReplyContext 控制（关掉可省 API 配额）。
// 转发内容则是内联的（st.Reblog），无需额外请求。
func (c *MastodonChannel) statusContext(ctx context.Context, st *Status, text string, skipReblog bool) string {
	if c.cfg.ResolveReplyContext && st.InReplyToID != "" {
		if parent := c.lookupReplyContext(ctx, st.InReplyToID); parent != nil {
			quoted := strutil.Truncate(htmlToText(parent.Content), 200)
			sender := parent.Account.DisplayName
			if sender == "" {
				sender = parent.Account.Username
			}
			if sender != "" && quoted != "" {
				if text == "" {
					text = fmt.Sprintf("[Reply to %s: %s]", sender, quoted)
				} else {
					text = fmt.Sprintf("[Reply to %s: %s]\n%s", sender, quoted, text)
				}
			}
		}
	}

	if !skipReblog && st.Reblog != nil {
		quoted := strutil.Truncate(htmlToText(st.Reblog.Content), 200)
		sender := st.Reblog.Account.DisplayName
		if sender == "" {
			sender = st.Reblog.Account.Username
		}
		if sender != "" && quoted != "" {
			if text == "" {
				text = fmt.Sprintf("[Boost from %s: %s]", sender, quoted)
			} else {
				text = fmt.Sprintf("[Boost from %s: %s]\n%s", sender, quoted, text)
			}
		}
	}
	return text
}

// lookupReplyContext 取父帖（带缓存）。失败返回 nil，不阻断入站。
func (c *MastodonChannel) lookupReplyContext(ctx context.Context, statusID string) *Status {
	if statusID == "" {
		return nil
	}
	c.replyMu.Lock()
	if e, ok := c.replyCache[statusID]; ok && time.Since(e.at) <= replyContextTTL {
		c.replyMu.Unlock()
		return e.status
	}
	c.replyMu.Unlock()

	st, err := c.api.getStatus(ctx, statusID)
	if err != nil || st == nil {
		traceid.L(ctx).Debugw("mastodon: reply context lookup failed",
			"channel", c.name, "status_id", statusID, "err", err)
		return nil
	}
	c.replyMu.Lock()
	c.replyCache[statusID] = &replyCacheEntry{status: st, at: time.Now()}
	c.replyMu.Unlock()
	return st
}

// isStopped 报告 channel 是否已停止。
func (c *MastodonChannel) isStopped() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stopped
}

// selfAccountID 返回 Bot 自身账号 ID（并发安全读）。
func (c *MastodonChannel) selfAccountID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.botAccountID
}

// selfAcct 返回 Bot 的完整账号标识（并发安全读）。
func (c *MastodonChannel) selfAcct() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.botAcct
}

// selfUsername 返回 Bot 的本地用户名（并发安全读）。
func (c *MastodonChannel) selfUsername() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.botUsername
}

// selfMentionRe 返回 @bot 提及正则（并发安全读），未初始化返回 nil。
func (c *MastodonChannel) selfMentionRe() *regexp.Regexp {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mentionRe
}

// getIngress 返回入站管道（并发安全读），未 Start 返回 nil。
func (c *MastodonChannel) getIngress() *inbound.Ingress {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ingress
}

// mastodonIngressChannelID 决定入站 Message.Channel。
// 时间线上的普通帖共用 mastodon:timeline；被 @ / direct 私信仍按用户隔离，
// 否则 user_choice 会把 ChatID 记成 mastodon:timeline，投票者对不上永远超时。
func mastodonIngressChannelID(eventType string, mentioned bool, visibility, userID string) string {
	if eventType == "timeline" && !mentioned && visibility != VisibilityDirect {
		return "mastodon:timeline"
	}
	return userID
}

// Stop 优雅停止 streaming。
func (c *MastodonChannel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.stopped = true
	c.mu.Unlock()

	// 注：这里**故意不** UnregisterPollCreator。pollCreators 是按平台名做 key 的
	// 全局表，本机可能同时挂着多个 mastodon bot，停掉其中一个就把整张表摘了会
	// 连累别的 bot 的 user_choice。改为在 CreatePollStatus 入口判 stopped 拒绝。

	// 停掉所有在途投票：轮询 goroutine 挂的是 runCtx，cancel 后会自己退出，
	// 但 debounce / expire 定时器是 *time.Timer，必须显式 Stop 才不会在
	// channel 停止后继续触发 resolvePollVote。
	c.pollsMu.Lock()
	for id, st := range c.polls {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		c.stopPollWatchLocked(st)
		delete(c.polls, id)
	}
	c.pollsMu.Unlock()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		traceid.L(ctx).Infow("mastodon channel stopped", "channel", c.name)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// truncateForLog 截断超长原始帧，避免日志被整条 HTML 正文刷屏。
func truncateForLog(s string) string {
	return strutil.Truncate(s, 500)
}

// mapVisibility 把可见性取值归一到 Mastodon 的四个合法值。
//
// 上游（如心跳/Action.Metadata）可能沿用 Misskey 语义的 "home"/"followers"/
// "specified"，这里一并接受，避免新平台接入时到处改调用方。
// 未知值回落 unlisted（而非 public）：Mastodon 对回复可见性由服务端强制不高于
// 父帖，但顶层发帖若不慎按 public 发出就不可逆，保守取 unlisted。
func mapVisibility(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "public":
		return VisibilityPublic
	case "unlisted", "home":
		return VisibilityUnlisted
	case "private", "followers":
		return VisibilityPrivate
	case "direct", "specified":
		return VisibilityDirect
	default:
		return VisibilityUnlisted
	}
}

// truncEllipsis 是 strutil.Truncate 超长时追加的省略号。
const truncEllipsis = "..."

// truncateFor 把 s 截到**不超过** limit 个 rune。
//
// ⚠️ 预算必须扣掉省略号：strutil.Truncate 在超长时返回 `maxRunes + 3` 个字符
// （尾部追加 "..."）。若直接把上限传进去，所有**需要被截断**的长文本都会
// 超限 3 个字符，被 Mastodon 的校验器判 422 `Validation failed /
// Text exceeds the maximum of N characters` —— 偏偏最长的回复最容易触发，
// 而短文本一切正常，自测时极容易漏掉。
func truncateFor(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	budget := limit - len(truncEllipsis)
	if budget <= 0 {
		// 上限连省略号都塞不下（异常实例配置）：硬切到 limit，
		// 宁可没有省略号也绝不能超限——超一点就是整个 422。
		r := []rune(s)
		if len(r) <= limit {
			return s
		}
		return string(r[:limit])
	}
	return strutil.Truncate(s, budget)
}

// truncateForStatus 按实例帖子字符上限截断正文。
func (c *MastodonChannel) truncateForStatus(s string) string {
	limit := c.api.statusLimit()
	if limit <= 0 {
		limit = defaultMaxStatusLength
	}
	return truncateFor(s, limit)
}

// Reply 向指定帖子回复。便捷方法，供 Pipeline Action 处理器调用。
// 文本超长时按实例上限自动截断。
func (c *MastodonChannel) Reply(ctx context.Context, statusID, text string) error {
	return c.ReplyWithVisibility(ctx, statusID, text, VisibilityUnlisted)
}

// ReplyWithVisibility 向指定帖子回复，使用指定可见性。
//
// 与 Misskey 的差异：Mastodon 不需要「纯 Renote 降级」——回复 boost 是合法的，
// 不存在 CANNOT_REPLY_TO_A_PURE_RENOTE 这类限制，故没有降级分支。
func (c *MastodonChannel) ReplyWithVisibility(ctx context.Context, statusID, text, visibility string) error {
	text = c.truncateForStatus(strings.TrimSpace(text))

	// 回复时自动 @ 被回复者及其他被 @ 的人（排除 Bot 自身）。
	// Mastodon 用 acct（含域），远程用户只有带域才能被正确提及。
	if statusID != "" {
		if parent, err := c.lookupStatus(ctx, statusID); err == nil && parent != nil {
			if prefix := c.buildReplyMentionPrefix(parent); prefix != "" {
				text = c.truncateForStatus(prefix + text)
			}
		} else if err != nil {
			traceid.L(ctx).Debugw("mastodon: getStatus for reply mention prefix skipped",
				"channel", c.name, "status_id", statusID, "err", err)
		}
	}

	_, err := c.api.createStatusFull(ctx, text, statusID, mapVisibility(visibility), "", nil, nil)
	if err != nil {
		traceid.L(ctx).Warnw("mastodon: reply failed, target status may be deleted",
			"channel", c.name, "status_id", statusID, "err", err)
	}
	return err
}

// lookupStatus 取一条帖子，优先走入站父帖缓存（省 API 配额）。
func (c *MastodonChannel) lookupStatus(ctx context.Context, statusID string) (*Status, error) {
	c.replyMu.Lock()
	if e, ok := c.replyCache[statusID]; ok && time.Since(e.at) <= replyContextTTL {
		c.replyMu.Unlock()
		return e.status, nil
	}
	c.replyMu.Unlock()
	return c.api.getStatus(ctx, statusID)
}

// buildReplyMentionPrefix 构造回复时的 @ 前缀。
// 规则：至少 @ 被回复者（父帖作者）；父帖文本里其他被 @ 的人，排除 Bot 自身后一并带上。
// 返回形如 "@alice @bob " 的前缀（含末尾空格），无则空字符串。
//
// Mastodon 特殊之处：@acct 已经带域（"@alice@example.social"），
// 不需要像 Misskey 那样再查 users/search 补全 host——本方法直接用 acct。
func (c *MastodonChannel) buildReplyMentionPrefix(parent *Status) string {
	if parent == nil {
		return ""
	}
	// 一次取值，避免闭包里反复加锁；身份字段在 Start 之后不再变。
	selfUser := c.selfUsername()
	selfAcct := c.selfAcct()
	seen := make(map[string]bool)
	var handles []string
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		// 取本地用户名部分用于排除自身（acct 可能是 user@domain）
		bare := strings.TrimPrefix(h, "@")
		if at := strings.Index(bare, "@"); at >= 0 {
			bare = bare[:at]
		}
		if (selfUser != "" && bare == selfUser) || (selfAcct != "" && strings.TrimPrefix(h, "@") == selfAcct) {
			return
		}
		if seen[h] {
			return
		}
		seen[h] = true
		handles = append(handles, h)
	}

	if parent.Account.Acct != "" {
		add("@" + parent.Account.Acct)
	}
	// 父帖 mentions 数组（比扫正文正则更可靠，覆盖 HTML 里的 h-card 提及）
	for _, m := range parent.Mentions {
		if m.Acct != "" {
			add("@" + m.Acct)
		}
	}
	// 正文里其余裸 @ 提及（兜底）
	for _, m := range mentionAnyRe.FindAllString(htmlToText(parent.Content), -1) {
		add(m)
	}

	if len(handles) == 0 {
		return ""
	}
	return strings.Join(handles, " ") + " "
}

// PostTimeline 向本频道时间线发布一条顶层新帖（不回复任何已有帖子）。
// 对等 Misskey 的 PostTimeline（core.TimelinePoster），用于心跳自主发声。
// 返回新建帖子 ID。
func (c *MastodonChannel) PostTimeline(ctx context.Context, text, visibility, cw string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("mastodon post: empty text")
	}
	if visibility == "" {
		visibility = VisibilityUnlisted
	}
	text = c.truncateForStatus(text)
	id, err := c.api.createStatus(ctx, text, "", mapVisibility(visibility), cw, nil)
	if err != nil {
		traceid.L(ctx).Warnw("mastodon: post to timeline failed", "channel", c.name, "err", err)
		return "", err
	}
	traceid.L(ctx).Infow("mastodon: posted to timeline",
		"channel", c.name, "status_id", id, "visibility", visibility)
	return id, nil
}

// Send 实现 bot.Sender / outbound.ChannelSender 接口。
// 根据 Action 的内容回写消息到 Mastodon。
//
// Action 字段约定：
//   - Action.Channel：回复目标的 statusID（来源于 Inbound 的 msg.Metadata["reply_target"]）
//   - Action.Payload：发送内容（string 类型的文本消息）
//   - Action.Metadata["visibility"]：可见性（public/unlisted/private/direct；
//     兼容 Misskey 语义的 home/followers/specified，见 mapVisibility）
//   - Action.Metadata["cw"]：CW 折叠文本（Mastodon 的 spoiler_text）
func (c *MastodonChannel) Send(ctx context.Context, action core.Action) error {
	statusID := action.Channel
	if statusID == "" {
		return fmt.Errorf("mastodon send: empty statusID in action.Channel")
	}
	text, ok := action.Payload.(string)
	if !ok {
		return fmt.Errorf("mastodon send: payload is %T, expected string", action.Payload)
	}
	if strings.TrimSpace(text) == "" {
		return nil // 空消息不发送
	}

	visibility := VisibilityUnlisted
	cw := ""
	if action.Metadata != nil {
		if v, ok := action.Metadata["visibility"].(string); ok && v != "" {
			visibility = mapVisibility(v)
		}
		if v, ok := action.Metadata["cw"].(string); ok {
			cw = v
		}
	}

	text = c.truncateForStatus(strings.TrimSpace(text))

	// 回复时自动补全 @acct：联邦用户必须带域才能被正确提及并收到通知。
	if parent, err := c.lookupStatus(ctx, statusID); err == nil && parent != nil {
		if prefix := c.buildReplyMentionPrefix(parent); prefix != "" {
			// 去掉 LLM 文本开头可能重复写的裸 @被回复者，避免重复 mention
			t := strings.TrimSpace(text)
			if parent.Account.Acct != "" {
				bare := "@" + parent.Account.Acct
				if strings.HasPrefix(t, bare+" ") || t == bare {
					t = strings.TrimSpace(strings.TrimPrefix(t, bare))
				}
			}
			text = c.truncateForStatus(prefix + t)
		}
	} else if err != nil {
		traceid.L(ctx).Debugw("mastodon: getStatus for send mention prefix skipped",
			"channel", c.name, "status_id", statusID, "err", err)
	}

	if _, err := c.api.createStatusFull(ctx, text, statusID, visibility, cw, nil, nil); err != nil {
		traceid.L(ctx).Warnw("mastodon send: reply failed",
			"channel", c.name, "status_id", statusID, "err", err)
		return errs.Wrapf(err, "mastodon send: reply to %q failed", statusID)
	}
	return nil
}

// Favourite 收藏（点赞）帖子。对等 Misskey 的 React。
//
// 平台差异：Mastodon 只有「收藏」这一种表态，没有自定义 emoji 反应，
// 因此没有 emoji 参数。
//
// 幂等：重复收藏属「目标已达成」，直接吞掉不向上层报错（与 Misskey React 一致）——
// 上层是出站 Action 处理器，它没有立场区分「已经收藏过」和「失败」。
// ⚠️ 别再写「重复收藏天然返回 200」：上游确实如此，但社区 fork 会返回
// 400 {"error":"already favourited"}，实测见 api.go 顶部的矩阵。
func (c *MastodonChannel) Favourite(ctx context.Context, statusID string) error {
	if err := c.api.favourite(ctx, statusID); err != nil {
		if errors.Is(err, ErrAlreadyFavourited) {
			traceid.L(ctx).Debugw("mastodon: favourite skipped (already favourited)",
				"channel", c.name, "status_id", statusID)
			return nil
		}
		return err
	}
	return nil
}

// Unfavourite 取消收藏。对等 Misskey 的 Unreact。
// 本就没有收藏时同样视为目标已达成。
func (c *MastodonChannel) Unfavourite(ctx context.Context, statusID string) error {
	if err := c.api.unfavourite(ctx, statusID); err != nil {
		if errors.Is(err, ErrNotFavourited) {
			return nil
		}
		return err
	}
	return nil
}

// Boost 转发（boost）帖子。对等 Misskey 的 createRenote。
// 重复转发视为目标已达成。
func (c *MastodonChannel) Boost(ctx context.Context, statusID string) error {
	if err := c.api.reblog(ctx, statusID); err != nil {
		if errors.Is(err, ErrAlreadyReblogged) {
			traceid.L(ctx).Debugw("mastodon: boost skipped (already reblogged)",
				"channel", c.name, "status_id", statusID)
			return nil
		}
		return err
	}
	return nil
}

// Unboost 撤销转发。本就没有转发时视为目标已达成。
func (c *MastodonChannel) Unboost(ctx context.Context, statusID string) error {
	if err := c.api.unreblog(ctx, statusID); err != nil {
		if errors.Is(err, ErrNotReblogged) {
			return nil
		}
		return err
	}
	return nil
}

// ============================================================================
// user_choice 原生投票
// ============================================================================

// CreatePollStatus 发布一条带投票的帖子，并启动轮询等待投票结果。
//
// 与 Misskey CreatePollNote 对等，但回填机制不同：
//   - Misskey：pollVoted 事件直推，能拿到 choice 和 voter；
//   - Mastodon：**没有投票事件**，且 poll 结果只暴露各选项票数、不暴露投票人。
//     因此这里发布后启动一个轮询 goroutine，靠「票数相对基线的增量」推断有人投票，
//     并用「问题所属会话」作为投票人回填（无法精确到人，属平台限制）。
//
// 参数：
//   - question: 题目文本（会作为帖子正文）
//   - replyID: 回复目标帖子 ID（可选）
//   - options: 选项文本列表（2~4 个，Mastodon 上限 4，超出截断）
//   - multiple: 是否多选
//   - timeoutSecs: 投票过期时间（秒）；Mastodon 要求 >= 300，不足时 clamp
//   - questionID: 关联的 interaction questionID（投票结果回填目标）
//
// 返回新建帖子 ID。
func (c *MastodonChannel) CreatePollStatus(ctx context.Context, question, replyID string, options []string, multiple bool, timeoutSecs int, questionID string) (string, error) {
	if len(options) < 2 {
		return "", fmt.Errorf("mastodon CreatePollStatus: at least 2 options required, got %d", len(options))
	}
	if c.isStopped() {
		// 已停止的 channel 不再发投票：轮询 goroutine 与回收定时器都没人管了。
		return "", errors.New("mastodon CreatePollStatus: channel already stopped")
	}
	// 选项数量/字数上限都是**实例级**配置（默认 4 个 / 25 字符，Misskey 是 10 个），
	// 不截断的话服务端直接 422 `Validation failed`，LLM 侧的 user_choice 整条失败。
	maxOpts := c.api.pollMaxOptions()
	if maxOpts < 2 {
		maxOpts = 2
	}
	if len(options) > maxOpts {
		options = options[:maxOpts]
	}
	// 注意同样要扣掉省略号预算（truncateFor），否则截断后反而超限被 422。
	maxOptChars := c.api.pollMaxOptionChars()
	if maxOptChars > 0 {
		for i, o := range options {
			options[i] = truncateFor(strings.TrimSpace(o), maxOptChars)
		}
	}
	// 题目即帖子正文，同样受实例字符上限约束。
	question = c.truncateForStatus(strings.TrimSpace(question))

	// clampPollExpiration 同时管上下限（Mastodon 默认 300 ~ 2629746 秒，越界即 422），
	// 且对 <=0 直接兜到实例下限，故无需再单独判 DefaultTimeoutSecs。
	expiresIn := c.api.clampPollExpiration(timeoutSecs)

	// 回复场景：可见性必须跟随父帖（direct 帖只能 direct 回复，否则 422）。
	visibility := VisibilityUnlisted
	if replyID != "" {
		if parent, err := c.lookupStatus(ctx, replyID); err == nil && parent != nil && parent.Visibility != "" {
			visibility = mapVisibility(parent.Visibility)
		}
	}

	res, err := c.api.createStatusFull(ctx, question, replyID, visibility, "", nil, &pollRequest{
		Options:   options,
		ExpiresIn: expiresIn,
		Multiple:  multiple,
	})
	if err != nil {
		return "", errs.Wrap(err, "mastodon CreatePollStatus")
	}
	if res == nil || res.ID == "" {
		return "", fmt.Errorf("mastodon CreatePollStatus: empty response from server")
	}
	if res.Poll == nil || res.Poll.ID == "" {
		return "", fmt.Errorf("mastodon CreatePollStatus: server did not return a poll for status %s", res.ID)
	}

	st := &pollState{
		QuestionID: questionID,
		Multiple:   multiple,
		OptionN:    len(options),
		PollID:     res.Poll.ID,
		baseline:   pollVotes(res.Poll),
	}

	// ⚠️ expire 定时器必须在**持锁期间**写入 st：stopPollWatchLocked 会读 st.expire，
	// 若在锁外赋值，与 Stop() 的并发清理构成数据竞争（-race 可复现），
	// 且 Stop 可能漏停刚起好的定时器，导致 channel 停止后仍触发 resolvePollVote。
	//
	// 轮询本身不需要在这里起 goroutine：Start 已经拉起了一个共享的 pollWatchLoop，
	// 它按 tick 扫 c.polls，这里只要把状态挂进表里即可。
	c.pollsMu.Lock()
	c.polls[res.ID] = st
	st.expire = time.AfterFunc(time.Duration(expiresIn)*time.Second, func() {
		c.expirePoll(res.ID, questionID)
	})
	c.pollsMu.Unlock()

	traceid.L(ctx).Infow("mastodon: poll status created",
		"channel", c.name, "status_id", res.ID, "poll_id", res.Poll.ID,
		"question_id", questionID, "options", len(options), "multiple", multiple,
		"expires_in", expiresIn)

	return res.ID, nil
}

// pollVotes 取出投票各选项当前票数快照。
func pollVotes(p *Poll) []int {
	if p == nil || len(p.Options) == 0 {
		return nil
	}
	out := make([]int, len(p.Options))
	for i, o := range p.Options {
		out[i] = o.VotesCount
	}
	return out
}

// pollWatchLoop 是**全局共享**的投票轮询循环：一个 ticker 按跳扫描所有在途投票。
//
// 为什么不是「每条投票一个 goroutine + 一个 ticker」：Mastodon 限流
// 300 req/5min（≈1 req/s）。每条投票独立跳时，跳数随并发投票数线性增长，
// 3 条并发投票就打满整个配额，发帖/点赞/取父帖连带 429。共享一个 ticker 后
// **跳数恒定**，单跳成本才是 O(在途投票数)，配额占用与并发投票数脱钩。
//
// 挂在 runCtx 上：轮询是长生命周期的，不能跟着某一轮 LLM 调用的 ctx 走。
// 表里没有在途投票时本循环是纯空转（不发任何请求），成本可忽略。
func (c *MastodonChannel) pollWatchLoop(ctx context.Context) {
	// ⚠️ 本循环由 Start 以 wg.Add(1) 起，必须 defer Done。
	// 少了这一行 Stop() 的 c.wg.Wait() 会**永久阻塞**（goroutine 其实已经退出，
	// 但计数器没归零），表现为 bot 停止/重启卡死且没有任何报错——
	// 只有真实实例跑一次才能发现，单元测试抓不到。
	defer c.wg.Done()
	ticker := time.NewTicker(pollWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollsMu.Lock()
			ids := make([]string, 0, len(c.polls))
			for id := range c.polls {
				ids = append(ids, id)
			}
			c.pollsMu.Unlock()
			for _, id := range ids {
				c.pollTick(ctx, id)
			}
		}
	}
}

// pollTick 拉一次投票结果，把「票数增量」转成一次投票事件。
// 返回 false 表示这条投票已终结（已结算或从表里消失），调用方应停止轮询。
func (c *MastodonChannel) pollTick(ctx context.Context, statusID string) bool {
	c.pollsMu.Lock()
	st, ok := c.polls[statusID]
	c.pollsMu.Unlock()
	if !ok {
		return false
	}

	p, err := c.api.getPoll(ctx, st.PollID)
	if err != nil {
		traceid.L(ctx).Debugw("mastodon: poll fetch failed (will retry)",
			"channel", c.name, "status_id", statusID, "poll_id", st.PollID, "err", err)
		return true
	}
	if p == nil {
		return true
	}

	current := pollVotes(p)
	c.pollsMu.Lock()
	st, ok = c.polls[statusID]
	if !ok {
		c.pollsMu.Unlock()
		return false
	}
	// 本轮新增票的选项：票数比基线多了几票就算几票。
	var gained []int
	for i := range current {
		base := 0
		if i < len(st.baseline) {
			base = st.baseline[i]
		}
		if current[i] > base {
			gained = append(gained, i)
		}
	}
	// 基线推进到当前值，避免同一票被反复计入。
	st.baseline = current
	c.pollsMu.Unlock()

	if len(gained) == 0 {
		return true
	}
	// Mastodon 不暴露投票人，回填时取问题所属会话（见 resolvePollVote）。
	for _, choice := range gained {
		c.onPollVote(ctx, statusID, choice)
	}
	return true
}

// onPollVote 是投票结算核心：单选首票即解封、多选 debounce 后回填、越界 choice 忽略。
// 逻辑与 Misskey 完全一致（复用同一套状态机语义）。
func (c *MastodonChannel) onPollVote(ctx context.Context, statusID string, choice int) {
	c.pollsMu.Lock()
	st, ok := c.polls[statusID]
	if !ok {
		c.pollsMu.Unlock()
		return
	}
	action := applyPollVote(st, choice)
	switch action {
	case pollVoteIgnore:
		c.pollsMu.Unlock()
		return
	case pollVoteResolveNow:
		qid := st.QuestionID
		sel := append([]int(nil), st.Selected...)
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		delete(c.polls, statusID)
		c.stopPollWatchLocked(st)
		c.pollsMu.Unlock()
		c.resolvePollVote(ctx, qid, sel, statusID)
		return
	case pollVoteDebounce:
		if st.timer != nil {
			st.timer.Stop()
		}
		st.timer = time.AfterFunc(pollMultiDebounce, func() {
			c.flushMultiPoll(ctx, statusID)
		})
		c.pollsMu.Unlock()
		return
	default:
		c.pollsMu.Unlock()
	}
}

type pollVoteAction int

const (
	pollVoteIgnore pollVoteAction = iota
	pollVoteResolveNow
	pollVoteDebounce
)

// addPollChoice 把 unique choice 记入 st.Selected；越界或重复返回 false。
func addPollChoice(st *pollState, choice int) bool {
	if st == nil || choice < 0 || (st.OptionN > 0 && choice >= st.OptionN) {
		return false
	}
	for _, s := range st.Selected {
		if s == choice {
			return false
		}
	}
	st.Selected = append(st.Selected, choice)
	return true
}

// applyPollVote 把一次投票记入状态，决定立刻 Resolve / debounce / 忽略。
func applyPollVote(st *pollState, choice int) pollVoteAction {
	if st == nil {
		return pollVoteIgnore
	}
	if !st.Multiple {
		if st.OptionN > 0 && choice >= st.OptionN {
			return pollVoteIgnore
		}
		st.Selected = []int{choice}
		return pollVoteResolveNow
	}
	if !addPollChoice(st, choice) {
		return pollVoteIgnore
	}
	if st.OptionN > 0 && len(st.Selected) >= st.OptionN {
		return pollVoteResolveNow
	}
	return pollVoteDebounce
}

// stopPollWatchLocked 停掉这条投票的定时器（需持有 pollsMu）。
// 轮询是共享 ticker，会自己发现表里没这条而跳过，无需在这里取消。
func (c *MastodonChannel) stopPollWatchLocked(st *pollState) {
	if st == nil {
		return
	}
	if st.expire != nil {
		st.expire.Stop()
		st.expire = nil
	}
}

// expirePoll 超时回收：确认同一 questionID 仍映射后删除并停掉轮询。
func (c *MastodonChannel) expirePoll(statusID, questionID string) {
	c.pollsMu.Lock()
	st, ok := c.polls[statusID]
	if !ok {
		c.pollsMu.Unlock()
		return
	}
	if questionID != "" && st.QuestionID != questionID {
		c.pollsMu.Unlock()
		return
	}
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	delete(c.polls, statusID)
	c.stopPollWatchLocked(st)
	c.pollsMu.Unlock()
}

// flushMultiPoll 多选 debounce 窗口结束后一次性回填。
func (c *MastodonChannel) flushMultiPoll(ctx context.Context, statusID string) {
	c.pollsMu.Lock()
	st, ok := c.polls[statusID]
	if !ok {
		c.pollsMu.Unlock()
		return
	}
	qid := st.QuestionID
	sel := append([]int(nil), st.Selected...)
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	delete(c.polls, statusID)
	c.stopPollWatchLocked(st)
	c.pollsMu.Unlock()
	if len(sel) == 0 {
		return
	}
	c.resolvePollVote(ctx, qid, sel, statusID)
}

// resolvePollVote 回填 user_choice 到等待中的 user_choice 工具。
//
// 与 Misskey 的差异：Mastodon 拿不到投票人 ID，无法做「投票人必须是问题所属
// 会话成员」的校验；这里直接取问题登记的 ChatID 作为回填来源。
// 这是平台能力限制（poll 结果不含投票人），已在 README 标注。
func (c *MastodonChannel) resolvePollVote(ctx context.Context, questionID string, selected []int, statusID string) {
	q, err := interaction.Default().Lookup(questionID)
	if err != nil {
		traceid.L(ctx).Debugw("mastodon: poll resolve lookup failed",
			"channel", c.name, "question_id", questionID, "err", err)
		return
	}
	ans := interaction.Answer{
		Selected: selected,
		Via:      interaction.ViaMastodon,
	}
	if err := interaction.Default().ResolveFrom(questionID, q.ChatID, ans); err != nil {
		traceid.L(ctx).Warnw("mastodon: poll resolve failed",
			"channel", c.name, "question_id", questionID, "selected", selected, "err", err)
		return
	}
	traceid.L(ctx).Infow("mastodon: poll vote resolved user_choice",
		"channel", c.name, "question_id", questionID, "status_id", statusID,
		"selected", selected)
}
