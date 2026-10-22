package wecom

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// fakeTurnCtl 记录调用、可按需返回失败的假控制器。
type fakeTurnCtl struct {
	mu       sync.Mutex
	aborted  []string
	appended []string
	// abortOK=false 模拟「那一轮已经结束了」；appendOK 同理。
	abortOK  bool
	appendOK bool
	// live 当前仍被视为「在执行中」的 trace 集合。
	live map[string]bool
}

func newFakeTurnCtl() *fakeTurnCtl {
	return &fakeTurnCtl{abortOK: true, appendOK: true, live: map[string]bool{}}
}

func (f *fakeTurnCtl) AbortMessage(_, traceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = append(f.aborted, traceID)
	return f.abortOK
}

func (f *fakeTurnCtl) AppendToMessage(_, traceID, text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, traceID+"|"+text)
	return f.appendOK
}

func (f *fakeTurnCtl) IsMessageActive(_, traceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[traceID]
}

// markLive 标记一条 trace 正在执行。
func (f *fakeTurnCtl) markLive(traceID string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if on {
		f.live[traceID] = true
	} else {
		delete(f.live, traceID)
	}
}

func (f *fakeTurnCtl) abortCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.aborted)
}

func (f *fakeTurnCtl) appendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.appended)
}

func newTurnTestChannel(ctl TurnController, policy string) *WeComChannel {
	c := &WeComChannel{cfg: Config{CaptionWindow: 0, InterruptPolicy: policy}}
	c.pend = make(map[string]*pendingCaption)
	c.dedup = make(map[string]time.Time)
	c.streams = make(map[string]*wecomStream)
	c.active = make(map[string]*activeTurn)
	c.turnCtl = ctl
	return c
}

// beginLiveTurn 登记一个「编排侧确实在跑」的轮次。
// 只 beginTurn 不够：turnRunning 还要过 IsMessageActive 这一关。
func beginLiveTurn(c *WeComChannel, ctl *fakeTurnCtl, key, traceID string) {
	c.beginTurn(key, traceID)
	ctl.markLive(traceID, true)
}

func TestIsStopCommand_ExactMatchOnly(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"停止", true},
		{"停止。", true},     // 尾部句号
		{"  停止  ", true},  // 空白
		{"STOP", true},    // 大小写
		{"Cancel!", true}, // 尾部感叹号
		{"停一下再说", false},
		{"停止帮我查一下", false},
		{"请停止输出", false},
		{"", false},
		{"   ", false},
	}
	for _, tc := range cases {
		if got := isStopCommand(tc.text, nil); got != tc.want {
			t.Errorf("isStopCommand(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
	// 自定义追加
	if !isStopCommand("闭嘴吧", []string{"闭嘴吧"}) {
		t.Error("custom stop command should match")
	}
	if isStopCommand("闭嘴吧你", []string{"闭嘴吧"}) {
		t.Error("custom stop command must match the whole string")
	}
}

func TestResolveTurn_NoActiveTurnAlwaysNew(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, "")

	// 没有在跑的轮次时，「停止」也只是句普通话，交给 LLM，不要吃掉它。
	action, tid := c.resolveTurn(context.Background(), "wecom:dm:u1", "停止")
	if action != turnNew {
		t.Fatalf("action = %v, want turnNew", action)
	}
	if !traceid.IsValid(tid) {
		t.Fatalf("traceID = %q, want a valid one", tid)
	}
	if ctl.abortCount() != 0 {
		t.Errorf("abort called %d times, want 0", ctl.abortCount())
	}
}

func TestResolveTurn_StopCommandAbortsAndDoesNotStartNewTurn(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, InterruptAppend) // 即便 append 策略，终止指令也只打断
	beginLiveTurn(c, ctl, "wecom:dm:u1", "T-old")

	action, tid := c.resolveTurn(context.Background(), "wecom:dm:u1", "停止")
	if action != turnStopped {
		t.Fatalf("action = %v, want turnStopped", action)
	}
	if tid != "" {
		t.Errorf("traceID = %q, want empty", tid)
	}
	if ctl.abortCount() != 1 {
		t.Fatalf("abort called %d times, want 1", ctl.abortCount())
	}
	if ctl.appendCount() != 0 {
		t.Errorf("append called %d times, want 0", ctl.appendCount())
	}
	if c.hasActiveTurn("wecom:dm:u1") {
		t.Error("turn should be cleared after stop")
	}
}

func TestResolveTurn_AppendPolicyInjects(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, InterruptAppend)
	beginLiveTurn(c, ctl, "wecom:dm:u1", "T-old")

	action, tid := c.resolveTurn(context.Background(), "wecom:dm:u1", "另外再查下天气")
	if action != turnAppend {
		t.Fatalf("action = %v, want turnAppend", action)
	}
	if tid != "" {
		t.Errorf("traceID = %q, want empty", tid)
	}
	if ctl.abortCount() != 0 {
		t.Errorf("abort called %d times, want 0", ctl.abortCount())
	}
	if ctl.appendCount() != 1 {
		t.Fatalf("append called %d times, want 1", ctl.appendCount())
	}
	// 注入成功后轮次仍在跑，不能摘
	if !c.hasActiveTurn("wecom:dm:u1") {
		t.Error("turn must stay active after append")
	}
}

