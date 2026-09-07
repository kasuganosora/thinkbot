package misskey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// MisskeyChannel — Misskey 平台输入端适配器
// ============================================================================

const (
	// misskeyMaxNoteLength 单条帖子最大长度（rune 数）。
	misskeyMaxNoteLength = 3000
	// 指数退避重连参数。
	misskeyReconnectDelayMin = 5 * time.Second
	misskeyReconnectDelayMax = 5 * time.Minute
	// 去重缓存 TTL 和清理间隔。
	misskeyDedupTTL          = 2 * time.Minute
	misskeyDedupCleanupEvery = 30 * time.Second
	// mainConnID 是 main 通道的固定连接 ID。
	mainConnID = "main-1"
)

// mentionAnyRe 匹配文本中的 @username 或 @username@host 提及（不分用户）。
var mentionAnyRe = regexp.MustCompile(`@[A-Za-z0-9_]+(@[A-Za-z0-9.\-]+)?`)

// timelineConnID 返回某个 timeline 频道的 streaming 连接 ID（如 "tl:homeTimeline"）。
func timelineConnID(channel string) string { return "tl:" + channel }

// Config 配置 MisskeyChannel。
type Config struct {
	// Host Misskey 实例 URL（如 "https://misskey.io"）。
	Host string

	// Token Misskey API Token（含 WebSocket streaming 和 HTTP API 权限）。
	Token string

	// WatchdogTimeout WebSocket 看门狗超时。0 = 使用 120s 默认值。
	WatchdogTimeout time.Duration

	// PingInterval WebSocket 自动 Ping 间隔。0 = 使用 30s 默认值。
	PingInterval time.Duration

	// ReconnectDelay 断线后重连间隔。0 = 使用 5s 默认值。
	ReconnectDelay time.Duration

	// TimelineChannels 需要订阅的 Misskey streaming timeline 频道列表。
	// 合法值来自 Misskey 官方：homeTimeline / localTimeline / hybridTimeline / globalTimeline。
	// 订阅后 Bot 会收到这些时间线上的帖子（Mentioned=false），可用于"旁听群聊"。
	// 留空则仅通过 main 通道接收 @提及 / 回复。
	TimelineChannels []string
}

// validTimelineChannels 是 Misskey 官方支持的可订阅 timeline streaming 频道。
// 与 packages/backend/src/server/api/stream/channels/*.ts 的 chName 一致。
var validTimelineChannels = map[string]bool{
	"homeTimeline":   true,
	"localTimeline":  true,
	"hybridTimeline": true,
	"globalTimeline": true,
}

// MisskeyChannel 是 Misskey 平台的输入端实现。
//
// 它通过 WebSocket streaming 连接到 Misskey 实例的 main 通道，
// 监听 mention（提及）和 reply（回复）事件，
// 归一化为 core.Message 后注入 Ingress。
//
// 使用示例：
//
//	ch := misskey.NewChannel("my-mk-bot", "my-bot-id", misskey.Config{
//	    Host:  "https://misskey.example.com",
//	    Token: "8xxxxxxxxxxxxx...",
//	})
//	bot, _ := bot.New(bot.BotParams{
//	    ID:       "my-bot-id",
//	    Channels: []bot.Channel{ch},
//	})
//	go bot.Run(ctx)
type MisskeyChannel struct {
	name  string
	botID string
	cfg   Config
	api   *apiClient
	hc    *http.Client

	// botUserID 是 Bot 自身的 Misskey User ID，在 Start 时通过 getSelf 获取。
	// 用于在 timeline 模式下过滤自己发的帖。
	botUserID string
	// botUsername 是 Bot 的用户名，用于从文本中剥离 @bot 提及。
	botUsername string
	// mentionRe 匹配 @botUsername 或 @botUsername@host 的正则表达式。
	// 确保不会误匹配更长的用户名（如 @botuser 不会匹配 @bot）。
	mentionRe *regexp.Regexp

	ingress *inbound.Ingress

	// 去重缓存：noteID -> 入时间。防止 mention+timeline 同时投递同一条帖子。
	dedupMu sync.Mutex
	dedup   map[string]time.Time

	// 锚点：最近一次成功处理的「提及 Bot」帖子的 noteID。
	// 断线重连后用它作为 sinceId 拉取 notes/mentions，补发断连窗口内错过的 @提及/回复，
	// 解决 Misskey streaming 不重放历史消息导致 mention 静默丢失的设计缺陷。
	anchorMu      sync.Mutex
	lastMentionID string

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool

	// streamConnects 累计成功建立的 streaming 连接次数。
	// 用于区分首次连接与重连（>1 即为重连），并在日志中暴露重连是否真的成功。
	streamConnects atomic.Int64

	// pollNotes 追踪 bot 发出的投票帖。单选首票即 ResolveFrom；多选累计 unique
	// 下标，debounce 后一次性回填（Misskey 每个选项单独发 pollVoted，没有 done）。
	pollNotesMu sync.Mutex
	pollNotes   map[string]*pollNoteState

	// wsMu / wsConn 持有当前 streaming 连接的引用，便于在建立连接后
	// 动态订阅/退订单条投票帖的 note capture 流。
	// 关键：Misskey 的 pollVoted 事件只投递到 note 流（channel:"note",params:{noteId}），
	// 不会出现在 main 流，因此必须显式订阅每条投票帖才能收到投票结果。
	wsMu   sync.Mutex
	wsConn *http.WSConn
}

// pollNoteState 是一条 bot 投票帖的进行中状态。
type pollNoteState struct {
	QuestionID string
	Multiple   bool
	Selected   []int // unique choice indices
	OptionN    int
	LastVoter  string
	timer      *time.Timer // 多选 debounce
	expire     *time.Timer // 无人投票时丢掉 mapping，避免泄漏
}

const pollMultiDebounce = 3 * time.Second

// NewChannel 创建一个 MisskeyChannel。
func NewChannel(name, botID string, cfg Config) *MisskeyChannel {
	if cfg.WatchdogTimeout <= 0 {
		cfg.WatchdogTimeout = 120 * time.Second
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = 5 * time.Second
	}
	cfg.TimelineChannels = normalizeTimelineChannels(cfg)
	return &MisskeyChannel{
		name:      name,
		botID:     botID,
		cfg:       cfg,
		hc:        http.New(),
		api:       newAPIClient(cfg.Host, cfg.Token),
		pollNotes: make(map[string]*pollNoteState),
	}
}

// normalizeTimelineChannels 过滤非法频道名并去重，保持稳定顺序。
func normalizeTimelineChannels(cfg Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, ch := range cfg.TimelineChannels {
		if validTimelineChannels[ch] && !seen[ch] {
			seen[ch] = true
			out = append(out, ch)
		}
	}
	return out
}

// Name 返回 Channel 名称。
func (c *MisskeyChannel) Name() string { return c.name }

// Type 返回 "misskey"。
func (c *MisskeyChannel) Type() string { return "misskey" }

// BotID 返回所属 Bot ID。
func (c *MisskeyChannel) BotID() string { return c.botID }

// Start 启动 WebSocket streaming 循环（非阻塞）。
func (c *MisskeyChannel) Start(ctx context.Context, ingress *inbound.Ingress) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("misskey channel: already stopped, cannot restart")
	}

	c.ingress = ingress

	// 验证 Token
	me, err := c.api.getSelf(ctx)
	if err != nil {
		return errs.Wrap(err, "misskey channel: token validation failed")
	}
	traceid.L(ctx).Infow("misskey channel started",
		"channel", c.name, "username", me.Username, "host", c.cfg.Host)
	c.botUserID = me.ID
	c.botUsername = me.Username
	c.dedup = make(map[string]time.Time)

	// 种子锚点：启动即拉取最新一条提及，作为后续重连 backfill 的基准。
	// 这样即便启动时还无任何实时 mention，首次断线重连也能从「启动时刻」起恢复，
	// 而非从空白开始（空白会导致首段历史全丢）。失败仅告警，不阻断启动。
	if notes, err := c.api.getMentions(ctx, "", "", 1); err != nil {
		traceid.L(ctx).Warnw("misskey: seed mention anchor failed (backfill baseline unavailable until first live mention)",
			"channel", c.name, "err", err)
	} else if len(notes) > 0 {
		c.setMentionAnchor(notes[0].ID)
	}

	// 注册 Bot 自身用户 ID 到 Ingress，作为防止自回复循环的第二道防线。
	// （第一道防线是本 channel 在 streaming 中过滤自帖，第 364、389 行）
	// 注册时机：在 streamLoop 启动前，确保零竞态。
	ingress.RegisterSelfUserID(me.ID)

	// 编译 @bot 正则：匹配 @username 或 @username@host，确保后面不跟字母数字或下划线
	c.mentionRe = regexp.MustCompile(`@` + regexp.QuoteMeta(me.Username) + `(?:@[\w.-]+)?\b`)

	// 注册 Misskey 原生投票创建器，供 user_choice 工具在 misskey 平台使用
	// （替代 unsupported 降级为纯文本的粗糙方案）
	interaction.RegisterPollCreator("misskey", c.CreatePollNote)

	// 派生可取消的 context
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// 启动去重缓存清理 goroutine
	c.wg.Add(1)
	go c.dedupCleanupLoop(runCtx)

	// 启动 streaming goroutine
	c.wg.Add(1)
	go c.streamLoop(runCtx)

	return nil
}

