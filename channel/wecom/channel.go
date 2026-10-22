package wecom

// 企业微信（WeCom）智能机器人 Channel —— 长连接模式。
//
// 与 Mastodon / Misskey 一样是「WS 入站 + 帧出站」的对称结构，但有一处根本差异：
// **企业微信没有「公开时间线」**，所有消息都发生在单聊或群里，
// 因此这里不实现 core.TimelinePoster，也不实现 RecentChatLister（ChatRef.ID 是 int64，
// 而企微会话 id 是字符串）。
//
// 平台坑位全部标注在对应函数上，汇总见 docs/wecom-channel-design.md §9。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// Channel 类型标识。
const channelType = "wecom"

const (
	// wecomReconnectDelayMin / Max 指数退避重连区间。
	wecomReconnectDelayMin = 3 * time.Second
	wecomReconnectDelayMax = 5 * time.Minute
	// wecomStablePeriod 连接存活超过这个时长就认为「稳定」，重置退避。
	wecomStablePeriod = 30 * time.Second

	// wecomDedupTTL / CleanupEvery 去重缓存的 TTL 与清理间隔。
	// 企微不重放消息，去重主要防同一 msgid 因重试/多路径被投递两次。
	wecomDedupTTL        = 5 * time.Minute
	wecomCleanupEvery    = time.Minute
	wecomPruneLimiterDiv = 10 // 每 10 轮清理顺带清一次限流桶

	// defaultCaptionWindow 「文字 + 文件」合并窗口默认值（见 Config.CaptionWindow）。
	// 实测两条回调间隔 0.3~2.5s，取 3s 留出余量。
	defaultCaptionWindow = 3 * time.Second

	// wecomInboxSize 入站消息队列容量（见 inboxLoop）。
	// 企微限流 30 条/分钟，32 足够吸收一次媒体下载期间的堆积。
	wecomInboxSize = 32
)

// WeComChannel 是企业微信平台的输入端与输出端实现。
//
// 入站：WS 长连接（aibot_msg_callback）→ 归一化为 core.Message → Ingress
// 出站：WS 帧（aibot_send_msg）→ 实现 bot.Sender
type WeComChannel struct {
	name  string
	botID string
	cfg   Config
	hc    *http.Client
	// mediaHC 是下载媒体专用的 HTTP client（放宽响应体上限，见 fetchMedia 注释）。
	mediaHC *http.Client

	// fileSink 把入站文件写入 bot 工作空间（文件名 → 内容），返回工作空间相对路径。
	// 由 api 层注入（与 Telegram 同构）；为 nil 时大文件只能回退成占位文本。
	fileSink FileSinkFunc

	// mu 保护 ingress / sess / cancel / stopped。
	//
	// 与 Mastodon 同理：这些字段在 Start（持写锁）里写，却被**出站 goroutine**
	// （Send 来自 pipeline）与**连接 goroutine** 并发读，裸读是数据竞争。
	mu      sync.RWMutex
	ingress *inbound.Ingress
	sess    *wsSession
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool

	// kicked 收到 disconnected_event（本连接被新连接踢掉）后置位。
	// ⚠️ 置位后**必须停止重连**：一个 bot 只允许一条连接，两个实例互相踢会
	// 双双陷入「连上→被踢→重连→把对方踢掉」的死循环，谁也连不上。
	kicked atomic.Bool

	// connects 累计成功建立的连接次数。
	connects atomic.Int64

	// limiter 是 per-chat 出站限流（30 条/分钟）。
	limiter *sendLimiter

	// dedup 入站 msgid 去重。
	dedupMu sync.Mutex
	dedup   map[string]time.Time

	// pend 暂存「裸文本」，等可能紧随其后的媒体消息来合并（见 cfg.CaptionWindow）。
	// key 是 channelID + "|" + userID —— 合并只在**同人同会话**间发生，
	// 跨用户/跨群不能并，否则会把 A 的话塞给 B 的文件。
	pendMu sync.Mutex
	pend   map[string]*pendingCaption

	// streams 按会话标识索引的进行中流式消息（见 stream.go）。
	//
	// ⚠️ key 只能是会话标识：core.Action 没有 TraceID 字段，出站侧（Send）
	// 拿不到 trace，唯一能对上号的就是 action.Channel。
	streamMu sync.Mutex
	streams  map[string]*wecomStream

	// turnCtl 由 api 层注入的轮次控制能力（abort / append），见 turn.go。
	// 与 fileSink 同构：为 nil 时退化为「每条消息都开新一轮」，功能不受损。
	turnCtl TurnController

	// active 按会话标识索引的**正在执行**轮次（见 turn.go）。
	//
	// ⚠️ 判定「本轮是否还在跑」全靠这张表：企微没有引用/回复关系，
	// 用户连发两条消息时无法从协议上区分「补充」还是「另起话题」。
	turnMu sync.Mutex
	active map[string]*activeTurn

	// inbox 入站消息队列，由 inboxLoop 串行消费（见 enqueueMsg 的注释）。
	//
	// ⚠️ 不要在 WS 分发循环里直接处理消息回调：媒体下载最长 30s×4 项，
	// 期间 ACK 帧无人 resolve —— ping 会假失败，出站 aibot_send_msg 会在
	// callTimeout(15s) 上超时失败，连「对方正在下载大文件时 bot 回不了话」
	// 都会发生。队列把重活挪出分发循环，同时保持单线程顺序处理。
	inbox chan *callbackBody

	// runCtx 是 channel 生命周期上下文（Start 建立，Stop 取消）。
	// 延迟投递必须挂它：回调带进来的 ctx 随本次回调返回就结束了。
	runCtx context.Context
}

