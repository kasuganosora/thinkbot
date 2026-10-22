package wecom

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pushRec 记录推送出口收到的每一帧。
type pushRec struct {
	mu  sync.Mutex
	rec []streamPush
}

type streamPush struct {
	content string
	finish  bool
}

func (p *pushRec) add(content string, finish bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rec = append(p.rec, streamPush{content: content, finish: finish})
}

func (p *pushRec) all() []streamPush {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]streamPush, len(p.rec))
	copy(out, p.rec)
	return out
}

func (p *pushRec) last() streamPush {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.rec) == 0 {
		return streamPush{}
	}
	return p.rec[len(p.rec)-1]
}

// newStreamTestChannel 造一个只测流式逻辑的 channel（不碰 WS 连接）。
func newStreamTestChannel() *WeComChannel {
	// StreamToolActivity 取生产默认（开），与 createWecomChannel 的缺省一致。
	c := &WeComChannel{cfg: Config{StreamReply: true, StreamToolActivity: true}}
	c.streams = make(map[string]*wecomStream)
	return c
}

// newFakeStream 造一条推送出口被打桩的流。
func newFakeStream(rec *pushRec) *wecomStream {
	st := &wecomStream{reqID: "r1", streamID: "s1", started: time.Now()}
	st.fakePush = func(content string, finish bool) error {
		rec.add(content, finish)
		return nil
	}
	return st
}

// TestFeedStream_PushesPartialContent B 档的核心：生成过程中就把内容推给用户，
// 让他能当场判断方向对不对、当场引导 —— 而不是等几十秒后才发现跑偏。
func TestFeedStream_PushesPartialContent(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec)
	c.streams["k"] = st

	ch := make(chan Delta, 8)
	c.deltaSrc = func(string) (<-chan Delta, func()) { return ch, func() {} }
	c.cfg.StreamDeltaInterval = 1 // 1ns：每个增量都推，去掉节流抖动

	ch <- Delta{Kind: DeltaText, Text: "从前有"}
	ch <- Delta{Kind: DeltaText, Text: "一座山"}
	close(ch)

	c.feedStream(context.Background(), "k", "T1")

	pushes := rec.all()
	if len(pushes) == 0 {
		t.Fatal("expected partial content to be pushed, got none")
	}
	if got := st.content; got != "从前有一座山" {
		t.Errorf("stream content = %q, want %q", got, "从前有一座山")
	}
	if !st.fed {
		t.Error("fed must be set once real content is pushed")
	}
	// 增量推送一律 finish=false：收尾由最终回复的 finish=true 负责。
	for _, p := range pushes {
		if p.finish {
			t.Errorf("delta push must not finish the stream, got finish=true (%q)", p.content)
		}
	}
}

// TestFeedStream_DisabledWhenIntervalNegative 显式关闭增量推送（<0）时不应有任何推送。
func TestFeedStream_DisabledWhenIntervalNegative(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	c.streams["k"] = newFakeStream(rec)
	c.cfg.StreamDeltaInterval = -1
	ch := make(chan Delta, 4)
	ch <- Delta{Kind: DeltaText, Text: "x"}
	close(ch)
	c.deltaSrc = func(string) (<-chan Delta, func()) { return ch, func() {} }

	c.feedStream(context.Background(), "k", "T1")
	if n := len(rec.all()); n != 0 {
		t.Errorf("delta interval < 0 must disable partial pushes, got %d", n)
	}
}

// TestFeedStream_NoDeltaSource 没注入增量源时静默退化为「只推占位 + 最终替换」。
func TestFeedStream_NoDeltaSource(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	c.streams["k"] = newFakeStream(rec)
	c.feedStream(context.Background(), "k", "T1") // deltaSrc == nil
	if n := len(rec.all()); n != 0 {
		t.Errorf("no delta source must degrade silently, got %d pushes", n)
	}
}