// streamLoop 维护 WebSocket 连接，断线自动重连（指数退避）。
func (c *MisskeyChannel) streamLoop(ctx context.Context) {
	defer c.wg.Done()

	delay := misskeyReconnectDelayMin
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
			traceid.L(ctx).Warnw("misskey stream disconnected",
				"channel", c.name, "err", err, "reconnect_delay", delay)
		}

		// 如果连接存活时间超过最大退避窗口，重置退避（可能是临时断线）。
		if time.Since(start) > misskeyReconnectDelayMax {
			delay = misskeyReconnectDelayMin
		} else if err == nil {
			// 干净断开（context 取消）无需退避。
			delay = misskeyReconnectDelayMin
		}

		// 重连前等待
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}

		// 指数退避：翻倍直到上限。
		delay *= 2
		if delay > misskeyReconnectDelayMax {
			delay = misskeyReconnectDelayMax
		}
	}
}

// dedupCleanupLoop 定期清理过期的去重缓存。
func (c *MisskeyChannel) dedupCleanupLoop(ctx context.Context) {
	defer c.wg.Done()
	ticker := time.NewTicker(misskeyDedupCleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.dedupMu.Lock()
			now := time.Now()
			for id, ts := range c.dedup {
				if now.Sub(ts) > misskeyDedupTTL {
					delete(c.dedup, id)
				}
			}
			c.dedupMu.Unlock()
		}
	}
}

// dedupSeen 检查 noteID 是否在去重窗口内已处理过，如果未处理则标记为已处理。
func (c *MisskeyChannel) dedupSeen(noteID string) bool {
	if noteID == "" {
		return false
	}
	c.dedupMu.Lock()
	defer c.dedupMu.Unlock()
	if ts, seen := c.dedup[noteID]; seen && time.Since(ts) < misskeyDedupTTL {
		return true
	}
	c.dedup[noteID] = time.Now()
	return false
}

// getMentionAnchor 返回当前锚点（最近一次成功处理的提及帖 noteID）。
func (c *MisskeyChannel) getMentionAnchor() string {
	c.anchorMu.Lock()
	defer c.anchorMu.Unlock()
	return c.lastMentionID
}

// setMentionAnchor 推进锚点为指定 noteID（空值不更新）。
// 在实时 mention 处理与 backfill 处理两条路径上都会调用，保证锚点始终是
// 已处理过的最新提及，重连时以此恢复断连窗口。
//
// 单调递增守卫：Misskey aid 定长且按时间字典序递增，锚点只允许向更新的 ID
// 推进。timeline 路径的帖子可能与 main/backfill 并发到达，若允许回退，
// 一条乱序的旧帖会把锚点拉回去，下次 backfill 就会重复拉取已处理的提及
// （2026-09-01 生产事故：timeline 路径不推进锚点 + 锚点被旧值卡住，
// 两次断连把整晚的 mention 重放了两轮，重复回复外发）。
func (c *MisskeyChannel) setMentionAnchor(noteID string) {
	if noteID == "" {
		return
	}
	c.anchorMu.Lock()
	defer c.anchorMu.Unlock()
	if c.lastMentionID != "" && noteID <= c.lastMentionID {
		return
	}
	c.lastMentionID = noteID
}

// backfillMentions 重连后补发断连窗口内错过的 @提及/回复。
//
// Misskey streaming 在断线期间不会重放消息，导致那段时间内的 mention/reply 被静默丢弃。
// 本方法以 lastMentionID 为锚（sinceId，不含自身），翻页拉取 notes/mentions 把错过的历史补齐。
// 设计要点：
//   - 复用 handleNote 走与实时 mention 完全相同的归一化/去重/注入路径，Mentioned=true（视为直接互动）。
//   - 与实时流并发：backfill 期间新到的实时 mention 可能和本次拉取的帖子重叠，dedupSeen 去重兜底。
//   - 每处理一条即推进锚点，长 outage 中途若再次断线也能从断点续传。
//   - 翻页用 untilId 取更旧的一页（sinceId/untilId 均为开区间），封顶 maxTotal 防止极端长 outage 拉爆。
//   - 端点直接查实例数据库，不依赖 Meilisearch，即便 notes/search 挂了 backfill 仍可用。
func (c *MisskeyChannel) backfillMentions(ctx context.Context) {
	anchor := c.getMentionAnchor()
	if anchor == "" {
		traceid.L(ctx).Debugw("misskey backfill: skipped (no anchor yet)",
			"channel", c.name)
		return
	}
	const pageSize = 100
	const maxTotal = 300
	total := 0
	until := ""
	for total < maxTotal {
		notes, err := c.api.getMentions(ctx, anchor, until, pageSize)
		if err != nil {
			traceid.L(ctx).Warnw("misskey backfill: notes/mentions failed",
				"channel", c.name, "anchor", anchor, "err", err)
			return
		}
		if len(notes) == 0 {
			break
		}
		// notes/mentions 最新在前；按时间正序（最旧→最新）处理，锚点自然推进到最新。
		for i := len(notes) - 1; i >= 0; i-- {
			n := notes[i]
			if c.dedupSeen(n.ID) {
				c.setMentionAnchor(n.ID)
				continue
			}
			// 忽略 Bot 自己（理论上 notes/mentions 不会含自己，但保险）
			if n.UserID == c.botUserID || (n.UserID == "" && n.User.ID == c.botUserID) {
				c.setMentionAnchor(n.ID)
				continue
			}
			c.handleNote(ctx, n, "backfill", true)
			c.setMentionAnchor(n.ID)
		}
		total += len(notes)
		if len(notes) < pageSize {
			break
		}
		// 用本页最旧 note 作 until 上界，翻下一页更旧的提及。
		until = notes[len(notes)-1].ID
	}
	// 无论补没补到都要留痕：只记「补发成功」会导致日志无法区分
	// 「backfill 跑了但窗口内确实没丢」与「backfill 根本没跑」。
	if total > 0 {
		traceid.L(ctx).Infow("misskey backfill: recovered missed mentions after reconnect",
			"channel", c.name, "count", total, "anchor", anchor)
		return
	}
	traceid.L(ctx).Infow("misskey backfill: no missed mentions in disconnect window",
		"channel", c.name, "anchor", anchor)
}

