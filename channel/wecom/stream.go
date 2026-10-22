package wecom

// 流式回复（长连接模式的「类 SSE」能力）。
//
// 协议：aibot_respond_msg + msgtype=stream，req_id **必须透传消息回调的 req_id**
// （见 types.go 的 callbackBody.ReqID）。content 是**全量内容**，同 stream.id 再推一次
// 就是客户端原位替换；finish=true 收尾。
//
// 2026-10-22 真机实测（15/15 步全 ok，见 docs §9.2.9）：
//   - 首推 / 反复刷新 / finish 全部 errcode=0
//   - 连推 13 次（约 5 次/秒，即 300 次/分钟）**零失败**
//     → respond 不落在「30 条/分钟/会话」那条限流上（那是 aibot_send_msg 的口径）
//   - 单次 content 27049 字节也成功 → 流式通道**不**受 markdown 的 20480 字节上限
//   - 与 aibot_send_msg 并存不冲突（探针刷流的同时 bot 照常整段回复）
//
// 本文件只做 A 档：占位 + 最终原位替换。完整执行过程（订阅 EventBus 刷工具进度）
// 是 B 档，留在这里的同一个 sendStream 出口上扩展。

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

const (
	// defaultPlaceholderDelay 入站后多久才推「正在思考…」。
	//
	// 为什么不是立刻推：快回复（<1.5s）如果也推一条占位，用户会看到
	// 「思考中」闪一下就被替换 —— 观感比干等还差。延迟推则快回复完全无感。
	// 实测 bot 单轮 17~20s，绝大多数场景都会落在占位之后。
	defaultPlaceholderDelay = 1500 * time.Millisecond

	// defaultStreamMaxAge 一条流在我们这侧的最长存活。
	//
	// ⚠️ 服务端硬上限是「首次推送起 10 分钟」，超时自动结束。取 5 分钟留一倍余量：
	// 到点强制 finish，避免 pipeline 静默（决定不回复）时消息永远卡在「正在思考…」。
	defaultStreamMaxAge = 5 * time.Minute

	// defaultPlaceholderRefresh 占位的刷新间隔（见 Config.PlaceholderRefresh）。
	defaultPlaceholderRefresh = 5 * time.Second

	// defaultDeltaInterval 生成过程中增量内容的最小推送间隔
	// （见 Config.StreamDeltaInterval）。
	defaultDeltaInterval = 2 * time.Second

	// streamPlaceholder 占位文案。
	streamPlaceholder = "正在思考…"

	// streamDraftSuffix 增量正文的尾标。
	//
	// 企微的流是**原位替换**，用户在气泡里看到文字一段段长出来，
	// 没有尾标根本分不清「还在写」还是「已经写完了，就这么短」。
	streamDraftSuffix = "\n\n（还在写…）"

	// streamToolSuffix 有工具正在跑时的尾标。
	//
	// 与正文尾标分开：这一刻 bot 并没有在「写」，它在等工具返回 ——
	// 统一写「还在写…」是句假话，而且恰好掩盖了「正在调工具」这个最该让用户知道的状态。
	streamToolSuffix = "\n\n（工具执行中…）"

	// maxToolLines 气泡里最多保留多少行工具活动。
	//
	// ⚠️ 工具行不随正文增长而丢弃，一个跑偏的 agent 调上百次工具会把气泡撑爆，
	// 而且每次推送都要把这一整坨重发一遍。到顶后不再追加（结束事件也就无从改写，忽略即可）。
	maxToolLines = 20
)

// 看门狗参数。用 var 而不是 const：单测要把它们压到毫秒级，
// 否则验证一次「轮次结束后确实退订」要等 6 秒。
var (
	// deltaLivenessInterval 检查「本轮还在不在跑」的间隔（见 feedStream 的看门狗）。
	deltaLivenessInterval = 1 * time.Second

	// deltaStartGrace 容忍「编排侧还没登记本轮」的时间。
	//
	// feedStream 起在 Ingress.Receive **之前**，那一瞬间 IsMessageActive 必然是
	// false（还没登记）。没有这段宽限期的话，看门狗第一轮就把自己判成
	// 「轮次已结束」退出了 —— 增量推送永远一条都推不出去。
	deltaStartGrace = 5 * time.Second
)