// pendingCaption 一条等待合并的裸文本消息。
type pendingCaption struct {
	msg   core.Message
	timer *time.Timer
}

// NewChannel 创建一个 WeComChannel。
func NewChannel(name, botID string, cfg Config) *WeComChannel {
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = wecomReconnectDelayMin
	}
	if cfg.WatchdogTimeout <= 0 {
		cfg.WatchdogTimeout = 120 * time.Second
	}
	if cfg.CaptionWindow == 0 {
		cfg.CaptionWindow = defaultCaptionWindow
	}
	// Secret 会出现在错误字符串里（订阅被拒时 errs 会把 errmsg 带出来），
	// 兜底注册到日志脱敏表，保证它无论走哪条路径都到不了 thinkbot.log。
	log.RegisterSecret(cfg.Secret)
	return &WeComChannel{
		name:    name,
		botID:   botID,
		cfg:     cfg,
		hc:      http.New(),
		mediaHC: http.New(http.WithHTTPClient(newMediaTransportClient()), http.WithMaxBodySize(wecomMediaMaxBytes+1024)),
		limiter: newSendLimiter(),
		dedup:   make(map[string]time.Time),
		pend:    make(map[string]*pendingCaption),
		inbox:   make(chan *callbackBody, wecomInboxSize),
		streams: make(map[string]*wecomStream),
		active:  make(map[string]*activeTurn),
	}
}

// FileSinkFunc 把入站媒体写入 bot 工作空间，返回工作空间内的相对路径。
// 与 channel/telegram 的同名类型一致（跨包复制，避免让 wecom 依赖 telegram）。
type FileSinkFunc func(ctx context.Context, botID, filename string, data []byte) (string, error)

// SetFileSink 注入入站文件写入器（由 api 层在构建 channel 后调用）。
func (c *WeComChannel) SetFileSink(fn FileSinkFunc) { c.fileSink = fn }

// Name 返回 Channel 名称。
func (c *WeComChannel) Name() string { return c.name }

// Type 返回 "wecom"。
func (c *WeComChannel) Type() string { return channelType }

// BotID 返回所属 Bot ID。
func (c *WeComChannel) BotID() string { return c.botID }

// Start 启动长连接（非阻塞）。
//
// ⚠️ 这里**不**做连通性预检：企微长连接的认证就是连接后的 aibot_subscribe，
// 而「一个 bot 只允许一条连接」意味着预检本身就会把已连接的实例踢掉。
// 凭据错误的表现是订阅被拒（errcode != 0），由 streamLoop 记日志并重连退避。
func (c *WeComChannel) Start(ctx context.Context, ingress *inbound.Ingress) error {
	if c.cfg.RobotID == "" {
		return errors.New("wecom channel: robot_id is required")
	}
	if c.cfg.Secret == "" {
		return errors.New("wecom channel: secret is required")
	}
	custom, err := validateEndpoint(c.cfg.endpoint(), c.cfg.AllowCustomEndpoint)
	if err != nil {
		return err
	}
	if custom {
		traceid.L(ctx).Warnw("wecom: using a non-official endpoint (allowCustomEndpoint opt-in); the bot secret will be sent to it",
			"channel", c.name, "endpoint", c.cfg.endpoint())
	}

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return errors.New("wecom channel: already stopped, cannot restart")
	}
	c.ingress = ingress
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	// 延迟投递（caption 合并窗口）要用它：回调 ctx 随本次回调返回就结束了。
	c.runCtx = runCtx
	c.mu.Unlock()

	// 注册 Bot 自身 ID：企微不会把机器人自己发的消息回推给我们（实测没有），
	// 但注册是零成本的第二道防线，且 aibotid 就是它的身份。
	ingress.RegisterSelfUserID(c.cfg.RobotID)

	c.wg.Add(5)
	go c.streamLoop(runCtx)
	go c.pingLoop(runCtx)
	go c.cleanupLoop(runCtx)
	go c.inboxLoop(runCtx)
	go c.streamTickLoop(runCtx)

	traceid.L(ctx).Infow("wecom channel started",
		"channel", c.name, "robot_id", c.cfg.RobotID, "endpoint", c.cfg.endpoint())
	return nil
}

// Stop 优雅停止长连接。
func (c *WeComChannel) Stop(ctx context.Context) error {
	// ⚠️ 先冲刷 caption 合并窗口里暂存的裸文本：runCtx 一取消，
	// 这些消息就再也投不出去了（用户那句配文会凭空消失）。
	// 先在锁内置 stopped，再冲刷：之后到达的 holdPending 会直接投递，
	// 已挂的定时器回调也会看到 stopped 而放弃（由 flushPending 统一投递）。
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.flushPending()

	// ⚠️ 收尾全部进行中的流：进程一走就再没人能 finish，
	// 客户端那条「正在思考…」会一直挂到服务端 10 分钟兜底。
	// 必须在取消 runCtx **之前**做 —— 取消后连接就断了，推不出去。
	c.abortStreams(c.deliveryCtx(nil))

	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.stopped = true
	// 主动关连接：runCtx 取消会让读循环退出，但显式 Close 能让对端立刻释放
	// 「一个 bot 一条连接」的占位 —— 否则下次启动会被自己上一次的僵尸连接踢掉。
	if c.sess != nil {
		_ = c.sess.conn.Close()
	}
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		traceid.L(ctx).Infow("wecom channel stopped", "channel", c.name)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ============================================================================
// 连接生命周期
// ============================================================================

// streamLoop 维护长连接，断线自动重连（指数退避）。
func (c *WeComChannel) streamLoop(ctx context.Context) {
	defer c.wg.Done()

	delay := c.cfg.ReconnectDelay
	for {
		if ctx.Err() != nil {
			return
		}

		start := time.Now()
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return // 主动停止
		}
		if c.kicked.Load() {
			traceid.L(ctx).Errorw("wecom: disconnected_event received — another connection took over this bot; "+
				"stop reconnecting to avoid a kick loop (restart thinkbot or the other instance)",
				"channel", c.name, "robot_id", c.cfg.RobotID)
			return
		}
		if err != nil {
			traceid.L(ctx).Warnw("wecom: connection lost",
				"channel", c.name, "err", err, "reconnect_delay", delay)
		}
		if time.Since(start) > wecomStablePeriod || err == nil {
			delay = c.cfg.ReconnectDelay
		}

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		if delay *= 2; delay > wecomReconnectDelayMax {
			delay = wecomReconnectDelayMax
		}
	}
}