// connectAndServe 建立 WebSocket 连接并持续处理消息。
// 阻塞直到连接断开或 ctx 被取消。
func (c *MisskeyChannel) connectAndServe(ctx context.Context) error {
	// 构建 streaming URL: wss://{host}/streaming?i={token}
	host := strings.TrimPrefix(strings.TrimPrefix(c.cfg.Host, "https://"), "http://")
	wsURL := fmt.Sprintf("wss://%s/streaming", host)

	// 准备订阅消息
	var connectMsgs []string

	// 订阅 main 通道（mention/reply 等事件）
	mainMsg, _ := json.Marshal(streamMessage{
		Type: "connect",
		Body: mustJSON(connectBody{
			Channel: "main",
			ID:      mainConnID,
		}),
	})
	connectMsgs = append(connectMsgs, string(mainMsg))

	// 按配置订阅各个 timeline 频道，连接 ID 形如 "tl:homeTimeline"。
	for _, tl := range c.cfg.TimelineChannels {
		tlMsg, _ := json.Marshal(streamMessage{
			Type: "connect",
			Body: mustJSON(connectBody{
				Channel: tl,
				ID:      timelineConnID(tl),
			}),
		})
		connectMsgs = append(connectMsgs, string(tlMsg))
	}

	cfg := http.WSConfig{
		WatchdogTimeout: c.cfg.WatchdogTimeout,
		PingInterval:    c.cfg.PingInterval,
		OnConnect: func(conn *http.WSConn) {
			// 发送所有订阅消息
			for _, msg := range connectMsgs {
				if err := conn.WriteText(msg); err != nil {
					traceid.L(ctx).Warnw("misskey stream: failed to send connect message",
						"channel", c.name, "err", err)
				}
			}
			traceid.L(ctx).Debugw("misskey stream: subscribed",
				"channel", c.name, "channels", connectMsgs)

			// 连接成功必须留痕：streamLoop 只在失败时打 WARN，
			// 没有这条日志就无法从日志确认断连后是否真的重连成功。
			n := c.streamConnects.Add(1)
			traceid.L(ctx).Infow("misskey stream connected",
				"channel", c.name, "connect_seq", n,
				"reconnect", n > 1, "subscribed", len(connectMsgs))

			// 保存连接引用，供后续动态订阅/退订投票帖的 note capture 流。
			c.wsMu.Lock()
			c.wsConn = conn
			c.wsMu.Unlock()
			// 重连后 Misskey 不会保留旧订阅，重新订阅仍在进行的投票帖，
			// 否则断连期间用户投的票会丢失、user_choice 再次超时。
			c.resubscribePollNotes(ctx)

			// 重连成功：补发断连窗口内错过的 @提及/回复（streaming 不重放历史消息）。
			// 首次连接时锚点已是启动时刻最新提及，backfill 自然无副作用。
			c.backfillMentions(ctx)
		},
		OnClose: func(code int, text string) {
			c.wsMu.Lock()
			c.wsConn = nil
			c.wsMu.Unlock()
		},
		OnError: func(err error) {
			traceid.L(ctx).Debugw("misskey ws error",
				"channel", c.name, "err", err)
		},
		OnText: func(text string) error {
			return c.handleStreamMessage(ctx, text)
		},
	}

	// DoWS 会阻塞直到连接关闭
	err := c.hc.Get(wsURL).
		SetContext(ctx).
		SetQuery("i", c.cfg.Token).
		DoWS(cfg)

	return err
}

// mustJSON 将 v 序列化为 json.RawMessage，序列化失败时返回空（仅用于已知可安全序列化的值）。
func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// handleStreamMessage 处理一条来自 streaming 的文本消息。
func (c *MisskeyChannel) handleStreamMessage(ctx context.Context, text string) error {
	var base streamMessage
	if err := json.Unmarshal([]byte(text), &base); err != nil {
		traceid.L(ctx).Debugw("misskey stream: failed to parse message",
			"channel", c.name, "raw", text, "err", err)
		return nil // 不中断连接
	}

	// noteUpdated：note capture 流事件，是**顶层消息**而非 channel 消息。
	// Misskey 的 pollVoted 只经 note capture 投递（subNote 订阅），main 流不含投票事件，
	// 因此必须在此分支接收投票结果，否则 user_choice 永远等不到答案（全 60s 超时）。
	// 形如 {"type":"noteUpdated","body":{"id":"<noteId>","type":"pollVoted",
	//        "body":{"choice":2,"userId":"..."}}}
	if base.Type == "noteUpdated" {
		// body 与 channelMessage 同构：{id, type, body}
		var nu channelMessage
		if err := json.Unmarshal(base.Body, &nu); err != nil {
			traceid.L(ctx).Debugw("misskey stream: failed to parse noteUpdated",
				"channel", c.name, "err", err)
			return nil
		}
		if nu.Type == "pollVoted" {
			c.handleNotePollVoted(ctx, nu.ID, nu.Body)
		}
		return nil
	}

	// 只处理 type=channel 的消息（服务端推送）
	if base.Type != "channel" {
		return nil
	}

	var chMsg channelMessage
	if err := json.Unmarshal(base.Body, &chMsg); err != nil {
		return nil
	}

	// main 通道事件：mention / reply（Bot 被明确提及）
	if chMsg.ID == mainConnID {
		switch chMsg.Type {
		case "mention", "reply":
			var note Note
			if err := json.Unmarshal(chMsg.Body, &note); err != nil {
				traceid.L(ctx).Debugw("misskey stream: failed to parse note",
					"channel", c.name, "type", chMsg.Type, "err", err)
				return nil
			}
			// 去重：防止 timeline 先投递了同一条帖子
			if c.dedupSeen(note.ID) {
				return nil
			}
			// 忽略 Bot 自己发的帖
			if note.UserID == c.botUserID || (note.UserID == "" && note.User.ID == c.botUserID) {
				return nil
			}
			c.handleNote(ctx, note, chMsg.Type, true) // Mentioned = true
			// 推进锚点：该 mention 已成功处理，后续断连以此 ID 为基准 backfill。
			c.setMentionAnchor(note.ID)
		case "pollVoted":
			// 用户对 bot 发出的投票帖进行了投票 → resolve user_choice
			c.handlePollVoted(ctx, chMsg.Body)
		case "notification":
			// 反应等通知：仅 reaction / reaction:grouped 入站感知，其余忽略
			c.handleNotification(ctx, chMsg.Body)
		default:
			// 忽略其他 main 事件（follow, renote 等）
		}
		return nil
	}

	// timeline 通道事件：时间线上的所有帖子（Bot 未被提及）。
	// 连接 ID 形如 "tl:homeTimeline"，任一订阅的 timeline 都走此分支。
	if strings.HasPrefix(chMsg.ID, "tl:") {
		switch chMsg.Type {
		case "note":
			var note Note
			if err := json.Unmarshal(chMsg.Body, &note); err != nil {
				traceid.L(ctx).Debugw("misskey stream: failed to parse timeline note",
					"channel", c.name, "err", err)
				return nil
			}
			// 去重
			if c.dedupSeen(note.ID) {
				return nil
			}
			// 忽略 Bot 自己发的帖
			if note.UserID == c.botUserID || (note.UserID == "" && note.User.ID == c.botUserID) {
				return nil
			}
			// 忽略 DM（timeline 不处理私聊）
			if note.Visibility == VisibilitySpecified {
				return nil
			}
			// 忽略没有文本且没有文件和 renote 的帖
			if note.Text == "" && len(note.Files) == 0 && note.Renote == nil {
				return nil
			}
			// timeline 上的帖子若明确指向本 Bot（字面 @ / 回复了 Bot 帖子 / mentions 含 Bot），
			// 视为被真人提及（Mentioned=true），走单聊路径由 Bot 自行决定回复。
			// 否则当同一条帖子先以 timeline 形式到达、main 通道的 mention/reply 事件因
			// 去重晚到被丢弃时，指向 Bot 的回复会被当成普通时间线消息，被聊天节奏按概率
			// 降频，用户体感即「回复了 Bot 却没反应」。详见 timelineMentioned。
			mentioned := c.timelineMentioned(note)
			c.handleNote(ctx, note, "timeline", mentioned)
			// 指向本 Bot 的帖子经 timeline 路径成功处理后，同样必须推进 mention 锚点。
			// 否则锚点停留在旧值，每次断连 backfill（sinceId=锚点）都会把这条及之后
			// 所有 mention 重新拉取一遍——2 分钟的 dedupSeen 窗口挡不住几十分钟后的
			// 重放，导致同一帖子被重复生成回复甚至重复外发（2026-09-01 事故根因）。
			if mentioned {
				c.setMentionAnchor(note.ID)
			}
		default:
			// 忽略其他 timeline 事件
		}
		return nil
	}

	return nil
}

// timelineMentioned 判断一条时间线帖子是否明确指向本 Bot。
// 命中任一条件即视为被真人提及（Mentioned=true），走单聊路径由 Bot 自行决定回复：
//  1. 正文字面 @botUsername（@username 或 @username@host）；
//  2. 回复目标就是 Bot 的帖子（点了 Bot 帖子的「回复」：Misskey 把 replyId 指向 Bot 帖，
//     但正文不一定写 @bot，纯靠字面 @ 会漏判）；
//  3. mentions 数组包含 Bot 的 userID（联邦/客户端显式 mention）。
//
// 仅依赖正文字面 @ 会漏判情形 2：时间线上先到达、main 通道 reply 事件后到的回复帖被当普通
// 时间线消息，按节奏概率降频，用户体感即「回复了 Bot 却没反应」。
// 复用初始化时按 bot 用户名编译的 mentionRe，与 handleNote 中剥离 @bot 文本的正则保持一致，避免误判。
func (c *MisskeyChannel) timelineMentioned(note Note) bool {
	// 1) 正文字面 @bot
	if c.mentionRe != nil && c.mentionRe.MatchString(note.Text) {
		return true
	}
	// botUserID 未就绪（理论上 Start 后即有）则跳过后续判断
	if c.botUserID == "" {
		return false
	}
	// 2) 回复目标就是 Bot 的帖子
	if note.Reply != nil && note.Reply.User.ID == c.botUserID {
		return true
	}
	// 3) mentions 数组含 Bot
	for _, m := range note.Mentions {
		if m == c.botUserID {
			return true
		}
	}
	return false
}

