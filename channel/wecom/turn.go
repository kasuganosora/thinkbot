package wecom

// 轮次控制：bot 正在输出时，用户又发来消息，这条消息归谁？
//
// 企微没有「回复/引用某条消息」的引用关系，也没有现成的中止按钮，
// 所以只能靠「本轮是否还在执行」这个状态来判定语义。三种动作：
//
//   - turnNew：开新一轮（若旧的还在跑，先打断它）。**默认策略** —— 聊天场景里
//     用户连发两句通常是想另起话题，把两轮内容混在一起比丢掉补充更难受。
//   - turnAppend：注入当前轮继续跑（Claude-CLI 风格）。由 `interruptPolicy=append` 开启。
//   - turnStopped：文本终止指令，只打断、不开新轮。
//
// 能力与 web 端同源：`POST /api/chat/append` 与 `POST /api/chat/abort` 用的就是
// BotService.AppendToMessage / AbortMessage，这里只是给企微接上同一个出口。

import (
	"context"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// TurnController 由 api 层注入的轮次控制能力（照 fileSink 的注入方式）。
//
// 抽成接口而不是直接依赖 *api.BotService：channel 包不该反向依赖 api 包，
// 单测里也能塞一个假的。
type TurnController interface {
	// AbortMessage 取消一条正在执行的消息。false = 未命中（已结束或不存在）。
	AbortMessage(botID, traceID string) bool
	// AppendToMessage 把用户补充注入正在执行的那一轮。false = 本轮已结束。
	AppendToMessage(botID, traceID, text string) bool
	// IsMessageActive 这条 trace 是否仍在执行。
	//
	// ⚠️ 轮次**不能**靠「出站 Send 来过」就摘掉：core.Action 不带 TraceID，
	// 出站侧认不出自己在服务哪一輪；更要命的是 pipeline 完全可能决定不回复
	// （静默 / 被抑制 / 出错），那时根本不会有 Send 回调。
	// 所以「本轮还在不在跑」必须以**编排侧**为准。
	IsMessageActive(botID, traceID string) bool
	// TakeUnconsumedAppends 取回注入成功但**没被编排层消费**的补充。
	//
	// 「AppendToMessage 返回 true」只说明投递进了通道，不说明被取走
	// （编排层只在一步生成结束后取一次）。本轮随即结束时这些补充没人管，
	// 调用方必须兜底成一次新的入站消息。
	TakeUnconsumedAppends(botID, traceID string) []string
}

// SetTurnController 注入轮次控制器（由 api 层在建好 channel 后调用）。
func (c *WeComChannel) SetTurnController(tc TurnController) { c.turnCtl = tc }

// 轮次策略取值（Config.InterruptPolicy）。
const (
	// InterruptRestart bot 忙时新消息 = 打断旧轮 + 开新一轮（默认）。
	InterruptRestart = "restart"
	// InterruptAppend bot 忙时新消息 = 注入当前轮继续跑。
	InterruptAppend = "append"
)

// defaultStopCommands 终止指令词表。
//
// ⚠️ 必须**整句完全匹配**才生效：「停止一下再说」不是终止指令，
// 否则用户一开口说「停」字就被掐断，体验灾难。
var defaultStopCommands = []string{
	"停止", "停下", "停", "别说了", "别讲了", "打住", "取消", "终止", "闭嘴",
	"stop", "cancel", "abort", "halt",
}

// activeTurn 一个正在执行的轮次。
type activeTurn struct {
	traceID string
	started time.Time
}

// turnAction 一条新入站消息的归属判定结果。
type turnAction int

const (
	// turnNew 开新一轮（必要时先打断旧的）。
	turnNew turnAction = iota
	// turnAppend 已注入当前轮，调用方**不要**再投递入站。
	turnAppend
	// turnStopped 已终止当前轮，调用方**不要**再投递入站。
	turnStopped
)

// normalizeCommand 归一化：去空白、去尾部标点、小写。
// 「停止。」「Stop!」「 停止 」应等价。
func normalizeCommand(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, "。.!！?？~～ \t")
	return strings.TrimSpace(s)
}

// isStopCommand 判断一条文本是不是终止指令。
func isStopCommand(text string, extra []string) bool {
	cmd := normalizeCommand(text)
	if cmd == "" {
		return false
	}
	for _, w := range defaultStopCommands {
		if cmd == w {
			return true
		}
	}
	for _, w := range extra {
		if cmd == normalizeCommand(w) {
			return true
		}
	}
	return false
}