// wecomStream 是一条进行中的流式消息。
type wecomStream struct {
	reqID    string
	streamID string
	sess     *wsSession
	started  time.Time

	timer *time.Timer // 占位延迟定时器

	// pushed 是否推送过任意内容。false 表示「首次推送即 finish」，
	// 这条路径服务端是否接受**未实测** —— 出现时会打日志，便于事后确认。
	pushed bool
	// content 最近一次推送的全量内容（超时兜底时用它收尾）。
	content string
	closed  bool

	// fed 是否已推过**真实增量内容**（B 档）。
	//
	// ⚠️ 置位后占位刷新与占位推送都必须停手：那条「正在思考… 12s」
	// 会把已经推上去的正文覆盖回占位文案，用户会看到内容倒退。
	fed bool

	// refreshed 已刷新占位的次数。只用于可观测性：成功刷新的日志
	// 每流只留第一条，否则长任务会刷出一整屏。
	refreshed int

	// fakePush 单测用的推送出口替换。wsSession 依赖真实 WS 连接，
	// 单测里构造不出来 —— 让流式逻辑（占位 / 增量 / 收尾）仍可被真实覆盖。
	fakePush func(content string, finish bool) error

	// pushMu 串行化同一条流的推送。
	//
	// ⚠️ 必须：conn.respond 对同一 req_id 只允许一个在途请求，撞上就报
	// 「already in flight」**直接丢帧**。而推送方不止一个 goroutine ——
	// 占位刷新的 ticker、推增量的 feedStream、Send 的收尾，三者会并发。
	// 实测企微 ACK 偶尔要 5s（2026-10-22 14:55:08 真机丢帧），这 5s 里谁撞上谁丢。
	// 丢一条占位刷新只是少跳一次秒；丢掉**收尾帧**会让 Send 回退成整段发送，
	// 用户就多收到一个气泡。
	pushMu sync.Mutex
}

// errPushBusy 上一条推送还在等 ACK，这一条主动放弃（见 tryPush）。
var errPushBusy = errors.New("wecom: previous push still awaiting ack")

// push 推送一帧流式内容。**流式层唯一的出口**，替换 fakePush 即可单测。
//
// ⚠️ 会阻塞到上一条推送收到 ACK（见 pushMu）。正文与收尾必须走这里 ——
// 它们丢了就是少内容 / 多气泡，宁可等。
func (st *wecomStream) push(ctx context.Context, content string, finish bool) error {
	st.pushMu.Lock()
	defer st.pushMu.Unlock()
	return st.pushLocked(ctx, content, finish)
}

// tryPush 非阻塞版本：拿不到锁立刻放弃，返回 errPushBusy。
//
// 只给**占位刷新**用：它是纯装饰性的，少刷一次秒无所谓；
// 为它去阻塞共享 ticker（甚至拖慢真正的正文推送）完全不划算。
func (st *wecomStream) tryPush(ctx context.Context, content string, finish bool) error {
	if !st.pushMu.TryLock() {
		return errPushBusy
	}
	defer st.pushMu.Unlock()
	return st.pushLocked(ctx, content, finish)
}

func (st *wecomStream) pushLocked(ctx context.Context, content string, finish bool) error {
	if st.fakePush != nil {
		return st.fakePush(content, finish)
	}
	if st.sess == nil {
		return errors.New("wecom: stream has no session")
	}
	return st.sess.sendStream(ctx, st.reqID, st.streamID, content, finish)
}

// DeltaKind 增量种类。
type DeltaKind int

const (
	// DeltaText 正文增量。
	DeltaText DeltaKind = iota
	// DeltaToolStart 开始调用一个工具（Text 是工具名）。
	DeltaToolStart
	// DeltaToolDone 一个工具执行结束（Text 是工具名，Failed 表示失败）。
	DeltaToolDone
)

// Delta 一条推给渠道的增量。
//
// ⚠️ 工具只带**名字**，不带参数与返回值：企微是外部渠道，参数里可能有
// 绝对路径、命令行、URL，带上就等于把内部信息写进聊天记录。
type Delta struct {
	Kind   DeltaKind
	ID     string // 工具调用 ID，用于把「正在调用」改写成「完成」
	Text   string // 正文增量 或 工具名
	Failed bool   // DeltaToolDone 专用
}