// TestPlaceholder_SkippedOnceContentFed 已经推过正文后再推/刷占位，
// 会把正文覆盖回「正在思考…」—— 用户看到内容倒退，比不推还糟。
func TestPlaceholder_SkippedOnceContentFed(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec)
	st.pushed = true
	st.fed = true
	c.streams["k"] = st

	c.pushPlaceholder("k", st)
	if n := len(rec.all()); n != 0 {
		t.Errorf("placeholder must not be pushed after real content, got %d", n)
	}

	c.refreshPlaceholders(context.Background())
	if n := len(rec.all()); n != 0 {
		t.Errorf("placeholder refresh must skip fed streams, got %d", n)
	}
}

// TestRefreshPlaceholders_StillRefreshesIdleStreams 没推过正文的流仍要刷新
// （秒数跳动 = 证明还在跑），而且要能**连续**刷新。
//
// ⚠️ 这里刻意断言 fed 保持 false：刷新推的是「正在思考… Ns」占位，不是正文。
// 若刷新也置 fed，refreshPlaceholders 下一轮就把自己当「已推正文」跳过 ——
// 刷新只生效一次，整条「证明还活着」的设计当场失效（真写错过一次，靠这条兜住）。
func TestRefreshPlaceholders_StillRefreshesIdleStreams(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec)
	st.pushed = true
	st.started = time.Now().Add(-12 * time.Second)
	c.streams["k"] = st

	c.refreshPlaceholders(context.Background())
	pushes := rec.all()
	if len(pushes) != 1 {
		t.Fatalf("want 1 refresh push, got %d", len(pushes))
	}
	if !strings.Contains(pushes[0].content, "12s") {
		t.Errorf("refresh must carry elapsed seconds, got %q", pushes[0].content)
	}
	if st.fed {
		t.Error("placeholder refresh must NOT set fed, otherwise it stops refreshing itself")
	}

	// 再刷一次：秒数必须继续走，且仍然刷新得动。
	st.started = time.Now().Add(-17 * time.Second)
	c.refreshPlaceholders(context.Background())
	pushes = rec.all()
	if len(pushes) != 2 {
		t.Fatalf("want a 2nd refresh push, got %d", len(pushes))
	}
	if !strings.Contains(pushes[1].content, "17s") {
		t.Errorf("2nd refresh must carry the new elapsed seconds, got %q", pushes[1].content)
	}
	if st.fed {
		t.Error("fed must still be false after repeated placeholder refreshes")
	}
}

// TestPlaceholderRefreshStopsAfterRealContent 正文开始推之后必须停止刷新，
// 否则「正在思考… Ns」会把用户正在看的正文覆盖掉。
func TestPlaceholderRefreshStopsAfterRealContent(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec)
	st.pushed = true
	st.started = time.Now().Add(-12 * time.Second)
	c.streams["k"] = st

	// 先刷一次占位，再推一段真实增量。
	c.refreshPlaceholders(context.Background())
	if !c.pushStreamContent(context.Background(), "k", st, "半截正文", "半截正文"+streamDraftSuffix) {
		t.Fatal("pushStreamContent should succeed")
	}
	// 「还在写…」只是推给用户的尾标，存档必须是干净正文 ——
	// 兜底收尾（reap/停机）用的就是 st.content，定格成「还在写」是自相矛盾的。
	if st.content != "半截正文" {
		t.Errorf("st.content = %q, want clean text without the draft suffix", st.content)
	}
	before := len(rec.all())

	st.started = time.Now().Add(-20 * time.Second)
	c.refreshPlaceholders(context.Background())
	if got := len(rec.all()); got != before {
		t.Errorf("refresh must not push after real content: %d -> %d", before, got)
	}
	if !st.fed {
		t.Error("real content push must set fed")
	}
}