// resolveTurn 判定这条消息的归属，并返回一个应绑定给它的 traceID。
//
// 返回的 traceID 必须写进 msg.TraceID：channel 自己生成它，
// 后续才能拿它去 Abort / Append（Ingress.Receive 会保留已设值，不会覆盖）。
//
// ⚠️ 打断/终止都会顺带收尾旧流：pipeline 被 ctx cancel 后不会再有 Send 回调，
// 那条「正在思考…」若没人 finish 会一直挂到服务端 10 分钟兜底。
func (c *WeComChannel) resolveTurn(ctx context.Context, key, text string) (turnAction, string) {
	ctl := c.turnCtl
	c.turnMu.Lock()
	cur := c.active[key]
	c.turnMu.Unlock()

	// 没有在跑的轮次：一切都从简（「停止」也只是句普通话，交给 LLM 理解）。
	// 仍要返回 traceID：调用方会把它写进 msg.TraceID 并登记新轮次。
	if cur == nil || ctl == nil {
		return turnNew, traceid.New()
	}

	// ⚠️ 僵尸轮次：编排侧早已结束（静默 / 出错 / 压根没起），只是活跃表还没摘。
	// 不判这一条的话，「停止」会打一行日志然后什么也没停，
	// 普通消息也会被误当成打断、白白推一条「（这条已被新消息打断）」。
	//
	// ⚠️ 必须留日志：「为什么我发的话没打断上一轮」全靠这一行定位 ——
	// 上一轮其实早就生成完了，只是这条消息到得晚。
	if !ctl.IsMessageActive(c.botID, cur.traceID) {
		traceid.L(ctx).Infow("wecom: stale turn detected, starting a new one",
			"channel", c.name, "chat", key, "trace_id", cur.traceID,
			"age", time.Since(cur.started).String(), "text", strutil.Truncate(text, 40))
		c.clearTurn(key, cur.traceID)
		return turnNew, traceid.New()
	}

	// 1) 文本终止指令：只打断，不开新轮。
	if isStopCommand(text, c.cfg.StopCommands) {
		if ctl.AbortMessage(c.botID, cur.traceID) {
			traceid.L(ctx).Infow("wecom: turn aborted by stop command",
				"channel", c.name, "chat", key, "trace_id", cur.traceID)
			c.clearTurn(key, cur.traceID)
			c.finishStream(ctx, key, "已停止。")
		} else {
			// 本轮其实已经结束了，只是活跃表还没摘 —— 清掉即可，别误报。
			c.clearTurn(key, cur.traceID)
		}
		return turnStopped, ""
	}

	// 2) 默认 append：注入当前轮，让它带着补充继续跑。
	//
	// ⚠️ 返回的是**当前轮**的 traceID，不是新的 —— 调用方要用它去兜底
	// 「注入成功但没被消费」（见 watchAppend）。
	if !strings.EqualFold(c.cfg.InterruptPolicy, InterruptRestart) {
		if ctl.AppendToMessage(c.botID, cur.traceID, text) {
			traceid.L(ctx).Infow("wecom: user supplement injected into running turn",
				"channel", c.name, "chat", key, "trace_id", cur.traceID,
				"text_len", len(text))
			return turnAppend, cur.traceID
		}
		// 本轮已结束 → 退化成新一轮（下面照常走）。
		c.clearTurn(key, cur.traceID)
		return turnNew, traceid.New()
	}

	// 3) restart：打断旧轮，开新一轮。
	//
	// ⚠️ **不要**在这里 finishStream：旧流已由 openStream 用 turnInterruptText
	// 收尾（见 handleMsgCallback）。此时表里的流是**新消息自己那条**，
	// 在这里 finish 会把新轮次的流也吃掉，最终答案只能退化成整段发送。
	if ctl.AbortMessage(c.botID, cur.traceID) {
		traceid.L(ctx).Infow("wecom: previous turn interrupted by new message",
			"channel", c.name, "chat", key, "trace_id", cur.traceID,
			"age", time.Since(cur.started).String(), "text", strutil.Truncate(text, 40))
	}
	c.clearTurn(key, cur.traceID)
	return turnNew, traceid.New()
}

// beginTurn 登记一个新轮次。
//
// ⚠️ turnCtl 为 nil 时不登记：没人能 abort/append 它，登记了只会让后来的消息
// 被判成「打断一个其实打不断的轮次」，白白多走一次 Abort 调用。
func (c *WeComChannel) beginTurn(key, traceID string) {
	if key == "" || traceID == "" || c.turnCtl == nil {
		return
	}
	c.turnMu.Lock()
	if c.active == nil {
		c.active = make(map[string]*activeTurn)
	}
	c.active[key] = &activeTurn{traceID: traceID, started: time.Now()}
	c.turnMu.Unlock()
}