// handleNotification 处理 main 流 notification 事件。
// 仅将 reaction / reaction:grouped 归一化为 awareness-only 入站消息（空 Text + InjectContext），
// 硬抑制出站由 api 层 reaction-ack enricher 负责。其它通知类型一律忽略。
func (c *MisskeyChannel) handleNotification(ctx context.Context, body json.RawMessage) {
	var n reactionNotification
	if err := json.Unmarshal(body, &n); err != nil {
		traceid.L(ctx).Debugw("misskey stream: failed to parse notification",
			"channel", c.name, "err", err)
		return
	}
	switch n.Type {
	case "reaction", "reaction:grouped":
	default:
		return
	}
	if n.Note == nil || n.Note.ID == "" {
		return
	}
	// 通知本应只针对 bot 自己的帖；再校验 note 作者，避免误入站。
	noteOwner := n.Note.UserID
	if noteOwner == "" {
		noteOwner = n.Note.User.ID
	}
	if c.botUserID != "" && noteOwner != "" && noteOwner != c.botUserID {
		return
	}
	if c.dedupSeen(n.ID) {
		return
	}

	type reactor struct {
		user     User
		reaction string
	}
	var reactors []reactor
	if n.Type == "reaction" {
		uid := n.UserID
		if uid == "" {
			uid = n.User.ID
		}
		if uid == "" {
			return
		}
		if c.botUserID != "" && uid == c.botUserID {
			return // 忽略自赞
		}
		u := n.User
		if u.ID == "" {
			u.ID = uid
		}
		reactors = append(reactors, reactor{user: u, reaction: n.Reaction})
	} else {
		for _, r := range n.Reactions {
			uid := r.User.ID
			if uid == "" {
				continue
			}
			if c.botUserID != "" && uid == c.botUserID {
				continue
			}
			reactors = append(reactors, reactor{user: r.User, reaction: r.Reaction})
		}
		if len(reactors) == 0 {
			return
		}
	}

	primary := reactors[0]
	username := "@" + primary.user.Username
	if primary.user.Host != "" {
		username += "@" + primary.user.Host
	}
	displayName := primary.user.Name
	if displayName == "" {
		displayName = primary.user.Username
	}

	var parts []string
	for _, r := range reactors {
		acct := "@" + r.user.Username
		if r.user.Host != "" {
			acct += "@" + r.user.Host
		}
		emoji := r.reaction
		if emoji == "" {
			emoji = "?"
		}
		parts = append(parts, fmt.Sprintf("%s %s", acct, emoji))
	}
	list := strings.Join(parts, "、")
	guidance := "只需要知道这件事即可：不要回复、不要转发、不要回赞、也不要为此调用工具，除非对方同时还发了文字在找你。"
	inject := fmt.Sprintf("[Misskey 反应] %s 对你的帖子表态了。%s\n[note_id: %s]", list, guidance, n.Note.ID)

	createdAt := time.Now()
	if t, err := time.Parse(time.RFC3339, n.CreatedAt); err == nil {
		createdAt = t
	}

	reactionMeta := primary.reaction
	if n.Type == "reaction:grouped" && len(reactors) > 1 {
		var rs []string
		for _, r := range reactors {
			rs = append(rs, r.reaction)
		}
		reactionMeta = strings.Join(rs, ",")
	}

	metadata := map[string]any{
		"event_type":      "reaction",
		"channel_type":    "misskey",
		"note_id":         n.Note.ID,
		"reaction":        reactionMeta,
		"notification_id": n.ID,
		"username":        primary.user.Username,
		"display_name":    displayName,
		"acct":            username,
		"ack_only":        true,
		// 故意不设 reply_target：避免误串接到被表态的帖子。
	}
	reactorIDs := make([]string, 0, len(reactors))
	for _, r := range reactors {
		if r.user.ID != "" {
			reactorIDs = append(reactorIDs, r.user.ID)
		}
	}
	if len(reactorIDs) > 0 {
		metadata["reactor_ids"] = reactorIDs
	}

	coreMsg := core.Message{
		ID:            n.ID,
		BotID:         c.botID,
		Source:        c.name,
		Channel:       primary.user.ID, // 1:1 会话；grouped 用首位反应器
		ChatType:      core.ChatPrivate,
		UserID:        primary.user.ID,
		Text:          "", // 空 Text：不污染 L0（同心跳契约）
		InjectContext: inject,
		Mentioned:     false,
		FromIsBot:     primary.user.IsBot,
		MediaType:     "text/plain",
		Metadata:      metadata,
		CreatedAt:     createdAt,
	}

	if c.ingress == nil {
		return
	}
	if err := c.ingress.Receive(ctx, coreMsg); err != nil {
		traceid.L(ctx).Warnw("misskey reaction notification ingress receive failed",
			"channel", c.name, "notification_id", n.ID, "err", err)
	}
}