// DeltaSourceFunc 订阅一条 trace 的**已清洗**增量（B 档：边生成边推）。
//
// 正文增量在 agent/stages/llmroute.go 里已串过 reply-control 过滤与出站清洗
// （<think>/<internal> 块丢弃、<public> 标签剥离），**可以直接推给外部渠道**，
// 无需在 channel 侧再洗一遍 —— 与 Web SSE 推给前端的是同一份。
//
// 由 api 层注入（与 fileSink 同构）：channel 包不该反向依赖 agent/outbound。
//
// 返回：增量通道（关闭 = 本轮生成结束）与退订函数；通道为 nil 表示不可用。
type DeltaSourceFunc func(traceID string) (<-chan Delta, func())

// SetDeltaSource 注入增量源（由 api 层在构建 channel 后调用）。
func (c *WeComChannel) SetDeltaSource(fn DeltaSourceFunc) { c.deltaSrc = fn }

// streamEnabled 是否启用流式回复。
func (c *WeComChannel) streamEnabled() bool { return c.cfg.StreamReply }

func (c *WeComChannel) placeholderDelay() time.Duration {
	if c.cfg.PlaceholderDelay > 0 {
		return c.cfg.PlaceholderDelay
	}
	return defaultPlaceholderDelay
}

func (c *WeComChannel) streamMaxAge() time.Duration {
	if c.cfg.StreamMaxAge > 0 {
		return c.cfg.StreamMaxAge
	}
	return defaultStreamMaxAge
}

func (c *WeComChannel) placeholderRefresh() time.Duration {
	if c.cfg.PlaceholderRefresh != 0 {
		return c.cfg.PlaceholderRefresh
	}
	return defaultPlaceholderRefresh
}

// deltaInterval 增量内容的最小推送间隔；<0 表示关闭增量推送。
func (c *WeComChannel) deltaInterval() time.Duration {
	if c.cfg.StreamDeltaInterval != 0 {
		return c.cfg.StreamDeltaInterval
	}
	return defaultDeltaInterval
}

// toolActivityEnabled 是否在气泡里显示工具活动行。
//
// ⚠️ 只显示工具**名**（见 toolActivityLine），不显示参数与返回值。
// 默认开：工具调用恰好落在「模型在想、还没写正文」的空窗里，
// 关掉它那段空窗就又变成纯盲等。
func (c *WeComChannel) toolActivityEnabled() bool { return c.cfg.StreamToolActivity }

// currentStream 取会话当前登记的流（不存在则 nil）。
func (c *WeComChannel) currentStream(key string) *wecomStream {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	st, ok := c.streams[key]
	if !ok || st == nil {
		return nil
	}
	return st
}

// openStream 为一次入站回调登记一条潜在流。
//
// ⚠️ 此时**不推送**任何东西：占位由延迟定时器触发（见 defaultPlaceholderDelay）。
// 若 pipeline 在延迟内就出结果，Send 会直接 finish —— 全程零中间态。
//
// key 是会话标识（channelID），与 Send 的 action.Channel 对齐：
// core.Action 没有 TraceID 字段，出站侧只能靠它找回对应的流。
func (c *WeComChannel) openStream(ctx context.Context, key, reqID, supersedeText string) {
	if !c.streamEnabled() || reqID == "" || key == "" {
		return
	}
	sess := c.currentSession()
	if sess == nil {
		return
	}

	st := &wecomStream{
		reqID:    reqID,
		streamID: newStreamID(key),
		sess:     sess,
		started:  time.Now(),
	}

	c.streamMu.Lock()
	if old, ok := c.streams[key]; ok && old != nil {
		// 同一会话来了新消息，旧流必须立刻收尾：不收尾它会一直挂在「正在思考…」，
		// 而且两条流共用 key 会让后面那条永远找不到自己。
		//
		// supersedeText 非空时用它收尾（旧轮被打断的场合），
		// 否则原样 finish —— 只把「正在思考…」定格，不留半截答案。
		delete(c.streams, key)
		c.streamMu.Unlock()
		c.closeStream(ctx, old, supersedeTextFor(old, supersedeText))
		c.streamMu.Lock()
	}
	c.streams[key] = st
	c.streamMu.Unlock()

	st.timer = time.AfterFunc(c.placeholderDelay(), func() { c.pushPlaceholder(key, st) })
}