// connectAndServe 建一次连接并阻塞到它断开。
func (c *WeComChannel) connectAndServe(ctx context.Context) error {
	cfg := http.WSConfig{
		WatchdogTimeout: c.cfg.WatchdogTimeout,
		// ⚠️ 故意**不开** WSConfig.PingInterval：那是 WebSocket 协议层的 ping/pong，
		// 企微要的是应用层 `{"cmd":"ping"}` 帧，由 channel 自己的 pingLoop 统一发。
		// 两套心跳并存只会让人以为「心跳开着」，实际对端根本不认。
		OnClose: func(code int, text string) {
			traceid.L(ctx).Debugw("wecom: ws closed", "channel", c.name, "code", code)
		},
		OnError: func(err error) {
			traceid.L(ctx).Debugw("wecom: ws error", "channel", c.name, "err", err)
		},
	}

	ch, conn, err := c.hc.Get(c.cfg.endpoint()).SetContext(ctx).DoWSMessages(cfg)
	if err != nil {
		return err
	}

	// ⚠️ 传 enqueueMsg 而不是 handleMsgCallback：媒体下载不能卡在分发循环里。
	sess := newWSSession(c.cfg, conn, c.enqueueMsg, c.handleEvent)
	c.setSession(sess)
	defer c.clearSession(sess)

	n := c.connects.Add(1)
	traceid.L(ctx).Infow("wecom: ws connected",
		"channel", c.name, "connect_seq", n, "reconnect", n > 1)

	return sess.run(ctx, ch)
}

// pingLoop 是**共享的单个**应用层心跳 ticker，随 channel 生命周期走。
//
// ⚠️ 不要按连接各起一个 ticker：重连时旧 goroutine 若没被正确回收，
// 跳数会随重连次数线性增长，直接打满「30 条/分钟」配额（连接心跳本身也可能计入）。
func (c *WeComChannel) pingLoop(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sess := c.currentSession()
			if sess == nil {
				continue // 正在重连，下一拍再试
			}
			if _, err := sess.call(ctx, cmdPing, nil); err != nil {
				traceid.L(ctx).Warnw("wecom: ping failed", "channel", c.name, "err", err)
			}
		}
	}
}

// inboxLoop 串行消费入站消息回调。
//
// 单线程是有意的：同一会话的消息必须按到达顺序处理，否则 caption 合并
// （先文本后媒体）会在乱序下失效。并发度不需要 —— 瓶颈在 LLM，不在解析。
//
// ⚠️ 挂的是 channel 的 runCtx：ctx 一取消（Stop），正在跑的媒体下载会立刻中断，
// 保证 wg.Wait() 不会卡在 30s 的下载超时上。
func (c *WeComChannel) inboxLoop(ctx context.Context) {
	defer c.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case body := <-c.inbox:
			if body == nil {
				continue
			}
			c.handleMsgCallback(ctx, body)
		}
	}
}

// enqueueMsg 把一条消息回调交给 inboxLoop，绝不阻塞 WS 分发循环。
//
// ⚠️ 队列满时**丢弃并报错**，而不是阻塞：阻塞等于把分发循环卡回去，
// 正是本队列要解决的故障。真丢消息必须留下 ERROR 级日志，不能静默。
//
// ⚠️ 不 close(inbox)：分发循环可能在 Stop 之后仍持有它并发来一帧，
// close 会 panic。靠 ctx 让 worker 退出即可。
func (c *WeComChannel) enqueueMsg(ctx context.Context, body *callbackBody) {
	if body == nil {
		return
	}
	select {
	case c.inbox <- body:
	default:
		traceid.L(ctx).Errorw("wecom: inbound queue full, dropping message",
			"channel", c.name, "msgid", body.MsgID, "cap", wecomInboxSize)
	}
}

// cleanupLoop 定期清理去重缓存与限流桶。
func (c *WeComChannel) cleanupLoop(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(wecomCleanupEvery)
	defer ticker.Stop()

	round := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-wecomDedupTTL)
			c.dedupMu.Lock()
			for id, t := range c.dedup {
				if t.Before(cutoff) {
					delete(c.dedup, id)
				}
			}
			c.dedupMu.Unlock()

			round++
			if round%wecomPruneLimiterDiv == 0 {
				c.limiter.prune()
				// 顺带收尾超时未完成的流（pipeline 静默时没人会来 finish）。
				c.reapStreams(ctx)
				// 顺带摘掉僵死的轮次（pipeline 决定不回复时没人来摘）。
				c.reapTurns(ctx)
			}
		}
	}
}

// ============================================================================
// 会话状态访问器
// ============================================================================

func (c *WeComChannel) setSession(s *wsSession) {
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
}