// handleNote 将一条 Misskey Note 转换为 core.Message 并注入 Ingress。
// mentioned 参数指示此 Note 是否明确 @提及了 Bot。
func (c *MisskeyChannel) handleNote(ctx context.Context, note Note, eventType string, mentioned bool) {
	text := strings.TrimSpace(note.Text)

	// 如果是纯 Renote（无自己的文字），回退到被 Renote 的帖子文本
	renoteFallback := false
	if text == "" && note.Renote != nil {
		if rt := strings.TrimSpace(note.Renote.Text); rt != "" {
			text = rt
			renoteFallback = true
		}
	}

	// 没有文本也没有附件，跳过
	if text == "" && len(note.Files) == 0 {
		return
	}

	// 从文本中剥离 @bot 提及（Bot 不需要看到自己被 @）
	// 使用正则确保精确匹配 @username 或 @username@host，不误匹配更长的用户名
	if c.mentionRe != nil {
		text = strings.TrimSpace(c.mentionRe.ReplaceAllString(text, ""))
	}

	// 剥离后如果为空但原来是 renote，再次回退
	if text == "" && note.Renote != nil {
		if rt := strings.TrimSpace(note.Renote.Text); rt != "" {
			text = rt
			renoteFallback = true
		}
	}

	// 如果仍然为空且没有附件，跳过
	if text == "" && len(note.Files) == 0 {
		return
	}

	// 构建回复/转发上下文，让 Bot 能看到用户回复了什么或转发了什么
	text = noteContext(note, text, renoteFallback)

	// 构建用户全名（@username@host 或 @username）
	username := "@" + note.User.Username
	if note.User.Host != "" {
		username += "@" + note.User.Host
	}

	displayName := note.User.Name
	if displayName == "" {
		displayName = note.User.Username
	}

	// 解析 createdAt
	createdAt := time.Now()
	if t, err := time.Parse(time.RFC3339, note.CreatedAt); err == nil {
		createdAt = t
	}

	// 分类帖子类型
	noteType := classifyNoteType(note)

	// 纯 Renote（Renote 指向的帖子、本身无正文）：Misskey 禁止对它发起文本回复
	// （API 返回 CANNOT_REPLY_TO_A_PURE_RENOTE）。标记后经 pure-renote enricher 抑制回复，
	// 避免生成注定失败的回复请求。注意 renoteFallback 已把被 Renote 帖的正文作为对话内容，
	// 所以 bot 仍能「看到并思考」这条纯 Renote，只是不回复。
	isPureRenote := note.Renote != nil && strings.TrimSpace(note.Text) == ""

	metadata := map[string]any{
		"note_id":      note.ID,
		"reply_target": note.ID, // outbound 回写时使用的精确目标（noteID）
		"username":     note.User.Username,
		"host":         note.User.Host,
		"visibility":   note.Visibility,
		"event_type":   eventType,
		"reply_id":     note.ReplyID,
		"renote_id":    note.RenoteID,
		"display_name": displayName,
		"acct":         username,
		"note_type":    noteType,
		"channel_type": "misskey", // Channel 类型，供 ToolSessionContext 使用
		core.MetaIsPureRenote: isPureRenote,
	}
	if len(note.Files) > 0 {
		metadata["file_count"] = len(note.Files)
		for i, f := range note.Files {
			metadata[fmt.Sprintf("file_%d_url", i)] = f.URL
			metadata[fmt.Sprintf("file_%d_name", i)] = f.Name
		}
	}

	// Mentioned 由调用方传入：mention/reply 事件为 true，timeline 事件为 false。
	// ChatType 方面，Misskey 没有传统意义上的 "群组" 概念，但我们按「交互性质」
	// 而非「帖子可见性」来判定单聊/群聊：
	//   被 @ / 被回复（Mentioned=true）→ 视为「单聊」（1:1 对话）：对方明确叫名，
	//     由 Bot 自行判断如何/是否回复（engagement 时序门控与节奏抑制都已对此放行）。
	//     无论原帖是 public/home/followers 还是 specified，一律归为 private（单聊语义），
	//     让 prompt 以「直接对话」而非「群聊广播」的语境组装。
	//   未被 @ 的时间线帖子（Mentioned=false）→ 视为「群聊/社交广播」（group），
	//     由 engagement + 节奏控制决定是否插话；其中 visibility=specified 的主动私信
	//     仍归为 private。
	// 注意：ChatType 只影响节奏策略键与 prompt 语境，不改变回复可见性
	// （回复可见性由 ReplyWithVisibility 取原帖 visibility 决定，公开 @ 仍是公开串接回复）。
	chatType := core.ChatGroup
	if mentioned || note.Visibility == VisibilitySpecified {
		chatType = core.ChatPrivate
	}

	// timeline 消息加上来源前缀（被明确提及的回复不标 [Timeline]，避免误导模型当作群聊广播）
	if eventType == "timeline" && !mentioned {
		text = fmt.Sprintf("[Timeline] @%s: %s", note.User.Username, text)
	}

	// 对方是 Bot 账号：在文本前标注，让 LLM 感知并自行决定是否/如何回复
	if note.User.IsBot {
		text = fmt.Sprintf("[对方是 Bot 账号 %s] %s", username, text)
	}

	// 把当前帖的 ID 附在消息末尾，仅供 LLM 调用反应/引用类工具时使用，
	// 避免它为了拿 noteId 去搜索（实例搜索服务 Meilisearch 常不可用）。
	// 这是给模型看的上下文，不是给用户的内容；prompt 已要求不得写进回复。
	if nid, ok := metadata["note_id"].(string); ok && nid != "" {
		text = fmt.Sprintf("%s\n[note_id: %s]", text, nid)
	}

	// 确定 Channel 标识：
	// - timeline 事件（社交时间线）→ 共享的社交空间，所有用户共享同一 channel scope
	// - mention/reply 事件（直接互动）→ 按 user ID 隔离，视为 1:1 对话
	// UserID 始终为发言者 ID，记忆系统据此为每个用户独立构建画像
	channelID := misskeyIngressChannelID(eventType, mentioned, note.Visibility, note.User.ID)

	coreMsg := core.Message{
		ID:        note.ID,
		BotID:     c.botID,
		Source:    c.name,
		Channel:   channelID,
		ChatType:  chatType,
		UserID:    note.User.ID,
		Text:      text,
		Mentioned: mentioned,
		FromIsBot: note.User.IsBot,
		MediaType: "text/plain",
		Metadata:  metadata,
		CreatedAt: createdAt,
	}

	if err := c.ingress.Receive(ctx, coreMsg); err != nil {
		traceid.L(ctx).Warnw("misskey ingress receive failed",
			"channel", c.name, "note_id", note.ID, "err", err)
	}
}

// classifyNoteType 判断帖子交互类型："note"（原创）/ "reply"（回复）/ "renote"（纯转发）/ "quote"（引用转发）。
func classifyNoteType(note Note) string {
	if note.RenoteID != "" {
		if strings.TrimSpace(note.Text) == "" && len(note.Files) == 0 {
			return "renote" // 纯转发
		}
		return "quote" // 引用转发（带评论）
	}
	if note.ReplyID != "" {
		return "reply"
	}
	return "note"
}

// noteContext 为帖子文本添加回复和转发上下文，让 Bot 能看到用户回复了什么或引用了什么。
// 回复/转发原文截断到 200 rune 以保持 prompt 简洁。
func noteContext(note Note, text string, skipRenote bool) string {
	// 回复上下文
	if note.Reply != nil && note.Reply.Text != "" {
		quoted := truncateRunes(strings.TrimSpace(note.Reply.Text), 200)
		sender := note.Reply.User.Name
		if sender == "" {
			sender = note.Reply.User.Username
		}
		if sender != "" && quoted != "" {
			if text == "" {
				text = fmt.Sprintf("[Reply to %s: %s]", sender, quoted)
			} else {
				text = fmt.Sprintf("[Reply to %s: %s]\n%s", sender, quoted, text)
			}
		}
	}

	// 转发上下文（如果 skipRenote 则跳过，因为转发文本已被用作主文本）
	if !skipRenote && note.Renote != nil && note.Renote.Text != "" {
		quoted := truncateRunes(strings.TrimSpace(note.Renote.Text), 200)
		sender := note.Renote.User.Name
		if sender == "" {
			sender = note.Renote.User.Username
		}
		if sender != "" && quoted != "" {
			if text == "" {
				text = fmt.Sprintf("[Renote from %s: %s]", sender, quoted)
			} else {
				text = fmt.Sprintf("[Renote from %s: %s]\n%s", sender, quoted, text)
			}
		}
	}

	return text
}

// truncateRunes 委托给 strutil.Truncate，保持包内调用简洁。
func truncateRunes(s string, maxRunes int) string {
	return strutil.Truncate(s, maxRunes)
}