// supersedeTextFor 旧流被新消息顶掉时，用什么内容收尾。
//
// ⚠️ 已经推过正文（fed）时**不能**只留打断提示：B 档之前气泡里只有「正在思考…」，
// 换成提示无所谓；现在用户正在读那半截正文，抹掉它比留着更难接受 ——
// 他正是因为看到内容才决定插话的。所以正文保留、提示**追加**在后面。
// 没推过正文时照旧：只有一句占位的话，「（这条已被新消息打断）」信息量更大。
func supersedeTextFor(old *wecomStream, supersedeText string) string {
	if old == nil || !old.fed || old.content == "" || supersedeText == "" {
		return supersedeText
	}
	return old.content + "\n\n" + supersedeText
}

// pushPlaceholder 延迟到点，推一条占位（finish=false）。
func (c *WeComChannel) pushPlaceholder(key string, st *wecomStream) {
	c.streamMu.Lock()
	// ⚠️ fed 置位说明已经在推真实正文了 —— 这时再推占位会把正文覆盖回
	// 「正在思考…」，用户看到内容倒退，比不推还糟。
	if cur, ok := c.streams[key]; !ok || cur != st || st.closed || st.fed {
		c.streamMu.Unlock()
		return // 已被 finish / 已被更新的流顶掉 / 已停机 / 已在推正文
	}
	c.streamMu.Unlock()

	ctx := c.deliveryCtx(nil)
	if err := st.push(ctx, streamPlaceholder, false); err != nil {
		// 占位推失败不硬撑：摘掉这条流，最终回复会走整段 aibot_send_msg。
		traceid.L(ctx).Warnw("wecom: stream placeholder failed, falling back to plain send",
			"channel", c.name, "stream_id", st.streamID, "err", err)
		c.dropStream(key, st)
		return
	}
	c.streamMu.Lock()
	st.pushed = true
	st.content = streamPlaceholder
	c.streamMu.Unlock()
	traceid.L(ctx).Debugw("wecom: stream placeholder pushed",
		"channel", c.name, "stream_id", st.streamID, "chat", key)
}

// finishStream 用最终内容收尾活跃流。
//
// 返回 true 表示已走流式（调用方直接返回）；false 表示没有可用流或推送失败，
// 调用方应回退到整段 aibot_send_msg —— **绝不能因此丢消息**。
func (c *WeComChannel) finishStream(ctx context.Context, key, text string) bool {
	c.streamMu.Lock()
	st, ok := c.streams[key]
	if ok {
		delete(c.streams, key)
	}
	c.streamMu.Unlock()
	if !ok || st == nil || st.closed {
		return false
	}
	return c.closeStream(ctx, st, text)
}

// closeStream 真正推送 finish=true 并标记关闭。
func (c *WeComChannel) closeStream(ctx context.Context, st *wecomStream, text string) bool {
	c.streamMu.Lock()
	if st.closed {
		c.streamMu.Unlock()
		return false
	}
	st.closed = true
	firstPushOnFinish := !st.pushed
	c.streamMu.Unlock()

	if st.timer != nil {
		st.timer.Stop()
	}
	if text == "" {
		text = st.content
	}
	if text == "" {
		return false // 没有任何内容可收尾，交给调用方整段发送
	}

	// ⚠️ 「首推就是 finish=true」服务端是否接受**未实测**（15 步探针没覆盖到：
	// 正常回复都要十几秒，占位早推过了）。这里不赌 —— 先补一帧 finish=false
	// 把内容推上去，再 finish=true 收尾，两步都落在实测过的路径上。
	// 代价只有「快回复多一帧」，可忽略；换来的是这个分支零不确定性。
	if firstPushOnFinish {
		if err := st.push(ctx, text, false); err != nil {
			traceid.L(ctx).Warnw("wecom: stream first push failed, falling back to plain send",
				"channel", c.name, "stream_id", st.streamID, "err", err)
			return false
		}
		c.streamMu.Lock()
		st.pushed = true
		st.content = text
		c.streamMu.Unlock()
	}

	if err := st.push(ctx, text, true); err != nil {
		traceid.L(ctx).Warnw("wecom: stream finish failed, falling back to plain send",
			"channel", c.name, "stream_id", st.streamID, "err", err)
		return false
	}
	traceid.L(ctx).Infow("wecom: stream finished",
		"channel", c.name, "stream_id", st.streamID, "bytes", len(text),
		// false = 回复太快，占位还没推就出结果了（已补一帧，见上）。
		"placeholder_shown", !firstPushOnFinish)
	return true
}