// clearSession 摘除会话；只有当它仍是当前会话时才摘（避免重连竞态下把新的摘掉）。
func (c *WeComChannel) clearSession(s *wsSession) {
	c.mu.Lock()
	if c.sess == s {
		c.sess = nil
	}
	c.mu.Unlock()
}

func (c *WeComChannel) currentSession() *wsSession {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sess
}

func (c *WeComChannel) isStopped() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stopped
}

func (c *WeComChannel) getIngress() *inbound.Ingress {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ingress
}

// markSeen 登记 msgid，返回 true 表示首次见到。
func (c *WeComChannel) markSeen(msgid string) bool {
	now := time.Now()
	c.dedupMu.Lock()
	defer c.dedupMu.Unlock()
	if _, ok := c.dedup[msgid]; ok {
		return false
	}
	c.dedup[msgid] = now
	return true
}

// ============================================================================
// 入站
// ============================================================================

// handleEvent 处理 aibot_event_callback。
func (c *WeComChannel) handleEvent(ctx context.Context, eventType string) {
	switch eventType {
	case EventDisconnected:
		// ⚠️ 必须停止重连（理由见 kicked 字段注释）。同时主动断开本连接，
		// 让 streamLoop 立刻退出而不是等到下一拍。
		c.kicked.Store(true)
		if s := c.currentSession(); s != nil {
			_ = s.conn.Close()
		}
		traceid.L(ctx).Errorw("wecom: disconnected_event — this connection was kicked by a newer one",
			"channel", c.name, "robot_id", c.cfg.RobotID)
	case EventEnterChat:
		traceid.L(ctx).Debugw("wecom: enter_chat event (welcome message not implemented yet)",
			"channel", c.name)
	case EventTemplateCard:
		// 一期不发模板卡片，不会出现；留个 debug 以防将来加了却忘了 5s 响应约束。
		traceid.L(ctx).Debugw("wecom: template_card_event (no card sent by us; nothing to respond)",
			"channel", c.name)
	case EventFeedback:
		traceid.L(ctx).Debugw("wecom: feedback_event (like/dislike; no action)", "channel", c.name)
	default:
		traceid.L(ctx).Debugw("wecom: unknown event", "channel", c.name, "event", eventType)
	}
}