// TestCloseStream_PrePushesWhenNeverPushed 「首推就是 finish=true」服务端是否
// 接受未实测，所以代码要先补一帧 finish=false 再收尾。
func TestCloseStream_PrePushesWhenNeverPushed(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec) // pushed=false

	if ok := c.closeStream(context.Background(), st, "答案"); !ok {
		t.Fatal("closeStream should succeed")
	}
	pushes := rec.all()
	if len(pushes) != 2 {
		t.Fatalf("want 2 pushes (pre-push + finish), got %d", len(pushes))
	}
	if pushes[0].finish || pushes[0].content != "答案" {
		t.Errorf("first push = %+v, want {答案 false}", pushes[0])
	}
	if !pushes[1].finish || pushes[1].content != "答案" {
		t.Errorf("second push = %+v, want {答案 true}", pushes[1])
	}
}

// TestCloseStream_NoPrePushWhenAlreadyPushed 已经推过占位就不用多补一帧。
func TestCloseStream_NoPrePushWhenAlreadyPushed(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec)
	st.pushed = true

	c.closeStream(context.Background(), st, "答案")
	pushes := rec.all()
	if len(pushes) != 1 || !pushes[0].finish {
		t.Fatalf("want exactly 1 finish push, got %+v", pushes)
	}
}

// TestFeedStream_UnsubscribesWhenTurnEnds 防订阅泄漏的回归测试。
//
// ⚠️ EventBus 的订阅**不会**在轮次结束时关闭，只有 Unsubscribe 才会关掉通道。
// 而 feedStream 是阻塞在 <-ch 上的 —— 若没有独立的存活看门狗，它会一直等一个
// 永远不会来的「通道关闭」，goroutine 与订阅双双永久泄漏。
// 订阅上限 1000，按每条消息漏一个的速度，一千条后 Web SSE 直接全线失效。
func TestFeedStream_UnsubscribesWhenTurnEnds(t *testing.T) {
	oldI, oldG := deltaLivenessInterval, deltaStartGrace
	deltaLivenessInterval, deltaStartGrace = 5*time.Millisecond, 10*time.Millisecond
	defer func() { deltaLivenessInterval, deltaStartGrace = oldI, oldG }()

	c := newStreamTestChannel()
	c.botID = "b1"
	c.streams["k"] = newFakeStream(&pushRec{})
	ctl := newFakeTurnCtl()
	ctl.markLive("T1", false) // 编排侧：这一轮没在跑
	c.SetTurnController(ctl)

	// 通道**永不关闭** —— 正是真实 EventBus 的行为。
	ch := make(chan Delta, 1)
	var stopped atomic.Bool
	c.deltaSrc = func(string) (<-chan Delta, func()) {
		return ch, func() { stopped.Store(true) }
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.feedStream(context.Background(), "k", "T1")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("feedStream never returned: the EventBus subscription would leak forever")
	}
	if !stopped.Load() {
		t.Fatal("feedStream returned without calling stop() -> subscription leak")
	}
}

// TestFeedStream_KeepsGoingWhileTurnAlive 轮次还在跑时看门狗不能把它误判成结束。
//
// 这是 deltaStartGrace 存在的理由：feedStream 起在 Receive 之前，
// 那一瞬间 IsMessageActive 必然 false，没有宽限期就会秒退。
func TestFeedStream_KeepsGoingWhileTurnAlive(t *testing.T) {
	oldI := deltaLivenessInterval
	deltaLivenessInterval = 5 * time.Millisecond
	defer func() { deltaLivenessInterval = oldI }()

	c := newStreamTestChannel()
	c.botID = "b1"
	c.streams["k"] = newFakeStream(&pushRec{})
	ctl := newFakeTurnCtl()
	ctl.markLive("T1", true)
	c.SetTurnController(ctl)

	ch := make(chan Delta, 4)
	c.deltaSrc = func(string) (<-chan Delta, func()) { return ch, func() {} }
	c.cfg.StreamDeltaInterval = 1

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.feedStream(context.Background(), "k", "T1")
	}()

	// 远超 deltaStartGrace：若看门狗误判，这时早就退了。
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("feedStream exited while the turn is still alive")
	default:
	}

	// 增量照旧要能推出来。
	ch <- Delta{Kind: DeltaText, Text: "半截正文"}
	time.Sleep(100 * time.Millisecond)
	st := c.currentStream("k")
	if st == nil || !st.fed {
		t.Fatal("deltas must still be pushed while the turn is alive")
	}

	// 轮次结束 → 必须退出。
	ctl.markLive("T1", false)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("feedStream did not exit after the turn ended")
	}
}