// turnInterruptText 旧轮被新消息打断时，旧流收尾用的文案。
const turnInterruptText = "（这条已被新消息打断）"

// appendWatchInterval 兜底 watcher 的轮询间隔（见 watchAppend）。
const appendWatchInterval = 250 * time.Millisecond

// watchAppendMaxWait 返回兜底 watcher 的等待上限；单测可覆盖缩短。
func (c *WeComChannel) watchAppendMaxWait() time.Duration {
	if c.watchAppendMaxWaitOverride > 0 {
		return c.watchAppendMaxWaitOverride
	}
	return appendWatchMaxWait
}

// appendWatchMaxWait 兜底 watcher 的最长等待时间（见 watchAppend）。
//
// ⚠️ 绝不能用 streamMaxAge（默认 5 分钟）：那是**企微流式气泡**的寿命，与
// 「本轮编排跑多久」毫无关系。实测 2026-10-26：用户在第 4 分钟补充，那一轮
// 跑到第 17 分钟才结束 —— watcher 在第 9 分钟就放弃了，TakeUnconsumedAppends
// 压根没机会执行，补充被静默丢弃。长任务是常态（没有墙钟硬上限），这里给一个
// 远大于任何合理轮次的上限，代价只是一次每 250ms 的 map 查表。
const appendWatchMaxWait = time.Hour

// startsNewTurn 这条消息会不会起一个新轮次（**无副作用**的预判）。
//
// 供入站路径决定要不要为它开流：终止指令要复用旧流（finish 时原位替换成
// 「已停止。」），append 补充压根不该有新气泡，只有 restart 才真的需要新流。
func (c *WeComChannel) startsNewTurn(key, text string) bool {
	if !c.turnRunning(key) {
		return true
	}
	if isStopCommand(text, c.cfg.StopCommands) {
		return false
	}
	return strings.EqualFold(c.cfg.InterruptPolicy, InterruptRestart)
}

// holdWorthy 这条裸文本值不值得进配文合并窗口。
//
// ⚠️ bot 正忙着时**不要**暂存：用户的打断/补充必须立刻生效，
// 再等 3s 合并窗口，用户早就把「停止」发了三遍了。
// ⚠️ 终止指令同理，它应该是即时的。
func (c *WeComChannel) holdWorthy(key, text string) bool {
	if isStopCommand(text, c.cfg.StopCommands) {
		return false
	}
	return !c.turnRunning(key)
}

// turnRunning 该会话是否真有一个**正在执行**的轮次。
//
// ⚠️ 不能只查活跃表：轮次结束没有统一回调点（pipeline 静默时连 Send 都没有），
// 表里留下的僵尸项会让后续消息一律被判成「打断一个已经结束的轮次」。
// 所以最终以编排侧为准（见 TurnController.IsMessageActive 的注释）。
func (c *WeComChannel) turnRunning(key string) bool {
	ctl := c.turnCtl
	if ctl == nil {
		return false
	}
	c.turnMu.Lock()
	cur := c.active[key]
	c.turnMu.Unlock()
	if cur == nil {
		return false
	}
	return ctl.IsMessageActive(c.botID, cur.traceID)
}

// hasActiveTurn 活跃表里是否有该会话的登记项（**不**判断是否还在跑）。
// 只供单测与 reapTurns 之类的内部维护用；业务判定一律走 turnRunning。
func (c *WeComChannel) hasActiveTurn(key string) bool {
	c.turnMu.Lock()
	_, ok := c.active[key]
	c.turnMu.Unlock()
	return ok
}

// clearTurn 摘除轮次（只在 traceID 对得上时摘，避免误摘新轮）。
func (c *WeComChannel) clearTurn(key, traceID string) {
	c.turnMu.Lock()
	if cur, ok := c.active[key]; ok && (traceID == "" || cur.traceID == traceID) {
		delete(c.active, key)
	}
	c.turnMu.Unlock()
}