// TestResolveTurn_AppendFallsBackToNewTurn 本轮其实已结束（Append 返回 false）时，
// 不能把用户的补充吞掉 —— 必须退化成新一轮。
func TestResolveTurn_AppendFallsBackToNewTurn(t *testing.T) {
	ctl := newFakeTurnCtl()
	ctl.appendOK = false
	c := newTurnTestChannel(ctl, InterruptAppend)
	beginLiveTurn(c, ctl, "wecom:dm:u1", "T-old")

	action, tid := c.resolveTurn(context.Background(), "wecom:dm:u1", "再查下天气")
	if action != turnNew {
		t.Fatalf("action = %v, want turnNew", action)
	}
	if !traceid.IsValid(tid) {
		t.Fatalf("traceID = %q, want a valid one", tid)
	}
	if c.hasActiveTurn("wecom:dm:u1") {
		t.Error("stale turn must be cleared when append misses")
	}
}

func TestResolveTurn_RestartIsDefault(t *testing.T) {
	for _, policy := range []string{"", "restart", "unknown-policy"} {
		ctl := newFakeTurnCtl()
		c := newTurnTestChannel(ctl, policy)
		beginLiveTurn(c, ctl, "wecom:dm:u1", "T-old")

		action, tid := c.resolveTurn(context.Background(), "wecom:dm:u1", "换个话题")
		if action != turnNew {
			t.Fatalf("policy %q: action = %v, want turnNew", policy, action)
		}
		if !traceid.IsValid(tid) {
			t.Fatalf("policy %q: traceID = %q", policy, tid)
		}
		if ctl.abortCount() != 1 {
			t.Errorf("policy %q: abort called %d times, want 1", policy, ctl.abortCount())
		}
		// ⚠️ 打断后必须摘掉旧轮：留着会让再下一条消息又去 abort 同一个 traceID。
		if c.hasActiveTurn("wecom:dm:u1") {
			t.Errorf("policy %q: old turn must be cleared", policy)
		}
	}
}

func TestStartsNewTurn_GuardsStreamOpening(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, "")
	// 没有活跃轮次 → 一律开流
	if !c.startsNewTurn("k", "停止") {
		t.Error("no active turn: should open stream")
	}
	beginLiveTurn(c, ctl, "k", "T1")
	// 有活跃轮次 + 终止指令 → 复用旧流（finish 成「已停止。」）
	if c.startsNewTurn("k", "停止") {
		t.Error("stop command with active turn: must NOT open a new stream")
	}
	// 有活跃轮次 + restart → 开新流
	if !c.startsNewTurn("k", "换个话题") {
		t.Error("restart: should open stream")
	}

	c2 := newTurnTestChannel(ctl, InterruptAppend)
	beginLiveTurn(c2, ctl, "k", "T1")
	if c2.startsNewTurn("k", "补充一句") {
		t.Error("append policy: must NOT open a new stream")
	}
}

func TestHoldWorthy_BusyAndStopSkipCaptionWindow(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, "")
	if !c.holdWorthy("k", "看看这个") {
		t.Error("idle: plain text should enter the caption window")
	}
	if c.holdWorthy("k", "停止") {
		t.Error("stop command must not be delayed by the caption window")
	}
	beginLiveTurn(c, ctl, "k", "T1")
	if c.holdWorthy("k", "看看这个") {
		t.Error("busy: interrupt must take effect immediately, not 3s later")
	}
}

// TestReapTurns 长时间没摘的轮次必须被回收，否则后续消息一律被判成「打断」。
func TestReapTurns(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, "")
	beginLiveTurn(c, ctl, "k", "T1")
	c.turnMu.Lock()
	c.active["k"].started = c.active["k"].started.Add(-2 * defaultStreamMaxAge)
	c.turnMu.Unlock()
	c.reapTurns(context.Background())
	if c.hasActiveTurn("k") {
		t.Error("stale turn should have been reaped")
	}
}

// TestResolveTurn_ZombieTurn 编排侧已结束（pipeline 静默，压根不会有 Send）
// 但活跃表还没摘：必须退化成新一轮 —— 既不能把用户的「停止」吃掉，
// 也不能对着一个已经结束的轮次白推一条「（这条已被新消息打断）」。
func TestResolveTurn_ZombieTurn(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, "")
	c.beginTurn("wecom:dm:u1", "T-old") // 刻意不 markLive：编排侧已经结束了

	for _, text := range []string{"停止", "换个话题"} {
		action, tid := c.resolveTurn(context.Background(), "wecom:dm:u1", text)
		if action != turnNew {
			t.Fatalf("text %q: action = %v, want turnNew", text, action)
		}
		if !traceid.IsValid(tid) {
			t.Fatalf("text %q: traceID = %q", text, tid)
		}
	}
	if ctl.abortCount() != 0 {
		t.Errorf("zombie turn must not be aborted, got %d calls", ctl.abortCount())
	}
	if c.hasActiveTurn("wecom:dm:u1") {
		t.Error("zombie turn must be cleared")
	}
}

// TestTurnRunning_FollowsOrchestration 「还在跑」必须以编排侧为准，
// 不能只看自己这张活跃表（表里会有僵尸项）。
func TestTurnRunning_FollowsOrchestration(t *testing.T) {
	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, "")
	c.beginTurn("k", "T1")
	if c.turnRunning("k") {
		t.Error("registered but not live: turnRunning must be false")
	}
	ctl.markLive("T1", true)
	if !c.turnRunning("k") {
		t.Error("live turn: turnRunning must be true")
	}
	ctl.markLive("T1", false)
	if c.turnRunning("k") {
		t.Error("turn finished: turnRunning must be false")
	}
}

// TestBeginTurn_RequiresController 没有控制器时不要登记轮次：
// 登记了只会让后来的消息去打断一个「其实打不断的轮次」。
func TestBeginTurn_RequiresController(t *testing.T) {
	c := newTurnTestChannel(nil, "")
	c.beginTurn("k", "T1")
	if c.hasActiveTurn("k") {
		t.Error("must not register a turn without a controller")
	}
}