// pushStreamContent 往一条已登记的流推一次内容（finish=false，原位替换）。
//
// ⚠️ 只在 st 仍是**当前**流时推：本轮若已被新消息打断/顶替，再推就会
// 把新轮次的占位覆盖成上一轮的残稿。
//
// ⚠️ store 与 display 分开：display 是推给用户看的（带「还在写…」之类的尾标），
// store 是存档（干净正文 + 工具活动行）。兜底收尾（reapStreams / abortStreams）
// 用的是 store —— 把「还在写…」当最终内容定格，用户会看到一条**已经结束**的消息
// 写着还在写。
func (c *WeComChannel) pushStreamContent(ctx context.Context, key string, st *wecomStream, store, display string) bool {
	c.streamMu.Lock()
	if cur, ok := c.streams[key]; !ok || cur != st || st.closed {
		c.streamMu.Unlock()
		return false
	}
	c.streamMu.Unlock()

	if err := st.push(ctx, display, false); err != nil {
		traceid.L(ctx).Warnw("wecom: stream push failed",
			"channel", c.name, "stream_id", st.streamID, "err", err)
		return false
	}
	c.streamMu.Lock()
	st.pushed = true
	st.fed = true
	st.content = store
	c.streamMu.Unlock()
	return true
}

// feedStream 订阅本轮的文本增量，边生成边往企微推（B 档）。
//
// 只有「正在思考…」是不够的：几十秒里用户根本不知道 bot 在往哪个方向写，
// 等结果出来才发现跑偏 —— 那时再纠正已经白等一整轮。推增量让他能当场判断、
// 当场发「停止」或补一句引导。
//
// ⚠️ 与占位刷新互斥：推过真实内容后（st.fed）占位就不再刷，否则内容会倒退。
func (c *WeComChannel) feedStream(ctx context.Context, key, traceID string) {
	if c.deltaSrc == nil || traceID == "" || !c.streamEnabled() {
		return
	}
	gap := c.deltaInterval()
	if gap < 0 {
		return // 显式关闭增量推送
	}

	ch, stop := c.deltaSrc(traceID)
	if ch == nil {
		return
	}
	// ⚠️ 这个 defer 是**必须的**，不是洁癖：EventBus 的订阅只在 Unsubscribe /
	// bus.Close 时才关，轮次结束**不会**关。而 stop() 是唯一会关 ch 的东西，
	// 若只在「ch 被关」时返回，两者就互相等待 —— goroutine 与订阅双双永久泄漏。
	// 订阅上限 1000（MaxSubscriptions），按每条入站消息漏一个的速度，
	// 一千条消息后 Subscribe 会开始返回已关闭订阅 → **Web SSE 全线失效**。
	defer stop()

	start := time.Now()

	var buf strings.Builder
	last := time.Time{}
	announced := false
	deltas := 0 // 收到的增量条数
	pushes := 0 // 实际推出去的帧数

	// activity 工具活动行，按发生顺序；正文在下、活动在上（见 compose）。
	//
	// 为什么要单列一块：工具调用恰好落在「模型在想、还没写出正文」的那段空窗里，
	// 那段时间原本一个字都看不到 —— 「🔧 正在调用 X」是这段空窗里唯一的状态信号。
	var activity []string

	// ⚠️ 收尾必须打一条总账：单次推送只在**首次成功**时留痕（否则长任务刷屏），
	// 光看日志根本数不出推了几帧 —— 「增量到底推没推、推得够不够密」就全靠猜。
	// 2026-10-22 真机就栽在这：日志只有一条 bytes=3，一度以为只推了一次。
	defer func() {
		traceid.L(ctx).Infow("wecom: stream delta feed ended",
			"channel", c.name, "chat", key, "trace_id", traceID,
			"deltas", deltas, "pushes", pushes, "bytes", buf.Len(),
			"tools", len(activity), "elapsed", time.Since(start).String())
	}()

	// mine 本轮**自己那条流**，首次 flush 时锁定，之后只往它推。
	//
	// ⚠️ 不能每次 flush 都去取「当前」流：旧轮被 abort 后它的 feedStream 还活着
	// （IsMessageActive 要等 OnMessageDone 才转 false，实测可达数秒），
	// 而新轮的流这时候已经开了 —— 每次取当前流的话，旧轮会把**上轮残稿**
	// 推进新轮的流，把新轮刚推的「正在思考…」直接盖掉。
	var mine *wecomStream
	// detached 本轮的流已被顶掉/推不动了，再循环下去只会污染别人的流。
	detached := false

	// toolLine toolCallID → activity 下标：工具结束时把「正在调用」就地改写成「完成」，
	// 而不是再追加一行（否则气泡里会同时挂着「正在调用」和「完成」，像卡住了）。
	toolLine := map[string]int{}
	running := 0 // 仍在执行的工具数（决定尾标是「还在写」还是「工具执行中」）

	// compose 拼出气泡内容：工具活动在上（引用块），正文在下。
	compose := func(text string) string {
		if len(activity) == 0 {
			return text
		}
		var b strings.Builder
		for i, line := range activity {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("> ")
			b.WriteString(line)
		}
		if text != "" {
			b.WriteString("\n\n")
			b.WriteString(text)
		}
		return b.String()
	}

	flush := func(final bool) {
		text := buf.String()
		if text == "" && len(activity) == 0 {
			return
		}
		if mine == nil {
			mine = c.currentStream(key)
			if mine == nil {
				return // 本轮的流还没登记（极少见），攒着等下一批增量
			}
		}
		store := compose(text)
		display := store
		if !final {
			if running > 0 {
				display += streamToolSuffix
			} else {
				display += streamDraftSuffix
			}
		}
		if !c.pushStreamContent(ctx, key, mine, store, display) {
			// 推不动 = 流已被新轮顶掉，或推送失败。两种都不必再试：
			// 继续循环只会把本轮残稿往**新**流上怼（见 mine 的注释）。
			detached = true
			return
		}
		// ⚠️ 首次成功的日志必须在**置位之前**取快照再判：
		// 写成「flush 完再判 !announced」永远为假，这条日志一次也打不出来。
		first := !announced
		announced = true
		last = time.Now()
		pushes++
		if final {
			traceid.L(ctx).Infow("wecom: stream delta flushed at turn end",
				"channel", c.name, "stream_id", mine.streamID, "bytes", len(text))
		} else if first {
			traceid.L(ctx).Infow("wecom: streaming partial content to user",
				"channel", c.name, "chat", key, "stream_id", mine.streamID,
				"trace_id", traceID, "bytes", len(store), "tools", len(activity))
		} else {
			traceid.L(ctx).Debugw("wecom: stream delta pushed",
				"channel", c.name, "stream_id", mine.streamID, "bytes", len(store))
		}
	}

	// 存活看门狗：轮次一结束就退订（这是唯一可靠的结束信号）。
	// 只靠 ctx 是不够的 —— 投递用的是 runCtx，它活到服务停机为止。
	watch := time.NewTicker(deltaLivenessInterval)
	defer watch.Stop()
	deadline := start.Add(c.streamMaxAge())
	sawActive := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-watch.C:
			// 硬兜底：turnCtl 未注入（单测/裁剪部署）时看门狗永远判不出结束，
			// 只剩 deadline 这一个出口。
			if time.Now().After(deadline) {
				flush(true)
				return
			}
			if c.turnAlive(traceID) {
				sawActive = true
				continue
			}
			// 还没见过「在跑」就别急着退：本轮可能只是还没登记（见 deltaStartGrace）。
			if !sawActive && time.Since(start) < deltaStartGrace {
				continue
			}
			flush(true)
			return
		case d, ok := <-ch:
			if !ok {
				// 增量通道关闭 = 本轮生成结束：把尾巴推一次。最终回复随后还会
				// finish 一次覆盖成完整版 —— 两次都在实测过的路径上。
				flush(true)
				return
			}
			deltas++
			if !sawActive {
				sawActive = c.turnAlive(traceID)
			}
			switch d.Kind {
			case DeltaText:
				buf.WriteString(d.Text)
				// 节流：太频繁会把「原位替换」变成闪烁，也白耗推送额度。
				// 2s 一次，53s 的一轮约 26 次 —— 远低于实测的 300 次/分钟。
				if time.Since(last) < gap {
					continue
				}
			case DeltaToolStart:
				if d.Text == "" || !c.toolActivityEnabled() {
					continue
				}
				running++
				if len(activity) >= maxToolLines {
					continue // 到顶不再追加（结束事件找不到下标，忽略即可）
				}
				toolLine[d.ID] = len(activity)
				activity = append(activity, toolActivityLine(d.Text, false, false))
				// 状态变化**立刻**推，不走节流：工具一进一出往往就几百毫秒，
				// 等 2s 节流的话用户根本看不到它被调用过。
			case DeltaToolDone:
				if d.Text == "" || !c.toolActivityEnabled() {
					continue
				}
				if running > 0 {
					running--
				}
				if idx, ok := toolLine[d.ID]; ok {
					activity[idx] = toolActivityLine(d.Text, true, d.Failed)
					delete(toolLine, d.ID)
				} else if len(activity) < maxToolLines {
					activity = append(activity, toolActivityLine(d.Text, true, d.Failed))
				}
			default:
				continue
			}
			flush(false)
			if detached {
				return
			}
		}
	}
}

