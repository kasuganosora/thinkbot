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
	"fmt"
	"hash/fnv"
	"strconv"
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

	// streamPlaceholder 占位文案。
	streamPlaceholder = "正在思考…"
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
}

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
		c.closeStream(ctx, old, supersedeText)
		c.streamMu.Lock()
	}
	c.streams[key] = st
	c.streamMu.Unlock()

	st.timer = time.AfterFunc(c.placeholderDelay(), func() { c.pushPlaceholder(key, st) })
}

// pushPlaceholder 延迟到点，推一条占位（finish=false）。
func (c *WeComChannel) pushPlaceholder(key string, st *wecomStream) {
	c.streamMu.Lock()
	if cur, ok := c.streams[key]; !ok || cur != st || st.closed {
		c.streamMu.Unlock()
		return // 已被 finish / 已被更新的流顶掉 / 已停机
	}
	c.streamMu.Unlock()

	ctx := c.deliveryCtx(nil)
	if err := st.sess.sendStream(ctx, st.reqID, st.streamID, streamPlaceholder, false); err != nil {
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
		if err := st.sess.sendStream(ctx, st.reqID, st.streamID, text, false); err != nil {
			traceid.L(ctx).Warnw("wecom: stream first push failed, falling back to plain send",
				"channel", c.name, "stream_id", st.streamID, "err", err)
			return false
		}
		c.streamMu.Lock()
		st.pushed = true
		st.content = text
		c.streamMu.Unlock()
	}

	if err := st.sess.sendStream(ctx, st.reqID, st.streamID, text, true); err != nil {
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