// TestFeedStream_DoesNotClobberNewerStream 上一轮的 feedStream 不能污染新轮次的流。
//
// ⚠️ 这是 restart 策略下的真 bug：旧轮被 abort 后它的 feedStream 还活着
// （IsMessageActive 要等 OnMessageDone 才转 false，可达数秒），而新轮的流已经开了。
// 若 flush 每次都取「当前」流，旧轮就会把**上轮残稿**推进新轮的流，
// 把新轮刚推的「正在思考…」直接盖掉。修法：首次 flush 锁定自己那条流，之后只往它推。
func TestFeedStream_DoesNotClobberNewerStream(t *testing.T) {
	c := newStreamTestChannel()
	oldRec := &pushRec{}
	c.streams["k"] = newFakeStream(oldRec)

	ch := make(chan Delta, 8)
	c.deltaSrc = func(string) (<-chan Delta, func()) { return ch, func() {} }
	c.cfg.StreamDeltaInterval = 1

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.feedStream(context.Background(), "k", "T1")
	}()

	ch <- Delta{Kind: DeltaText, Text: "上一轮的残稿"}
	waitFor(t, func() bool { return len(oldRec.all()) >= 1 }, "first delta push")

	// 新消息到达：旧流被收尾，新轮开了一条自己的流。
	newRec := &pushRec{}
	c.streamMu.Lock()
	c.streams["k"] = newFakeStream(newRec)
	c.streamMu.Unlock()

	ch <- Delta{Kind: DeltaText, Text: "再来一段"}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("feedStream must bail out once its own stream is superseded")
	}
	if got := newRec.all(); len(got) != 0 {
		t.Errorf("stale feedStream clobbered the new turn's stream: %+v", got)
	}
	if !strings.Contains(oldRec.last().content, "上一轮的残稿") {
		t.Errorf("old turn content = %q", oldRec.last().content)
	}
}

// TestSupersedeTextFor_KeepsDraftContent 旧流被顶掉时，已经推给用户看的正文必须留住。
//
// ⚠️ B 档之前气泡里只有「正在思考…」，换成打断提示无所谓；现在用户正在读半截正文，
// 抹掉它等于把他刚看到的成果扔了 —— 他正是因为看到内容才决定插话的。
func TestSupersedeTextFor_KeepsDraftContent(t *testing.T) {
	draft := &wecomStream{fed: true, content: "半截正文"}
	if got := supersedeTextFor(draft, turnInterruptText); got != "半截正文\n\n"+turnInterruptText {
		t.Errorf("supersede text = %q, want draft + note", got)
	}
	// 只推过占位：提示信息量更大，照旧替换。
	idle := &wecomStream{fed: false, content: streamPlaceholder}
	if got := supersedeTextFor(idle, turnInterruptText); got != turnInterruptText {
		t.Errorf("supersede text = %q, want plain note", got)
	}
	// 无提示文案（普通顶替）：交给 closeStream 用 st.content 原样定格。
	if got := supersedeTextFor(draft, ""); got != "" {
		t.Errorf("supersede text = %q, want empty (let closeStream freeze content)", got)
	}
}