// toolActivityLine 生成一行工具活动文案。
//
// ⚠️ 只放工具**名**：参数与返回值里可能有绝对路径、命令行、URL，
// 企微是外部渠道，带上就等于把内部信息写进聊天记录。
func toolActivityLine(name string, done, failed bool) string {
	switch {
	case !done:
		return "🔧 正在调用 " + name
	case failed:
		return "❌ " + name + " 失败"
	default:
		return "✅ " + name + " 完成"
	}
}

// turnAlive 编排侧是否仍在执行这一轮。
//
// 判不出来时（没注入控制器 / botID 为空）返回 true = 不干预，交给 deadline 兜底 ——
// 宁可多等一会儿，也不能把还在生成的轮次提前判死、让增量推送半途断掉。
func (c *WeComChannel) turnAlive(traceID string) bool {
	ctl := c.turnCtl
	if ctl == nil || c.botID == "" || traceID == "" {
		return true
	}
	return ctl.IsMessageActive(c.botID, traceID)
}

// streamTickLoop 周期刷新占位（共享 ticker，随 Start/Stop 走）。
//
// ⚠️ 别按每条流各起一个 ticker：并发流一多，推送数会随流数线性增长，
// 直接打满平台限流（Misskey 上吃过这个亏）。
func (c *WeComChannel) streamTickLoop(ctx context.Context) {
	defer c.wg.Done()
	d := c.placeholderRefresh()
	if d <= 0 {
		return
	}
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshPlaceholders(ctx)
		}
	}
}