// watchAppend 兜底「注入成功但没被消费」的补充（append 策略专用）。
//
// ⚠️ AppendToMessage 返回 true 只说明进了通道，编排层**只在一部生成结束后**
// 取一次（drainInterruptMessages）。若那一轮的取用已经跑过、本轮随即结束，
// 这条补充就没人管了 —— 实测窗口约 0.7s（生成完成 → 出站 → 笔记落库 →
// OnMessageDone）。丢了是静默的，用户只会觉得「bot 没理我」。
//
// 判定方式不做猜测：等到本轮**确实结束**后去 TakeUnconsumedAppends，
// 取回来有内容 = 没被消费（通道已被编排层取空则返回空）。
func (c *WeComChannel) watchAppend(ctx context.Context, msg core.Message, traceID string) {
	ctl := c.turnCtl
	if ctl == nil || traceID == "" {
		return
	}
	deadline := time.Now().Add(c.watchAppendMaxWait())
	ended := false
	for time.Now().Before(deadline) {
		if c.isStopped() {
			return // 停机中：不做兜底，避免往正在关闭的 pipeline 里塞消息
		}
		if !ctl.IsMessageActive(c.botID, traceID) {
			ended = true
			break
		}
		time.Sleep(appendWatchInterval)
	}
	// ⚠️ 超时且本轮仍在跑：**绝对不能**取 —— 那些补充马上会被消费，
	// 取走再开一轮就是重复处理（用户会收到两条回复）。
	//
	// 走到这里说明本轮跑了超过 appendWatchMaxWait 还没结束，属于异常长任务：
	// 留一条 WARN，否则「我补充了但 bot 完全没反应」又是一次零线索排查。
	if !ended {
		traceid.L(ctx).Warnw("wecom: append watcher gave up before the turn ended",
			"channel", c.name, "chat", msg.Channel, "trace_id", traceID,
			"waited", appendWatchMaxWait.String())
		return
	}

	left := ctl.TakeUnconsumedAppends(c.botID, traceID)
	if len(left) == 0 {
		return // 已被编排层取走，正常路径
	}
	supplement := strings.Join(left, "\n")
	traceid.L(ctx).Warnw("wecom: supplement was never consumed, falling back to a new turn",
		"channel", c.name, "chat", msg.Channel, "trace_id", traceID,
		"text", strutil.Truncate(supplement, 60))

	msg.Text = supplement
	msg.TraceID = traceid.New()
	ing := c.getIngress()
	if ing == nil {
		return
	}
	c.beginTurn(msg.Channel, msg.TraceID)
	if err := ing.Receive(c.deliveryCtx(ctx), msg); err != nil {
		traceid.L(ctx).Warnw("wecom: append fallback delivery failed",
			"channel", c.name, "trace_id", msg.TraceID, "err", err)
		c.clearTurn(msg.Channel, msg.TraceID)
	}
}

// reapTurns 收尾长时间未摘除的轮次（cleanupLoop 调用）。
//
// 轮次主要靠 Send 成功/失败回调摘除，但 pipeline 静默（决定不回复）时没人摘，
// 留着会让后来的消息一律被判成「打断」。
func (c *WeComChannel) reapTurns(ctx context.Context) {
	maxAge := c.streamMaxAge()
	now := time.Now()

	// ⚠️ 先只收集候选、出了锁再判活：turnRunning 自己要拿 turnMu，
	// 持锁调用会死锁（sync.Mutex 不可重入）。
	type staleTurn struct {
		key     string
		traceID string
		age     time.Duration
	}
	c.turnMu.Lock()
	var stale []staleTurn
	for k, t := range c.active {
		if t == nil || now.Sub(t.started) < maxAge {
			continue
		}
		stale = append(stale, staleTurn{key: k, traceID: t.traceID, age: now.Sub(t.started)})
	}
	c.turnMu.Unlock()

	for _, s := range stale {
		// ⚠️⚠️ **仍在跑的轮次绝不能摘**。摘掉之后同会话的下一条消息查不到
		// 活跃登记，会被判成 turnNew → 起第二个 turn，两个 turn 并发跑在
		// 同一个 workspace 上互相破坏（2026-10-26 15:16 实测：后起的 turn
		// 一句 `rm -rf wecom-doc-mcp && git clone` 就把前一个 turn 刚装好的
		// 目录删了，两边重复劳动、双双变慢）。
		//
		// 光看 age 是不够的：长任务天然会超过 streamMaxAge，但它**没结束**。
		// 是否僵死要以编排侧为准（turnRunning → ctl.IsMessageActive）——
		// 只有编排层确认已经不活跃的登记才是真僵尸。
		if c.turnRunning(s.key) {
			continue
		}
		// traceID 对得上才删，避免摘掉期间刚起的新轮。
		c.clearTurn(s.key, s.traceID)
		traceid.L(ctx).Warnw("wecom: stale turn reaped",
			"channel", c.name, "chat", s.key, "trace_id", s.traceID, "age", s.age.String())
	}
}