// handleMsgCallback 把一条企微消息归一化为 core.Message 并投递给 Ingress。
func (c *WeComChannel) handleMsgCallback(ctx context.Context, body *callbackBody) {
	if body == nil || c.isStopped() {
		return
	}

	userID := body.From.UserID
	if userID == "" {
		traceid.L(ctx).Debugw("wecom: drop message without from.userid",
			"channel", c.name, "msgid", body.MsgID)
		return
	}
	if userID == c.cfg.RobotID {
		traceid.L(ctx).Debugw("wecom: drop message from bot itself", "channel", c.name)
		return
	}

	// ⚠️ 单聊的 chatid 是**空串**（实测），只能靠 chattype 判定、靠 userid 寻址。
	isGroup := strings.EqualFold(body.ChatType, "group") ||
		(body.ChatType == "" && body.ChatID != "")

	var channelID string
	if isGroup {
		if body.ChatID == "" {
			traceid.L(ctx).Warnw("wecom: drop group message without chatid",
				"channel", c.name, "msgid", body.MsgID, "user", userID)
			return
		}
		channelID = PrefixChat + body.ChatID
	} else {
		channelID = PrefixDM + userID
	}

	if body.MsgID == "" || !c.markSeen(body.MsgID) {
		traceid.L(ctx).Debugw("wecom: drop duplicate or unidentified message",
			"channel", c.name, "msgid", body.MsgID)
		return
	}

	text, mentioned, items := extractInbound(body, c.cfg.BotName, isGroup)
	text = strings.TrimSpace(text)
	if text == "" && len(items) == 0 {
		// 实测真实存在：用户只发一个 "@mybot "。剥完前缀就是空串，
		// 不挡掉会让 LLM 收到一条空消息（进而回一句莫名其妙的话）。
		traceid.L(ctx).Debugw("wecom: drop empty message after stripping @mention",
			"channel", c.name, "msgid", body.MsgID, "raw_type", body.MsgType)
		return
	}

	chatType := core.ChatPrivate
	if isGroup {
		chatType = core.ChatGroup
	}

	meta := map[string]any{
		"channel_type": channelType,
		// 出站寻址：企微没有 thread，回复目标就是会话本身。
		"reply_target": channelID,
		"chat_id":      body.ChatID,
		"msg_id":       body.MsgID,
		"chat_native":  strings.ToLower(body.ChatType),
		"aibot_id":     body.AIBotID,
	}
	// ⚠️ 刻意**不**把 response_url 塞进 metadata：它是一次性凭据（内含
	// response_code），会被一路带进会话历史与日志里，而我们一期根本用不到它。
	// 将来要做流式/分片回复时再从回调帧里取，用完即弃。

	msg := core.Message{
		ID:        body.MsgID,
		BotID:     c.botID,
		Source:    c.name,
		Channel:   channelID,
		ChatType:  chatType,
		UserID:    userID,
		Text:      text,
		Mentioned: mentioned,
		MediaType: mediaMIME(items),
		Metadata:  meta,
		CreatedAt: time.Now(),
	}

	// 开流（A 档）：登记这条回调的 req_id，占位由延迟定时器推，最终回复会原位替换。
	//
	// ⚠️ 必须在 caption 分支**之前**：走 holdPending 的消息要等 3s 才投递，
	// 那时再开流就已经晚了（占位要等 3s 后才出现）。放在这里两条路径都覆盖。
	//
	// ⚠️ req_id 是本次回调帧的；合并后的消息带的是**媒体那条**回调的 req_id，
	// 配文那条回调的 req_id 用不上（一次回复只能关联一次回调）。
	//
	// ⚠️ 只有「这条消息会起一个新轮次」时才开流（见 startsNewTurn）：
	// 终止指令要复用旧流、finish 时原位替换成「已停止。」；
	// append 补充不产生新气泡。给它开流会多冒一条「（这条已被新消息打断）」。
	if c.startsNewTurn(channelID, text) {
		c.openStream(ctx, channelID, body.ReqID, turnInterruptText)
	}

	// ⚠️ caption 合并（docs §9.2.4）：企微发文件时把配文拆成另一条**独立回调**
	// （两个 msgid、无任何关联字段），直接投递会让 bot 对「文字」和「文件」各回一次。
	// 裸文本先暂存一个窗口，期间同人同会话来了媒体消息就并进去、只发一条。
	//
	// ⚠️ mixed **也要参与认领**：群聊发图走的就是 mixed（实测），
	// 早期版本把 mixed 排除在外，于是群里「配文 + 图片」照样双回复 ——
	// 配文暂存 3s 后单独投递，图片另起一条。mixed 的正文里通常已经内联了同一句配文，
	// 所以下面合并前会先判重：认领到但不重复拼，等价于「丢弃重复的那条」。
	//
	// ⚠️⚠️ 认领配文必须发生在**下载媒体之前**：大文件下载+解密要好几秒，
	// 等下完再取暂存，3s 窗口早就过期了 —— 2026-10-20 16:12 的 6.5MB 实测就是
	// 这样差之毫秒没合并上（text 暂存于 :39.0，文件下完已是 :42.0，窗口 :42.0 到期）。
	// 认领提前后，窗口只受「两条回调的到达间隔」影响（实测最大 2.5s），与文件大小无关。
	var caption string
	var pendingMentioned bool
	if w := c.cfg.CaptionWindow; w > 0 {
		key := channelID + "|" + userID
		// ⚠️ bot 忙 / 终止指令时不暂存：打断与「停止」必须即时生效，
		// 再等 3s 合并窗口就太迟了（见 holdWorthy）。
		if len(items) == 0 && text != "" && c.holdWorthy(channelID, text) {
			c.holdPending(ctx, key, w, msg)
			return
		}
		if len(items) > 0 {
			if pm := c.takePending(key); pm != nil {
				caption = strings.TrimSpace(pm.Text)
				pendingMentioned = pm.Mentioned
			}
		}
	}

	// 媒体：下载 + 解密 + 转附件。失败也不影响消息入站（见 fetchMedia 注释）。
	atts, notes := c.fetchMedia(ctx, body.MsgID, items)
	if len(notes) > 0 {
		if text == "" {
			text = strings.Join(notes, "\n")
		} else {
			// ⚠️ 图文混排时**必须**把媒体提示追加进去，不能因为已有正文就丢掉：
			// 用户发「看这张图 <图>」时，模型只看到文字、看不到图片，
			// 会回答「我没收到图片」—— 这正是实测里暴露出来的现象。
			text = text + "\n" + strings.Join(notes, "\n")
		}
	}
	msg.Text = text
	if len(atts) > 0 {
		core.SetAttachments(&msg, atts)
	}
	// 合并后的消息继承配文的 @：群里常见「@bot 配文」+「不带 @ 的文件」。
	msg.Mentioned = msg.Mentioned || pendingMentioned
	if caption != "" {
		if strings.Contains(msg.Text, caption) {
			// mixed 把配文内联在 msg_item 里了：认领到但不再拼一遍。
			// 这一步是「认领」而不是「丢弃」—— 不认领的话它会在 3s 后单独投递成第二条回复。
			traceid.L(ctx).Debugw("wecom: caption already inlined in media message, dropping duplicate",
				"channel", c.name, "msgid", body.MsgID, "chat", channelID,
				"caption", strutil.Truncate(caption, 60))
		} else {
			msg.Text = mergeCaption(caption, msg.Text)
			traceid.L(ctx).Infow("wecom: merged caption into media message",
				"channel", c.name, "msgid", body.MsgID, "chat", channelID,
				"caption", strutil.Truncate(caption, 60))
		}
	}

	traceid.L(ctx).Infow("wecom: inbound message",
		"channel", c.name, "msgid", body.MsgID, "chat", channelID,
		"chat_type", chatType, "user", userID, "msgtype", body.MsgType,
		"mentioned", mentioned, "media", len(items), "attachments", len(atts),
		"text", strutil.Truncate(msg.Text, 120))

	c.deliver(ctx, msg)
}