// refreshPlaceholders 把「已推过占位、还没收尾」的流刷新一次，末尾带上已用秒数。
//
// 为什么必须刷新：一轮生成实测 17~55s（本次「再来一篇超级长的」跑了 53s），
// 占位推出来后就一动不动，用户根本分不清「还在跑」还是「卡死了」——
// 反馈里那句「之前的思考还是停不下来」就是这个观感。带秒数至少证明它活着。
func (c *WeComChannel) refreshPlaceholders(ctx context.Context) {
	if !c.streamEnabled() {
		return
	}
	now := time.Now()

	c.streamMu.Lock()
	due := make([]*wecomStream, 0, len(c.streams))
	for _, st := range c.streams {
		// 只刷「已经推过占位、且还没开始推正文」的：
		// 还没推占位说明回复很快，刷它多余；已在推正文的刷了会让内容倒退。
		if st == nil || st.closed || !st.pushed || st.fed {
			continue
		}
		due = append(due, st)
	}
	c.streamMu.Unlock()

	for _, st := range due {
		text := fmt.Sprintf("%s %ds", streamPlaceholder, int(now.Sub(st.started).Seconds()))
		// ⚠️ 用 tryPush：刷新是装饰性的，上一条还在等 ACK 就直接跳过，
		// 绝不能阻塞共享 ticker（会把真正的正文推送也一起拖慢）。
		if err := st.tryPush(ctx, text, false); err != nil {
			// 刷新失败不摘流：占位已经在那儿了，最终回复照样能 finish，
			// 摘掉反而会让最终答案退化成整段发送（多一个气泡）。
			if errors.Is(err, errPushBusy) {
				// 不是故障，是并发让路 —— 打成 WARN 会把「真出错」淹掉。
				traceid.L(ctx).Debugw("wecom: stream placeholder refresh skipped, push busy",
					"channel", c.name, "stream_id", st.streamID)
			} else {
				traceid.L(ctx).Warnw("wecom: stream placeholder refresh failed",
					"channel", c.name, "stream_id", st.streamID, "err", err)
			}
			continue
		}
		c.streamMu.Lock()
		st.content = text
		first := st.refreshed == 0
		st.refreshed++
		c.streamMu.Unlock()

		// ⚠️ 刷新成功也得留痕（每流只留第一条）：否则「占位到底刷没刷」
		// 只能靠肉眼盯着客户端看，事后无从核对。
		if first {
			traceid.L(ctx).Infow("wecom: stream placeholder refreshing (turn still alive)",
				"channel", c.name, "stream_id", st.streamID,
				"elapsed", now.Sub(st.started).String())
		} else {
			traceid.L(ctx).Debugw("wecom: stream placeholder refreshed",
				"channel", c.name, "stream_id", st.streamID,
				"elapsed", now.Sub(st.started).String())
		}
	}
}