// Stop 优雅停止 streaming。
func (c *MisskeyChannel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.stopped = true
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		traceid.L(ctx).Infow("misskey channel stopped", "channel", c.name)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reply 向指定帖子回复。便捷方法，供 Pipeline Action 处理器调用。
// 文本超长时自动截断到 3000 rune。回复使用 home 可见性。
// 如果回复目标帖子已被删除，放弃回复。
func (c *MisskeyChannel) Reply(ctx context.Context, noteID, text string) error {
	return c.ReplyWithVisibility(ctx, noteID, text, VisibilityHome)
}

// ReplyWithVisibility 向指定帖子回复，使用指定可见性。
func (c *MisskeyChannel) ReplyWithVisibility(ctx context.Context, noteID, text, visibility string) error {
	text = truncateRunes(strings.TrimSpace(text), misskeyMaxNoteLength)

	// 回复时自动 @ 被回复者及其他被 @ 的人（排除 Bot 自身）
	if noteID != "" {
		if replyNote, err := c.api.getNote(ctx, noteID); err == nil && replyNote != nil {
			if prefix := c.buildReplyMentionPrefix(replyNote); prefix != "" {
				text = prefix + text
				text = truncateRunes(text, misskeyMaxNoteLength) // 加前缀后再次裁切，防止超长
			}
		} else if err != nil {
			traceid.L(ctx).Debugw("misskey: getNote for reply mention prefix skipped",
				"channel", c.name, "note_id", noteID, "err", err)
		}
	}

	_, err := c.api.createNoteFull(ctx, text, noteID, "", visibility, "", nil)
	if err != nil {
		// 降级：纯 Renote 无法作为回复目标（Misskey 返回
		// code=CANNOT_REPLY_TO_A_PURE_RENOTE / HTTP 400）。此时改为发一条独立新帖
		// （去掉 replyID），正文已带 buildReplyMentionPrefix 构造的 @ 前缀，
		// 仍可 @ 到原 Renote 作者，等价于「对纯 Renote 说话」，避免整条回复丢失。
		if noteID != "" && strings.Contains(err.Error(), "CANNOT_REPLY_TO_A_PURE_RENOTE") {
			traceid.L(ctx).Warnw("misskey: reply target is a pure renote, downgrade to standalone note",
				"channel", c.name, "note_id", noteID)
			if _, retryErr := c.api.createNoteFull(ctx, text, "", "", visibility, "", nil); retryErr != nil {
				traceid.L(ctx).Warnw("misskey: standalone downgrade after pure-renote reply failure failed",
					"channel", c.name, "err", retryErr)
				return retryErr
			}
			return nil
		}
		traceid.L(ctx).Warnw("misskey: reply failed, target note may be deleted",
			"channel", c.name, "note_id", noteID, "err", err)
	}
	return err
}

// buildReplyMentionPrefix 构造回复时的 @ 前缀。
// 规则：至少 @ 被回复者（被回复 note 的作者）；若被回复 note 文本里还有其他 @ 账号，
// 排除 Bot 自身后一并 @ 上。返回形如 "@alice @bob " 的前缀（含末尾空格），无则空字符串。
func (c *MisskeyChannel) buildReplyMentionPrefix(replyNote *Note) string {
	if replyNote == nil {
		return ""
	}
	seen := make(map[string]bool)
	var handles []string
	add := func(handle string) {
		handle = strings.TrimSpace(handle)
		if handle == "" {
			return
		}
		// 提取纯用户名（去掉 @ 和 host）用于排除自身判断
		bare := strings.TrimPrefix(handle, "@")
		if at := strings.Index(bare, "@"); at >= 0 {
			bare = bare[:at]
		}
		if bare == c.botUsername {
			return // 排除 Bot 自身
		}
		if seen[handle] {
			return
		}
		seen[handle] = true
		handles = append(handles, handle)
	}

	// 被回复者（note 作者）必带
	if replyNote.User.Username != "" {
		h := "@" + replyNote.User.Username
		if replyNote.User.Host != "" {
			h += "@" + replyNote.User.Host
		}
		add(h)
	}
	// 被回复 note 文本里其他被 @ 的人
	for _, m := range mentionAnyRe.FindAllString(replyNote.Text, -1) {
		add(m)
	}

	if len(handles) == 0 {
		return ""
	}
	return strings.Join(handles, " ") + " "
}

// resolveBareMentions 解析出站文本中不带 host 的裸 @username 提及，
// 通过 Misskey API 查询是否为远程用户，若是则补全为 @username@host。
//
// 背景：LLM 从记忆里取出远程账号时只记了 username（如 @hko_en），
// Misskey 联邦协议要求跨站提及必须带 host，否则解析为 ?（未找到）。
// 本方法作为出站安全网：对每个裸 mention 做 searchUser，有 Host 则补全；
// 本站用户 Host 为空、查不到的用户、API 错误均原样保留（不丢数据）。
func (c *MisskeyChannel) resolveBareMentions(ctx context.Context, text string) string {
	matches := mentionAnyRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return text
	}

	// 收集所有裸 mention（不含 @host 部分）并去重
	bareSet := make(map[string]bool)
	for _, m := range matches {
		trimmed := strings.TrimPrefix(m, "@")
		if !strings.Contains(trimmed, "@") {
			bareSet[trimmed] = true
		}
	}
	if len(bareSet) == 0 {
		return text
	}

	// 逐个查询并构建替换映射
	replacements := make(map[string]string) // bare username → full @username@host
	for username := range bareSet {
		users, err := c.api.searchUser(ctx, username, 1)
		if err != nil || len(users) == 0 {
			continue // 查不到 / API 错误 → 原样保留
		}
		u := users[0]
		if u.Host != "" {
			replacements[username] = "@" + username + "@" + u.Host
		}
		// Host=="" 是本站用户，@username 已足够，无需替换
	}
	if len(replacements) == 0 {
		return text
	}

	// 执行替换（按长匹配优先排序，避免短名截断长名前缀）
	var pairs []struct{ old, new string }
	for old, new := range replacements {
		pairs = append(pairs, struct{ old, new string }{old, new})
	}
	slices := make([]string, 0, len(pairs))
	for _, p := range pairs {
		slices = append(slices, p.old, p.new)
	}
	replacer := strings.NewReplacer(slices...)
	return replacer.Replace(text)
}

// React 对帖子添加 emoji 反应。
func (c *MisskeyChannel) React(ctx context.Context, noteID, emoji string) error {
	// Misskey 反应格式：自定义 emoji 用 :name:，unicode emoji 直接使用
	if !strings.HasPrefix(emoji, ":") && !isUnicodeEmoji(emoji) {
		emoji = ":" + emoji + ":"
	}
	if err := c.api.createReaction(ctx, noteID, emoji); err != nil {
		// 幂等语义：已反应过视为成功，不向上层报错。
		if errors.Is(err, ErrAlreadyReacted) {
			return nil
		}
		return err
	}
	return nil
}

// Unreact 移除对帖子的反应。
func (c *MisskeyChannel) Unreact(ctx context.Context, noteID string) error {
	return c.api.deleteReaction(ctx, noteID)
}

// Send 实现 bot.Sender / outbound.ChannelSender 接口。
// 根据 Action 的内容回写消息到 Misskey。
//
// Action 字段约定：
//   - Action.Channel：回复目标的 noteID（来源于 Inbound 的 msg.Channel）
//   - Action.Payload：发送内容（string 类型的文本消息）
//   - Action.Metadata["visibility"]：帖子可见性（"public"/"home"/"followers"/"specified"，可选，默认 "home"）
//   - Action.Metadata["cw"]：CW 折叠文本（可选）
//
// 行为：
//   - ActionReply：回复目标帖子（文本超长自动截断到 3000 rune）
//   - 其他 ActionType：当前也按回复处理（后续扩展）
//
// PostTimeline 向本频道时间线发布一条顶层新帖（不回复任何已有帖子）。
//
// 与 Send（必须带 noteID 作为回复目标）不同，本方法用于「主动说点什么」场景
// （如心跳自主唤醒决定对外发声）。可见性默认 "home"，调用方可显式指定
// "public" / "home" / "followers" / "specified"。
// 返回新建帖子 ID。
func (c *MisskeyChannel) PostTimeline(ctx context.Context, text, visibility, cw string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("misskey post: empty text")
	}
	if visibility == "" {
		visibility = VisibilityHome
	}
	text = truncateRunes(text, misskeyMaxNoteLength)
	noteID, err := c.api.createNoteFull(ctx, text, "", "", visibility, cw, nil)
	if err != nil {
		traceid.L(ctx).Warnw("misskey: post to timeline failed",
			"channel", c.name, "err", err)
		return "", err
	}
	traceid.L(ctx).Infow("misskey: posted to timeline",
		"channel", c.name, "note_id", noteID, "visibility", visibility)
	return noteID, nil
}

func (c *MisskeyChannel) Send(ctx context.Context, action core.Action) error {
	noteID := action.Channel
	if noteID == "" {
		return fmt.Errorf("misskey send: empty noteID in action.Channel")
	}

	// 提取文本
	text, ok := action.Payload.(string)
	if !ok {
		return fmt.Errorf("misskey send: payload is %T, expected string", action.Payload)
	}
	if text == "" {
		return nil // 空消息不发送
	}

	// 解析可选的 Metadata 参数
	visibility := VisibilityHome
	cw := ""

	if action.Metadata != nil {
		if v, ok := action.Metadata["visibility"]; ok {
			if vis, ok := v.(string); ok && vis != "" {
				visibility = vis
			}
		}
		if v, ok := action.Metadata["cw"]; ok {
			if cwText, ok := v.(string); ok {
				cw = cwText
			}
		}
	}

	// 截断长文本
	text = truncateRunes(strings.TrimSpace(text), misskeyMaxNoteLength)

	// 回复时自动补全 @host：联邦/远程用户必须带 host 才能被正确提及并收到通知。
	// 若只写 @username，bot 在 maid.lat 上发的会指向本实例同名用户，远程用户收不到。
	// （outbound 走 Send 而非 ReplyWithVisibility，故此处需自行补全，不要依赖调用方。）
	if noteID != "" {
		if replyNote, err := c.api.getNote(ctx, noteID); err == nil && replyNote != nil {
			if prefix := c.buildReplyMentionPrefix(replyNote); prefix != "" {
				// 去除 LLM 文本开头可能重复写的裸 @被回复者（不带 host），避免重复 mention
				t := strings.TrimSpace(text)
				if replyNote.User.Username != "" {
					bare := "@" + replyNote.User.Username
					if strings.HasPrefix(t, bare+" ") || t == bare {
						t = strings.TrimSpace(strings.TrimPrefix(t, bare))
					}
				}
				text = prefix + t
				text = truncateRunes(text, misskeyMaxNoteLength) // 加前缀后再次裁切，防止超长
			}
		} else if err != nil {
			traceid.L(ctx).Debugw("misskey: getNote for send mention prefix skipped",
				"channel", c.name, "note_id", noteID, "err", err)
		}
	}

	// 解析裸 @mention 的 host：LLM 从记忆取出的远程账号可能只有 @username（如 @hko_en），
	// Misskey 联邦要求跨站提及带 host，否则显示为 ?。
	// 本站用户 Host 为空则不替换；查不到 / API 错误均原样保留。
	text = c.resolveBareMentions(ctx, text)

	// 构建回复
	_, err := c.api.createNoteFull(ctx, text, noteID, "", visibility, cw, nil)
	if err != nil {
		traceid.L(ctx).Warnw("misskey send: reply failed",
			"channel", c.name, "note_id", noteID, "err", err)
		return errs.Wrapf(err, "misskey send: reply to %q failed", noteID)
	}

	return nil
}