// deliver 把归一化后的消息投给 Ingress。
//
// ⚠️ 这里也是**轮次控制的唯一入口**：立即投递 / 配文暂存后投递 / Stop 时 flush
// 三条路径都汇聚过来，而「本轮是否还在跑」必须等到**真要把消息交给 pipeline 的
// 这一刻**才判定 —— 配文暂存的那 3s 里 pipeline 还没起，那时就登记轮次，
// 会让随后的消息去打断一个根本没在跑的轮次（Abort 落空，还把人家的 traceID 摘了）。
func (c *WeComChannel) deliver(ctx context.Context, msg core.Message) {
	ing := c.getIngress()
	if ing == nil {
		return
	}
	// 停机回灌（flushPending）不做轮次判定：服务要关了，别再触发 abort。
	if !c.isStopped() {
		switch action, tid := c.resolveTurn(ctx, msg.Channel, msg.Text); action {
		case turnAppend:
			// 已注入正在跑的那一轮。tid 是**那一轮**的 traceID：
			// 起一个兜底 watcher，万一补充没被消费就退化成新一轮（见 watchAppend）。
			go c.watchAppend(c.deliveryCtx(ctx), msg, tid)
			return
		case turnStopped:
			return // 已终止当前轮
		case turnNew:
			// Ingress.Receive 会保留已设的 TraceID（为空才自己生成），
			// 所以我们在这里钉死它，后面才能拿同一个 ID 去 abort/append。
			msg.TraceID = tid
		}
	}
	// ⚠️ 必须在 Receive **之前**登记：Ingress 是异步派发的，
	// 晚一步登记会出现「pipeline 已在跑但活跃表里查不到」的空窗。
	c.beginTurn(msg.Channel, msg.TraceID)
	if err := ing.Receive(c.deliveryCtx(ctx), msg); err != nil {
		traceid.L(ctx).Warnw("wecom: ingress receive failed",
			"channel", c.name, "msgid", msg.ID, "err", err)
		c.clearTurn(msg.Channel, msg.TraceID)
	}
}

// deliveryCtx 选择投递用的 ctx：调用方给的 ctx 若为 nil（定时器路径）就用 channel 的
// runCtx，保证延迟投递不会挂在早已结束的回调 ctx 上。
func (c *WeComChannel) deliveryCtx(fallback context.Context) context.Context {
	if fallback != nil {
		return fallback
	}
	c.mu.RLock()
	rc := c.runCtx
	c.mu.RUnlock()
	if rc != nil {
		return rc
	}
	return context.Background()
}

// holdPending 暂存一条裸文本，窗口内没人来合并就照常投递。
//
// ⚠️ 同一 key 已有待投递文本时（用户连发多条）**直接放行旧的**，只暂存最新一条：
// 否则旧的那条会被悄悄吞掉，用户连发两句话只收到一次回复。
func (c *WeComChannel) holdPending(ctx context.Context, key string, w time.Duration, msg core.Message) {
	if c.isStopped() {
		c.deliver(ctx, msg)
		return
	}
	p := &pendingCaption{msg: msg}

	// 可观测性：暂存时刻必须落日志，否则「配文为什么没合并上」只能靠猜
	// （2026-10-20 16:12 的 6.5MB 案例就是靠这条的时间差定位到下载吃掉窗口的）。
	traceid.L(ctx).Infow("wecom: caption held, waiting for possible media",
		"channel", c.name, "msgid", msg.ID, "chat", msg.Channel,
		"window", w.String(), "text", strutil.Truncate(msg.Text, 60))

	// ⚠️ 摘旧桶 + 插新桶必须在**同一临界区**内完成。
	// 早期版本在中间放锁去投递旧消息，同一 key 的并发 holdPending 会趁机插入，
	// 随后被 `c.pend[key] = p` 覆盖 —— 那条消息的定时器还在，但表里已查不到自己，
	// 于是 `cur != p` 判重成立、直接 return：用户那句话**凭空消失**。
	var superseded *core.Message
	c.pendMu.Lock()
	if c.pend == nil {
		c.pend = make(map[string]*pendingCaption)
	}
	if old, ok := c.pend[key]; ok && old != nil {
		delete(c.pend, key)
		if old.timer != nil {
			old.timer.Stop()
		}
		m := old.msg
		superseded = &m
	}
	c.pend[key] = p
	p.timer = time.AfterFunc(w, func() {
		if c.isStopped() {
			return // Stop 已接管：flushPending 负责投递
		}
		c.pendMu.Lock()
		if cur, ok := c.pend[key]; !ok || cur != p {
			c.pendMu.Unlock()
			return // 已被合并或已被 flush，别重复投递
		}
		delete(c.pend, key)
		c.pendMu.Unlock()
		c.deliver(nil, p.msg)
	})
	c.pendMu.Unlock()

	// 被顶掉的旧消息照常投递（放在锁外，避免投递路径重入本锁）。
	if superseded != nil {
		c.deliver(ctx, *superseded)
	}
}

// takePending 取出并清除该 key 的待合并文本；没有则返回空串。
// 返回整条暂存消息（合并时还要继承它的 Mentioned 等字段）；没有则返回 nil。
func (c *WeComChannel) takePending(key string) *core.Message {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	p, ok := c.pend[key]
	if !ok || p == nil {
		return nil
	}
	delete(c.pend, key)
	if p.timer != nil {
		p.timer.Stop()
	}
	m := p.msg
	return &m
}

// flushPending 立即投递全部暂存的裸文本（Stop 时调用，避免停机丢消息）。
func (c *WeComChannel) flushPending() {
	c.pendMu.Lock()
	pending := c.pend
	c.pend = make(map[string]*pendingCaption)
	for _, p := range pending {
		if p != nil && p.timer != nil {
			p.timer.Stop()
		}
	}
	c.pendMu.Unlock()
	for _, p := range pending {
		if p == nil {
			continue
		}
		c.deliver(nil, p.msg)
	}
}

// mergeCaption 把暂存的配文并进媒体消息正文。
// 媒体消息自己的正文可能只是媒体提示（如「[收到文件 …]」），配文放前面更像人话。
func mergeCaption(caption, body string) string {
	caption = strings.TrimSpace(caption)
	body = strings.TrimSpace(body)
	switch {
	case caption == "":
		return body
	case body == "":
		return caption
	default:
		return caption + "\n" + body
	}
}