// dropStream 摘除一条流（占位推送失败时）。
func (c *WeComChannel) dropStream(key string, st *wecomStream) {
	c.streamMu.Lock()
	if cur, ok := c.streams[key]; ok && cur == st {
		delete(c.streams, key)
	}
	st.closed = true
	c.streamMu.Unlock()
	if st.timer != nil {
		st.timer.Stop()
	}
}

// reapStreams 收尾超时未完成的流（cleanupLoop 调用）。
//
// ⚠️ 必须存在：pipeline 完全可能**决定不回复**（静默、被抑制、出错），
// 那时没有 Send 会来收尾，消息会永远停在「正在思考…」直到服务端 10 分钟兜底。
func (c *WeComChannel) reapStreams(ctx context.Context) {
	if !c.streamEnabled() {
		return
	}
	maxAge := c.streamMaxAge()
	now := time.Now()

	c.streamMu.Lock()
	var stale []*wecomStream
	for k, st := range c.streams {
		if st == nil || now.Sub(st.started) < maxAge {
			continue
		}
		delete(c.streams, k)
		stale = append(stale, st)
	}
	c.streamMu.Unlock()

	for _, st := range stale {
		if st.closed {
			continue
		}
		traceid.L(ctx).Warnw("wecom: stream reaped after max age (no reply came)",
			"channel", c.name, "stream_id", st.streamID, "age", now.Sub(st.started).String())
		c.closeStream(ctx, st, st.content)
	}
}

// abortStreams 停机时收尾全部流（Stop 调用）。
//
// ⚠️ 进程没了就再没人能 finish，客户端会一直显示「正在思考…」直到服务端兜底 ——
// 停机前必须主动收尾。
func (c *WeComChannel) abortStreams(ctx context.Context) {
	c.streamMu.Lock()
	all := make([]*wecomStream, 0, len(c.streams))
	for _, st := range c.streams {
		if st != nil {
			all = append(all, st)
		}
	}
	c.streams = make(map[string]*wecomStream)
	c.streamMu.Unlock()

	for _, st := range all {
		if st.closed {
			continue
		}
		_ = c.closeStream(ctx, st, st.content)
	}
}

// newStreamID 生成 stream.id。同一个 id 的所有推送属于同一条流式消息，
// 不同轮次必须换新的（否则会去刷新上一条已 finish 的消息）。
//
// ⚠️ 会话标识不能直接当 id：同会话的第 N 轮若复用上一轮的 id，会去刷新一条
// 已经 finish 的消息（服务端会拒绝）。key 只用来做身份痕迹，唯一性靠时间戳。
func newStreamID(key string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("tb-%s-%d", strconv.FormatUint(uint64(h.Sum32()), 36), time.Now().UnixNano())
}