// TestFeedStream_ToolActivityShown 工具调用必须显示出来，且「正在调用」要被
// 「完成」**就地改写**，不能两行并存（并存看起来像卡住了）。
//
// 为什么值得做：模型「在想、还没写出正文」的那段空窗里，工具调用是唯一的状态信号。
func TestFeedStream_ToolActivityShown(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec)
	c.streams["k"] = st

	ch := make(chan Delta, 8)
	c.deltaSrc = func(string) (<-chan Delta, func()) { return ch, func() {} }
	c.cfg.StreamDeltaInterval = time.Hour // 正文走节流，只有工具事件会触发推送

	ch <- Delta{Kind: DeltaToolStart, ID: "c1", Text: "sandbox_read_file"}
	ch <- Delta{Kind: DeltaToolDone, ID: "c1", Text: "sandbox_read_file"}
	ch <- Delta{Kind: DeltaText, Text: "正文"}
	close(ch)

	c.feedStream(context.Background(), "k", "T1")

	pushes := rec.all()
	if len(pushes) == 0 {
		t.Fatal("tool activity must be pushed")
	}
	// 「正在调用」那一帧：工具还没结束，尾标应该是「工具执行中」而不是「还在写」。
	if !strings.Contains(pushes[0].content, "🔧 正在调用 sandbox_read_file") {
		t.Errorf("first push = %q, want running tool line", pushes[0].content)
	}
	if !strings.Contains(pushes[0].content, streamToolSuffix) {
		t.Errorf("running tool must use the tool suffix, got %q", pushes[0].content)
	}
	last := pushes[len(pushes)-1].content
	if !strings.Contains(last, "✅ sandbox_read_file 完成") {
		t.Errorf("last push = %q, want completed tool line", last)
	}
	if strings.Contains(last, "🔧 正在调用") {
		t.Errorf("the running line must be rewritten in place, got %q", last)
	}
	if !strings.Contains(last, "正文") {
		t.Errorf("text must be composed under the activity block, got %q", last)
	}
}

// TestFeedStream_ToolActivityDisabled 关掉开关后一条工具活动都不该出现。
func TestFeedStream_ToolActivityDisabled(t *testing.T) {
	c := newStreamTestChannel()
	c.cfg.StreamToolActivity = false
	rec := &pushRec{}
	c.streams["k"] = newFakeStream(rec)

	ch := make(chan Delta, 4)
	c.deltaSrc = func(string) (<-chan Delta, func()) { return ch, func() {} }
	c.cfg.StreamDeltaInterval = 1

	ch <- Delta{Kind: DeltaToolStart, ID: "c1", Text: "sandbox_read_file"}
	ch <- Delta{Kind: DeltaToolDone, ID: "c1", Text: "sandbox_read_file"}
	close(ch)

	c.feedStream(context.Background(), "k", "T1")
	for _, p := range rec.all() {
		if strings.Contains(p.content, "sandbox_read_file") {
			t.Errorf("tool activity must be hidden when disabled, got %q", p.content)
		}
	}
}

// TestToolActivityLine 工具名之外的东西一律不出现在气泡里。
func TestToolActivityLine(t *testing.T) {
	cases := []struct {
		name    string
		done    bool
		failed  bool
		want    string
		notWant []string
	}{
		{"sandbox_read_file", false, false, "🔧 正在调用 sandbox_read_file", nil},
		{"sandbox_read_file", true, false, "✅ sandbox_read_file 完成", nil},
		{"sandbox_exec", true, true, "❌ sandbox_exec 失败", nil},
	}
	for _, tc := range cases {
		got := toolActivityLine(tc.name, tc.done, tc.failed)
		if got != tc.want {
			t.Errorf("toolActivityLine(%q,%v,%v) = %q, want %q", tc.name, tc.done, tc.failed, got, tc.want)
		}
	}
}