// isUnicodeEmoji 粗略判断字符串是否为 unicode emoji（非 ASCII 字符）。
func isUnicodeEmoji(s string) bool {
	for _, r := range s {
		// 排除 CJK 统一表意文字（0x4E00–0x9FFF）与日文假名（平假名 0x3040–0x309F、
		// 片假名 0x30A0–0x30FF），否则中文/日文会被误判为 emoji，导致 React 给中文加
		// 冒号变成 ":中文:" 而 400/无效反应（历史缺陷 5049）。
		if (r >= 0x3040 && r <= 0x30FF) || (r >= 0x4E00 && r <= 0x9FFF) {
			return false
		}
		if r > 0x2B00 { // emoji / 符号范围（➡️⭐✅ 等）
			return true
		}
	}
	return false
}

// misskeyIngressChannelID 决定入站 Message.Channel。
// timeline 上的普通帖共用 misskey:timeline；被 @ / specified 私信仍按用户隔离，
// 否则 user_choice 会把 ChatID 记成 misskey:timeline，投票者 userId 对不上永远超时。
func misskeyIngressChannelID(eventType string, mentioned bool, visibility, userID string) string {
	if eventType == "timeline" && !mentioned && visibility != VisibilitySpecified {
		return "misskey:timeline"
	}
	return userID
}

// pollVisibilityFromNote 把原帖可见性抄到投票回复上。specified 必须带 visibleUserIds，
// 否则要么 400，要么误发成 home 把私信问题泄漏到主页。
func pollVisibilityFromNote(note *Note) (visibility string, visibleUserIDs []string, err error) {
	if note == nil {
		return VisibilityHome, nil, nil
	}
	vis := note.Visibility
	if vis == "" {
		vis = VisibilityHome
	}
	if vis != VisibilitySpecified {
		return vis, nil, nil
	}
	asker := note.User.ID
	if asker == "" {
		asker = note.UserID
	}
	if asker == "" {
		return "", nil, fmt.Errorf("misskey specified note %s has no author id", note.ID)
	}
	return vis, []string{asker}, nil
}

// CreatePollNote 发布一条带投票的帖子，并注册 noteID → questionID 的映射。
// 当用户在 Misskey 上对此帖投票时，WS 收到 pollVoted 事件后会自动
// 调用 interaction.Resolve 唤醒 user_choice 工具的等待。
//
// 参数：
//   - question: 题目文本（会作为帖子正文）
//   - replyID: 回复目标帖子 ID（可选，用于回复某条消息）
//   - options: 选项文本列表（2~10 个）
//   - multiple: 是否多选
//   - timeoutSecs: 投票过期时间（秒）；<=0 时 Misskey poll 本身不过期，但本地 mapping 仍按 DefaultTimeoutSecs 回收
//   - questionID: 关联的 interaction questionID（投票结果回填目标）
//
// 返回新建帖子 ID。
func (c *MisskeyChannel) CreatePollNote(ctx context.Context, question, replyID string, options []string, multiple bool, timeoutSecs int, questionID string) (string, error) {
	if len(options) < 2 {
		return "", fmt.Errorf("misskey CreatePollNote: at least 2 options required, got %d", len(options))
	}
	if len(options) > 10 {
		options = options[:10]
	}

	var expiresAt int64
	if timeoutSecs > 0 {
		expiresAt = time.Now().Add(time.Duration(timeoutSecs) * time.Second).UnixMilli()
	}

	poll := &Poll{
		Choices:   options,
		Multiple:  multiple,
		ExpiresAt: expiresAt,
	}

	visibility := VisibilityHome
	var visibleUserIDs []string
	if replyID != "" {
		orig, gerr := c.api.getNote(ctx, replyID)
		if gerr != nil {
			return "", fmt.Errorf("misskey CreatePollNote: lookup reply %s: %w", replyID, gerr)
		}
		var visErr error
		visibility, visibleUserIDs, visErr = pollVisibilityFromNote(orig)
		if visErr != nil {
			return "", visErr
		}
	}
	noteID, err := c.api.createNoteWithPoll(ctx, question, replyID, visibility, "", poll, visibleUserIDs)
	if err != nil {
		return "", errs.Wrap(err, "misskey CreatePollNote")
	}

	c.pollNotesMu.Lock()
	st := &pollNoteState{
		QuestionID: questionID,
		Multiple:   multiple,
		OptionN:    len(options),
	}
	c.pollNotes[noteID] = st
	c.armPollExpiryLocked(st, noteID, timeoutSecs)
	c.pollNotesMu.Unlock()

	traceid.L(ctx).Infow("misskey: poll note created",
		"channel", c.name, "note_id", noteID, "question_id", questionID,
		"options", len(options), "multiple", multiple, "expires_in", timeoutSecs)

	// 订阅该投票帖的 note capture 流：pollVoted 只在此流投递，
	// 不订阅就永远收不到投票结果（main 流不含 pollVote）。
	c.subscribePollNoteWS(ctx, noteID)

	return noteID, nil
}

// noteConnID 把投票帖 noteID 映射成 streaming 订阅连接 ID。
// 用 "note:" 前缀与 main（main-1）/timeline（tl:xxx）订阅区分开。
// subscribePollNoteWS 订阅单条投票帖的 note capture 流。
//
// 注意：note capture **不是** channel —— Misskey 的 getChannelConstructor 里没有
// 名为 "note" 的 channel（connect{channel:"note"} 会被服务端 throw 掉、静默无订阅）。
// 正确协议是顶层消息 {"type":"subNote","body":{"id":"<noteId>"}}，
// 事件以顶层 {"type":"noteUpdated",...} 回来。
// 连接尚未建立时静默跳过（建连时 OnConnect 会 resubscribePollNotes 补齐）。
func (c *MisskeyChannel) subscribePollNoteWS(ctx context.Context, noteID string) {
	c.wsMu.Lock()
	conn := c.wsConn
	c.wsMu.Unlock()
	if conn == nil {
		traceid.L(ctx).Warnw("misskey: poll note subscribe skipped (ws not connected)",
			"channel", c.name, "note_id", noteID)
		return
	}
	msg, _ := json.Marshal(streamMessage{
		Type: "subNote",
		Body: mustJSON(map[string]string{"id": noteID}),
	})
	if err := conn.WriteText(string(msg)); err != nil {
		traceid.L(ctx).Warnw("misskey: failed to subscribe poll note stream",
			"channel", c.name, "note_id", noteID, "err", err)
		return
	}
	traceid.L(ctx).Infow("misskey: subscribed poll note capture stream",
		"channel", c.name, "note_id", noteID)
}

// unsubscribePollNoteWS 退订单条投票帖的 note 流（投票已结算或超时后调用）。
func (c *MisskeyChannel) unsubscribePollNoteWS(ctx context.Context, noteID string) {
	c.wsMu.Lock()
	conn := c.wsConn
	c.wsMu.Unlock()
	if conn == nil {
		return
	}
	msg, _ := json.Marshal(streamMessage{
		Type: "unsubNote",
		Body: mustJSON(map[string]string{"id": noteID}),
	})
	if err := conn.WriteText(string(msg)); err != nil {
		traceid.L(ctx).Debugw("misskey: failed to unsubscribe poll note stream",
			"channel", c.name, "note_id", noteID, "err", err)
		return
	}
	traceid.L(ctx).Infow("misskey: unsubscribed poll note capture stream",
		"channel", c.name, "note_id", noteID)
}

// resubscribePollNotes 重连成功后，重新订阅所有仍在进行中的投票帖。
// Misskey 不会在断线重连后保留旧订阅，不补齐会导致断连期间投票丢失。
func (c *MisskeyChannel) resubscribePollNotes(ctx context.Context) {
	c.pollNotesMu.Lock()
	ids := make([]string, 0, len(c.pollNotes))
	for id := range c.pollNotes {
		ids = append(ids, id)
	}
	c.pollNotesMu.Unlock()
	for _, id := range ids {
		c.subscribePollNoteWS(ctx, id)
	}
}