// extractInbound 从回调体里抽出 (正文, 是否被@, 媒体项列表)。
//
// ⚠️ 图文混排 `mixed` 的**顶层没有 text**（实测群聊发图走的就是它），
// 必须遍历 msg_item；只读 body.Text 会拿到空串，表现为「群里发图 bot 没反应」。
//
// ⚠️ 返回**列表**而不是单个媒体：一条 mixed 可能同时带多张图，
// 只留第一个会让「发三张图」变成「收到一张」。
func extractInbound(body *callbackBody, botName string, isGroup bool) (string, bool, []mediaItem) {
	var sb strings.Builder
	var items []mediaItem

	switch body.MsgType {
	case MsgTypeText:
		if body.Text != nil {
			sb.WriteString(body.Text.Content)
		}
	case MsgTypeMixed:
		if body.Mixed != nil {
			for _, it := range body.Mixed.Items {
				if it.MsgType == MsgTypeText {
					if it.Text != nil {
						sb.WriteString(it.Text.Content)
					}
					continue
				}
				items = append(items, itemFromMixed(it))
			}
		}
	case MsgTypeImage, MsgTypeVoice, MsgTypeFile, MsgTypeVideo:
		u, k := urlAndKey(body)
		items = append(items, mediaItem{Type: body.MsgType, URL: u, AESKey: k})
	default:
		// 未知类型：正文能救多少算多少；若带了任一媒体字段也一并收下。
		if body.Text != nil {
			sb.WriteString(body.Text.Content)
		}
		if it := firstMedia(body); it.URL != "" {
			items = append(items, it)
		}
	}

	text, mentioned := stripMention(sb.String(), botName, isGroup)
	return text, mentioned, items
}

// itemFromMixed 把 mixed 里的一项转成 mediaItem。
func itemFromMixed(it mixedItem) mediaItem {
	m := mediaItem{Type: it.MsgType}
	if it.Image != nil {
		m.URL, m.AESKey = it.Image.URL, it.Image.AESKey
	}
	return m
}

// urlAndKey 从顶层媒体字段里取 url/aeskey。
func urlAndKey(body *callbackBody) (url, aeskey string) {
	switch body.MsgType {
	case MsgTypeImage:
		if body.Image != nil {
			return body.Image.URL, body.Image.AESKey
		}
	case MsgTypeVoice:
		if body.Voice != nil {
			return body.Voice.URL, body.Voice.AESKey
		}
	case MsgTypeFile:
		if body.File != nil {
			return body.File.URL, body.File.AESKey
		}
	case MsgTypeVideo:
		if body.Video != nil {
			return body.Video.URL, body.Video.AESKey
		}
	}
	return "", ""
}

// firstMedia 在 msgtype 未识别时，按顺序找出第一个存在的媒体字段。
func firstMedia(body *callbackBody) mediaItem {
	for _, t := range []string{MsgTypeImage, MsgTypeFile, MsgTypeVideo, MsgTypeVoice} {
		switch t {
		case MsgTypeImage:
			if body.Image != nil {
				return mediaItem{Type: t, URL: body.Image.URL, AESKey: body.Image.AESKey}
			}
		case MsgTypeFile:
			if body.File != nil {
				return mediaItem{Type: t, URL: body.File.URL, AESKey: body.File.AESKey}
			}
		case MsgTypeVideo:
			if body.Video != nil {
				return mediaItem{Type: t, URL: body.Video.URL, AESKey: body.Video.AESKey}
			}
		case MsgTypeVoice:
			if body.Voice != nil {
				return mediaItem{Type: t, URL: body.Voice.URL, AESKey: body.Voice.AESKey}
			}
		}
	}
	return mediaItem{}
}

// mentionFallbackCount 统计「群消息里解析不出 @botName 但仍按被 @ 处理」的次数。
var mentionFallbackCount atomic.Int64

// stripMention 剥离群消息里的 @机器人，并回报是否被 @。
//
// ⚠️ 回调体里只有 aibotid，**没有机器人显示名**（实测），所以主要靠配置的 BotName。
//
// ⚠️ @ **不一定在开头**。实测用户会写「群聊带 @ 的模式。@mybot 看看能否收到」——
// @ 落在句中。只判 HasPrefix 会把这类消息判成「没被 @」→ 整条群聊链路团灭
// （2026-10-20 线上现象）。必须全串搜索。
//
// ⚠️ 兜底：企微**只在群里被 @ 时**才把消息推给机器人（实测：不带 @ 的群消息一条都
// 没送达，官方文档的回调示例正文也一律带 @）。所以「能收到」本身就已经说明被 @ 了，
// 解析不出 @ 多半是客户端渲染差异（全角、零宽字符等）—— 这里 fail-open 才对，
// fail-closed 会让机器人永久哑火。详见 docs/wecom-channel-design.md §9.2.2.1。
// 这条兜底只作用于群聊：单聊恒为 true，本来就不依赖 @ 解析。
func stripMention(text, botName string, isGroup bool) (string, bool) {
	t := strings.TrimSpace(text)
	if !isGroup {
		// 单聊里说的每一句都是对机器人说的。
		return t, true
	}
	if t == "" {
		return "", false
	}

	if botName != "" {
		at := "@" + botName
		if idx := strings.Index(t, at); idx >= 0 {
			// 剥掉这个 @token 与其后的一个空格，其余原样保留（@ 可能在句中）。
			rest := t[:idx] + strings.TrimPrefix(t[idx+len(at):], " ")
			return strings.TrimSpace(rest), true
		}
		// 没解析出 @：仍然算被 @（理由见函数注释的兜底说明）。
		// 计数 + Debug 日志：平台行为一旦变化（开始推送不带 @ 的群消息）能及时发现。
		n := mentionFallbackCount.Add(1)
		traceid.L(context.Background()).Debugw("wecom: group message without parsable @botName, treating as mentioned",
			"bot_name", botName, "fallback_total", n)
		return t, true
	}

	// 未配置 BotName：退化成「任意 @token」。注意这会误判 "@ " 之类的普通文本，
	// 所以 BotName 是推荐配置项。
	if idx := strings.Index(t, "@"); idx >= 0 {
		rest := t[:idx]
		if tail := t[idx+1:]; tail != "" {
			if i := strings.IndexAny(tail, " \t\u00a0"); i >= 0 {
				rest += tail[i+1:]
			}
		}
		return strings.TrimSpace(rest), true
	}
	return t, true
}