// TestPush_SerializesSameStream 同一条流的推送必须串行。
//
// ⚠️ conn.respond 对同一 req_id 只允许一个在途请求，撞上就报
// 「already in flight」直接丢帧（2026-10-22 14:55:08 真机丢过一次）。
// 丢掉收尾帧会让 Send 回退成整段发送 —— 用户多收到一个气泡。
func TestPush_SerializesSameStream(t *testing.T) {
	rec := &pushRec{}
	st := newFakeStream(rec)

	// ⚠️ 必须模拟 conn.respond 的真实行为：同一 req_id 只允许一个在途，
	// 并发进来就报「already in flight」丢帧。
	// 少了这段模拟，去掉 pushMu 这个测试也照样全绿 —— 测试就成了摆设。
	var inflight atomic.Int32
	st.fakePush = func(content string, finish bool) error {
		if inflight.Add(1) != 1 {
			inflight.Add(-1)
			return errors.New("wecom: respond req_id X already in flight")
		}
		time.Sleep(80 * time.Millisecond) // 模拟等 ACK
		inflight.Add(-1)
		rec.add(content, finish)
		return nil
	}

	var wg sync.WaitGroup
	var lost atomic.Int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.push(context.Background(), "帧", false); err != nil {
				lost.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := rec.all(); len(got) != 5 {
		t.Errorf("all 5 pushes must go through, got %d (lost=%d)", len(got), lost.Load())
	}
	if lost.Load() != 0 {
		t.Errorf("%d pushes were dropped as 'already in flight'", lost.Load())
	}
}

// TestTryPush_YieldsWhenBusy 占位刷新用 tryPush：上一条还在等 ACK 就**放弃**，
// 而不是阻塞 —— 否则会拖住共享 ticker，连正文推送一起慢下来。
func TestTryPush_YieldsWhenBusy(t *testing.T) {
	rec := &pushRec{}
	st := newFakeStream(rec)
	release := make(chan struct{})
	st.fakePush = func(content string, finish bool) error {
		<-release // 卡住，模拟 ACK 迟迟不来
		rec.add(content, finish)
		return nil
	}

	go func() { _ = st.push(context.Background(), "正文", false) }()
	time.Sleep(30 * time.Millisecond) // 等它拿到锁

	if err := st.tryPush(context.Background(), "正在思考… 9s", false); !errors.Is(err, errPushBusy) {
		t.Fatalf("tryPush must yield while busy, got %v", err)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if got := len(rec.all()); got != 1 {
		t.Errorf("only the content frame must be pushed, got %d", got)
	}
}

// TestRefreshPlaceholders_SkipsWhenBusy 刷新撞上在途推送时要静默跳过，
// 不能打成 WARN —— 那是并发让路，不是故障，打 WARN 会把真正的错误淹掉。
func TestRefreshPlaceholders_SkipsWhenBusy(t *testing.T) {
	c := newStreamTestChannel()
	rec := &pushRec{}
	st := newFakeStream(rec)
	st.pushed = true
	st.started = time.Now().Add(-12 * time.Second)
	c.streams["k"] = st

	release := make(chan struct{})
	st.fakePush = func(content string, finish bool) error {
		<-release
		rec.add(content, finish)
		return nil
	}

	go func() { _ = st.push(context.Background(), "正文", false) }()
	time.Sleep(30 * time.Millisecond)

	c.refreshPlaceholders(context.Background()) // 必须立刻返回，不能卡在锁上
	close(release)
	time.Sleep(50 * time.Millisecond)
	if got := len(rec.all()); got != 1 {
		t.Errorf("busy refresh must be skipped, got %d pushes", got)
	}
}

// waitFor 轮询等待条件成立（避免测试里写死 sleep）。
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestDeltaInterval_DefaultsAndOverride(t *testing.T) {
	c := &WeComChannel{}
	if got := c.deltaInterval(); got != defaultDeltaInterval {
		t.Errorf("default delta interval = %v, want %v", got, defaultDeltaInterval)
	}
	c.cfg.StreamDeltaInterval = 500 * time.Millisecond
	if got := c.deltaInterval(); got != 500*time.Millisecond {
		t.Errorf("override = %v", got)
	}
}