// handlePollVoted handles Misskey WS pollVoted from the main channel
// （仅某些 Misskey 衍生版本可能在 main 投递；标准版在 note 流，见 handleNotePollVoted）。
// When a user votes on bot's poll note, this finds the associated questionID
// via pollNotes map and calls interaction.ResolveFrom to unblock user_choice.
func (c *MisskeyChannel) handlePollVoted(ctx context.Context, body json.RawMessage) {
	// Misskey pollVoted body: {"noteId":"...","choice":0,"userId":"..."}
	var pv struct {
		NoteID string `json:"noteId"`
		Choice int    `json:"choice"`
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(body, &pv); err != nil {
		traceid.L(ctx).Debugw("misskey: failed to parse pollVoted",
			"channel", c.name, "err", err)
		return
	}
	if pv.NoteID == "" || pv.Choice < 0 {
		return
	}
	c.onPollVote(ctx, pv.NoteID, pv.Choice, pv.UserID)
}

// handleNotePollVoted handles pollVoted delivered on the note capture stream.
// 标准 Misskey 把 pollVoted 发到 note 流，body 为嵌套结构：
// {"id":"<noteID>","userId":"<帖主ID>","body":{"choice":N,"userId":"<投票人ID>"}}
func (c *MisskeyChannel) handleNotePollVoted(ctx context.Context, noteID string, body json.RawMessage) {
	// noteUpdated.body.body 形如 {"choice":2,"userId":"..."}；
	// noteID 由外层 noteUpdated.body.id 给出。
	var pv struct {
		Choice int    `json:"choice"`
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(body, &pv); err != nil {
		traceid.L(ctx).Debugw("misskey: failed to parse note pollVoted",
			"channel", c.name, "note_id", noteID, "err", err)
		return
	}
	if noteID == "" || pv.Choice < 0 {
		return
	}
	traceid.L(ctx).Infow("misskey: note pollVoted received",
		"channel", c.name, "note_id", noteID, "choice", pv.Choice, "voter", pv.UserID)
	c.onPollVote(ctx, noteID, pv.Choice, pv.UserID)
}

// onPollVote 是投票结算核心：根据 choice/voter 推进 pollNotes 状态，
// 单选首票即解封、多选 debounce 后回填、越界 choice 忽略。
func (c *MisskeyChannel) onPollVote(ctx context.Context, noteID string, choice int, voter string) {
	c.pollNotesMu.Lock()
	st, ok := c.pollNotes[noteID]
	if !ok {
		c.pollNotesMu.Unlock()
		traceid.L(ctx).Debugw("misskey: pollVoted for unknown note (not our poll)",
			"channel", c.name, "note_id", noteID)
		return
	}
	action := applyPollVote(st, choice)
	st.LastVoter = voter
	switch action {
	case pollVoteIgnore:
		c.pollNotesMu.Unlock()
		return
	case pollVoteResolveNow:
		qid := st.QuestionID
		sel := append([]int(nil), st.Selected...)
		voter := st.LastVoter
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		delete(c.pollNotes, noteID)
		c.pollNotesMu.Unlock()
		c.tryResolvePoll(ctx, noteID, qid, voter, sel, st)
		return
	case pollVoteDebounce:
		if st.timer != nil {
			st.timer.Stop()
		}
		st.timer = time.AfterFunc(pollMultiDebounce, func() {
			c.flushMultiPoll(ctx, noteID)
		})
		c.pollNotesMu.Unlock()
		return
	default:
		c.pollNotesMu.Unlock()
	}
}

type pollVoteAction int

const (
	pollVoteIgnore pollVoteAction = iota
	pollVoteResolveNow
	pollVoteDebounce
)

// addPollChoice 把 unique choice 记入 st.Selected；越界或重复返回 false。
func addPollChoice(st *pollNoteState, choice int) bool {
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

// applyPollVote 把一次 pollVoted 记入状态，决定立刻 Resolve / debounce / 忽略。
func applyPollVote(st *pollNoteState, choice int) pollVoteAction {
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

// armPollExpiryLocked 必须在持有 pollNotesMu 且 st 已写入 pollNotes 时调用。
func (c *MisskeyChannel) armPollExpiryLocked(st *pollNoteState, noteID string, timeoutSecs int) {
	if st == nil {
		return
	}
	if timeoutSecs <= 0 {
		timeoutSecs = interaction.DefaultTimeoutSecs
	}
	if st.expire != nil {
		st.expire.Stop()
	}
	qid := st.QuestionID
	st.expire = time.AfterFunc(time.Duration(timeoutSecs)*time.Second, func() {
		c.expirePollNote(noteID, qid)
	})
}

// expirePollNote 超时回收：确认同一 questionID/note 仍映射后删除，并停掉 debounce。
func (c *MisskeyChannel) expirePollNote(noteID, questionID string) {
	c.pollNotesMu.Lock()
	st, ok := c.pollNotes[noteID]
	if !ok {
		c.pollNotesMu.Unlock()
		return
	}
	if questionID != "" && st.QuestionID != questionID {
		c.pollNotesMu.Unlock()
		return
	}
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if st.expire != nil {
		st.expire.Stop()
		st.expire = nil
	}
	delete(c.pollNotes, noteID)
	c.pollNotesMu.Unlock()
	// 投票已过期，退订 note 流，避免订阅泄漏。
	c.unsubscribePollNoteWS(context.Background(), noteID)
}

func (c *MisskeyChannel) flushMultiPoll(ctx context.Context, noteID string) {
	c.pollNotesMu.Lock()
	st, ok := c.pollNotes[noteID]
	if !ok {
		c.pollNotesMu.Unlock()
		return
	}
	qid := st.QuestionID
	sel := append([]int(nil), st.Selected...)
	voter := st.LastVoter
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	delete(c.pollNotes, noteID)
	c.pollNotesMu.Unlock()
	if len(sel) == 0 {
		if st.expire != nil {
			st.expire.Stop()
			st.expire = nil
		}
		// 多选 debounce 窗口内无人投票，退订 note 流。
		c.unsubscribePollNoteWS(ctx, noteID)
		return
	}
	c.tryResolvePoll(ctx, noteID, qid, voter, sel, st)
}

// tryResolvePoll 在已从 pollNotes 摘掉 mapping 之后回填。voter 不匹配时把 mapping 放回去，
// 好让真正的会话主人稍后还能投；已终态 / 找不到则丢弃。
func (c *MisskeyChannel) tryResolvePoll(ctx context.Context, noteID, questionID, voterID string, selected []int, st *pollNoteState) {
	if !c.resolvePollVote(ctx, questionID, voterID, selected, noteID) {
		c.pollNotesMu.Lock()
		if _, exists := c.pollNotes[noteID]; !exists && st != nil {
			c.pollNotes[noteID] = st
		}
		c.pollNotesMu.Unlock()
		return
	}
	// 永久丢掉 mapping：停掉过期定时器，避免无人投票路径泄漏。
	if st != nil && st.expire != nil {
		st.expire.Stop()
		st.expire = nil
	}
}

// resolvePollVote 回填 user_choice。返回 true 表示 mapping 应丢弃。
func (c *MisskeyChannel) resolvePollVote(ctx context.Context, questionID, voterID string, selected []int, noteID string) bool {
	ans := interaction.Answer{
		Selected: selected,
		Via:      interaction.ViaMisskey,
	}
	q, err := interaction.Default().Lookup(questionID)
	if err != nil {
		traceid.L(ctx).Debugw("misskey: poll resolve lookup failed",
			"channel", c.name, "question_id", questionID, "err", err)
		c.unsubscribePollNoteWS(ctx, noteID) // mapping 已摘掉，退订 note 流
		return true
	}
	chatID := voterID
	if q.ChatID != "" && q.ChatID != voterID {
		traceid.L(ctx).Warnw("misskey: poll voter does not match question chat",
			"channel", c.name, "question_id", questionID,
			"chat_id", q.ChatID, "voter", voterID, "note_id", noteID)
		return false // 保留 mapping 与订阅，等待真正的会话主人投票
	}
	if q.ChatID == "" {
		chatID = ""
	}
	if err := interaction.Default().ResolveFrom(questionID, chatID, ans); err != nil {
		traceid.L(ctx).Warnw("misskey: pollVoted resolve failed",
			"channel", c.name, "question_id", questionID, "selected", selected, "err", err)
		c.unsubscribePollNoteWS(ctx, noteID)
		return true
	}
	traceid.L(ctx).Infow("misskey: poll vote resolved user_choice",
		"channel", c.name, "question_id", questionID, "note_id", noteID,
		"selected", selected, "user_id", voterID)
	c.unsubscribePollNoteWS(ctx, noteID)
	return true
}