// mediaLabel 把媒体类型渲染成给 LLM 看的中文占位。
func mediaLabel(media string) string {
	switch media {
	case MsgTypeImage:
		return "图片"
	case MsgTypeVoice:
		return "语音"
	case MsgTypeFile:
		return "文件"
	case MsgTypeVideo:
		return "视频"
	default:
		return "附件"
	}
}

// mediaMIME 把媒体类型映射到 core.Message.MediaType（取第一个媒体项为代表）。
func mediaMIME(items []mediaItem) string {
	if len(items) == 0 {
		return ""
	}
	switch items[0].Type {
	case MsgTypeImage:
		return "image/*"
	case MsgTypeVoice:
		return "audio/*"
	case MsgTypeVideo:
		return "video/*"
	case MsgTypeFile:
		return "application/octet-stream"
	default:
		return ""
	}
}

// ============================================================================
// 出站
// ============================================================================

// Send 实现 bot.Sender / outbound.ChannelSender。
//
// Action 字段约定：
//   - Action.Channel：会话标识（入站 Message.Metadata["reply_target"] 原样回传），
//     形如 `wecom:dm:<userid>` / `wecom:chat:<chatid>` —— 前缀同时编码了 chat_type，
//     出站寻址只需要这一个字段。
//   - Action.Payload：文本内容（string）
func (c *WeComChannel) Send(ctx context.Context, action core.Action) error {
	target := action.Channel
	if target == "" {
		return errors.New("wecom send: empty target in action.Channel")
	}
	text, ok := action.Payload.(string)
	if !ok {
		return fmt.Errorf("wecom send: payload is %T, expected string", action.Payload)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil // 空消息不发送
	}

	// ⚠️ 刻意**不**在这里摘活跃轮次：core.Action 不带 TraceID，出站侧认不出
	// 自己在服务哪一輪，无差别摘除会把「下一轮刚登记、本轮才刚出结果」的
	// 竞态窗口变成误摘。轮次是否还在跑一律以编排侧的 IsActive 为准
	// （见 turn.go 的 turnRunning）。

	chatID, chatType, err := parseTarget(target)
	if err != nil {
		return err
	}
	if c.isStopped() {
		return errors.New("wecom send: channel stopped")
	}
	sess := c.currentSession()
	if sess == nil {
		return errors.New("wecom send: no active connection (reconnecting)")
	}

	// 流式优先：本轮若开着一条流，用 finish=true 原位替换掉「正在思考…」。
	//
	// ⚠️ 必须放在整段发送**之前**，且失败要能落回去 —— 流式只是「更好看的同一条回复」，
	// 它失败绝不能变成「消息发不出去」。
	// ⚠️ 走流式时**不截断**：实测流式通道不受 markdown 的 20480 字节上限
	// （27049 字节照样成功），截掉反而会丢内容。
	// ⚠️ 也**不走** limiter：那是 aibot_send_msg 的 30 条/分钟口径，
	// 实测 respond 连推 13 次（≈5 次/秒）零失败，不属于该配额。
	if c.finishStream(ctx, target, text) {
		traceid.L(ctx).Debugw("wecom send: ok via stream", "channel", c.name, "target", target)
		return nil
	}

	// 30 条/分钟/会话。宁可晚两秒发出去，也不要触发限流后整条丢掉。
	if err := c.limiter.wait(ctx, target); err != nil {
		return err
	}

	if err := sess.sendMarkdown(ctx, chatID, chatType, truncateForMarkdown(text)); err != nil {
		traceid.L(ctx).Warnw("wecom send: failed",
			"channel", c.name, "target", target,
			"text", strutil.Truncate(text, 120), "err", err)
		return err
	}
	traceid.L(ctx).Debugw("wecom send: ok", "channel", c.name, "target", target)
	return nil
}

// parseTarget 把会话标识拆成 (chatid, chat_type)。
//
// ⚠️ chat_type 必须显式给出：缺省（0）会被服务端按群聊解析，
// 导致**单聊静默发不出去**（连接正常、ACK 也可能正常，就是对方收不到）。
func parseTarget(target string) (chatID string, chatType int, err error) {
	switch {
	case strings.HasPrefix(target, PrefixChat):
		chatID = strings.TrimPrefix(target, PrefixChat)
		if chatID == "" {
			return "", 0, fmt.Errorf("wecom: empty chatid in target %q", target)
		}
		return chatID, ChatTypeGroup, nil
	case strings.HasPrefix(target, PrefixDM):
		chatID = strings.TrimPrefix(target, PrefixDM)
		if chatID == "" {
			return "", 0, fmt.Errorf("wecom: empty userid in target %q", target)
		}
		return chatID, ChatTypeSingle, nil
	default:
		return "", 0, fmt.Errorf("wecom: unrecognized target %q (want %q… or %q…)",
			target, PrefixDM, PrefixChat)
	}
}
