package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// fakeRunner 记录编排调用，替代真实 Engine。
// llmText 会被注入到返回 env 的 llm.result.Text，模拟 LLM 输出的 JSON 决策。
type fakeRunner struct {
	calls   int
	actions []core.Action
	err     error
	llmText string
	lastEnv *core.Envelope
}

func (f *fakeRunner) ProcessSync(_ context.Context, env *core.Envelope) (*core.Envelope, []core.Action, error) {
	f.calls++
	f.lastEnv = env
	env.Set("llm.result", &llm.GenerateResult{Text: f.llmText})
	return env, f.actions, f.err
}

// newTestExecutor 创建测试用 Executor；opts 可用于注入 channelLister / channelPoster。
func newTestExecutor(t *testing.T, cfg Config, runner TriggerRunner, admission AdmissionFn, opts ...func(*Executor)) (*Executor, *Store) {
	t.Helper()
	store := NewStore(t.TempDir())
	if err := store.SaveConfig("bot-1", &cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	e := NewExecutor(ExecutorConfig{
		Runner:      runner,
		BotID:       "bot-1",
		Store:       store,
		Logger:      zap.NewNop().Sugar(),
		AdmissionFn: admission,
	})
	for _, o := range opts {
		o(e)
	}
	return e, store
}

// withSingleTarget 注入一个可发目标（Misskey 时间线）与一个记录发帖内容的 poster。
func withSingleTarget(captured *string) func(*Executor) {
	return func(e *Executor) {
		e.channelLister = func(_ context.Context) ([]ChannelTarget, error) {
			return []ChannelTarget{{Channel: "Misskey", Type: "misskey", Label: "Misskey 时间线（发新帖）"}}, nil
		}
		e.channelPoster = func(_ context.Context, _ ChannelTarget, content string) error {
			*captured = content
			return nil
		}
	}
}

func lastLog(t *testing.T, store *Store) Log {
	t.Helper()
	ls, err := store.LoadLogs("bot-1")
	if err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(ls.Logs) == 0 {
		t.Fatal("expected at least one heartbeat log")
	}
	return ls.Logs[0] // 头部插入，最新在前
}

// 心跳消息契约：Text 必须为空（否则会被 note_capture 当用户原文写入 L0 记忆），
// 唤醒提示必须走 InjectContext 通道；且必须进入决策模式（KVHeartbeatMode + 恒 KVSuppressReply）。
func TestExecute_HeartbeatMessageContract(t *testing.T) {
	runner := &fakeRunner{llmText: `{"decision":"silent"}`}
	e, _ := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner, nil)

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("expected runner called once, got %d", runner.calls)
	}
	msg := runner.lastEnv.Message
	if msg.Text != "" {
		t.Errorf("heartbeat Text must stay empty to avoid L0 pollution, got %q", msg.Text)
	}
	if msg.InjectContext == "" {
		t.Error("heartbeat wake prompt must be carried by InjectContext")
	}
	// 决策模式：KVHeartbeatMode 必须置位（驱动 LLMStage 强制 JSON 输出）。
	if v, ok := runner.lastEnv.Get(core.KVHeartbeatMode); !ok || v != true {
		t.Errorf("KVHeartbeatMode not set, got %v (ok=%v)", v, ok)
	}
	// 恒设 KVSuppressReply：心跳决策的真实发帖由 Executor 手动路由，不走伪频道 dispatcher。
	if v, ok := runner.lastEnv.Get(core.KVSuppressReply); !ok || v != true {
		t.Errorf("KVSuppressReply must always be set for heartbeat, got %v (ok=%v)", v, ok)
	}
	// 可发目标列表已注入（供 LLM 决策选择）。
	if _, ok := runner.lastEnv.Get(core.KVHeartbeatTargets); !ok {
		t.Error("KVHeartbeatTargets not injected")
	}
	if msg.Source != core.SourceHeartbeat {
		t.Errorf("Source = %q, want %q", msg.Source, core.SourceHeartbeat)
	}
	if msg.UserID != "system:heartbeat" {
		t.Errorf("UserID = %q, want system:heartbeat", msg.UserID)
	}
	if msg.TraceID == "" {
		t.Error("heartbeat must carry a TraceID for log correlation")
	}
}

// 第一级闸门：allow_post=false → 平台压制；但 LLM 决策 post 也只会记录为 suppressed，不误发。
func TestExecute_PlatformGateSuppressesReply(t *testing.T) {
	runner := &fakeRunner{llmText: `{"decision":"post","channel":"Misskey","conversation_id":"","content":"提醒带伞"}`}
	e, store := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: false}, runner, nil,
		withSingleTarget(new(string)))

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("pipeline must still run when posting is suppressed, calls=%d", runner.calls)
	}
	if v, ok := runner.lastEnv.Get(core.KVSuppressReply); !ok || v != true {
		t.Errorf("KVSuppressReply not set, got %v (ok=%v)", v, ok)
	}
	if reason, _ := runner.lastEnv.Get(core.KVSuppressReplyReason); reason != "platform_policy" {
		t.Errorf("suppress reason = %v, want platform_policy", reason)
	}
	entry := lastLog(t, store)
	if entry.Status != StatusSuppressed {
		t.Errorf("status = %q, want %q", entry.Status, StatusSuppressed)
	}
	if entry.Decision != DecisionPost {
		t.Errorf("decision = %q, want %q (LLM 想发但被平台拦)", entry.Decision, DecisionPost)
	}
}

// 第二级闸门：平台允许 + 发帖器就绪 → 按 LLM 决策真实发帖，status=acted。
func TestExecute_BotActsWhenAllowed(t *testing.T) {
	var posted string
	runner := &fakeRunner{llmText: `{"decision":"post","channel":"Misskey","conversation_id":"","content":"今天天气不错"}`}
	e, store := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner, nil,
		withSingleTarget(&posted))

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if v, ok := runner.lastEnv.Get(core.KVHeartbeatMode); !ok || v != true {
		t.Error("KVHeartbeatMode must be set so LLM emits JSON")
	}
	entry := lastLog(t, store)
	if entry.Status != StatusActed {
		t.Errorf("status = %q, want %q", entry.Status, StatusActed)
	}
	if posted != "今天天气不错" {
		t.Errorf("poster content = %q, want %q", posted, "今天天气不错")
	}
	if !entry.Admitted {
		t.Error("admitted must be true for a wake that entered orchestration")
	}
}

// DecisionNote：LLM 决定只记内部笔记 → noteSaver 被调用写入长期记忆，
// status=note、actions=["note"]，且不走伪频道 dispatcher（KVSuppressReply 恒为 true）。
// 这是 Bug-fix 验证：重设计前 DecisionNote 的笔记只存在于 LLM 的 JSON 输出里，
// 因 reply 被抑制而永不落库；现在由 Executor 经 noteSaver → bot.SaveNote 链路显式写入。
func TestExecute_DecisionNoteSavesToMemory(t *testing.T) {
	var saved string
	savedErr := false
	noteSaver := func(_ context.Context, content string) error {
		saved = content
		return nil
	}
	failSaver := func(_ context.Context, content string) error {
		savedErr = true
		return fmt.Errorf("boom")
	}

	// 1) 正常保存：笔记内容经 noteSaver 落库，日志 status=note。
	runner := &fakeRunner{llmText: `{"decision":"note","content":"明天跟进露娜的部署脚本评审"}`}
	e, store := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner, nil)
	e.noteSaver = noteSaver
	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if saved != "明天跟进露娜的部署脚本评审" {
		t.Errorf("noteSaver content = %q, want %q", saved, "明天跟进露娜的部署脚本评审")
	}
	entry := lastLog(t, store)
	if entry.Status != StatusNote {
		t.Errorf("status = %q, want %q", entry.Status, StatusNote)
	}
	if !reflect.DeepEqual(entry.Actions, []string{"note"}) {
		t.Errorf("actions = %v, want [note]", entry.Actions)
	}

	// 2) noteSaver 失败时只 Warn 不阻断：status 仍为 note，不升级为 error。
	runner2 := &fakeRunner{llmText: `{"decision":"note","content":"这条笔记存不进去，但不该炸"}`}
	e2, store2 := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner2, nil)
	e2.noteSaver = failSaver
	if _, err := e2.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !savedErr {
		t.Error("failSaver should have been invoked")
	}
	entry2 := lastLog(t, store2)
	if entry2.Status != StatusNote {
		t.Errorf("status on noteSaver failure = %q, want %q", entry2.Status, StatusNote)
	}

	// 3) DecisionNote 但内容为空 → 不调用 noteSaver（避免空笔记落库），仍记 note 日志。
	var calledOnEmpty bool
	emptySaver := func(_ context.Context, content string) error {
		calledOnEmpty = true
		return nil
	}
	runner3 := &fakeRunner{llmText: `{"decision":"note","content":""}`}
	e3, store3 := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner3, nil)
	e3.noteSaver = emptySaver
	if _, err := e3.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if calledOnEmpty {
		t.Error("noteSaver must NOT be called when content is empty")
	}
	entry3 := lastLog(t, store3)
	if entry3.Status != StatusNote {
		t.Errorf("empty-content note status = %q, want %q", entry3.Status, StatusNote)
	}
}

// bot 自主克制：LLM 决策 silent → silent（非 suppressed）。
func TestExecute_BotSilentWhenNothingToDo(t *testing.T) {
	runner := &fakeRunner{llmText: `{"decision":"silent","reason":"没有需要主动处理的事"}`}
	e, store := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner, nil)

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := lastLog(t, store).Status; got != StatusSilent {
		t.Errorf("status = %q, want %q", got, StatusSilent)
	}
}

// 决策 JSON 解析失败 → 安全降级为 silent（宁可多睡，不可误发）。
func TestExecute_DecisionParseFailureDowngradesToSilent(t *testing.T) {
	runner := &fakeRunner{llmText: `这不是合法 JSON 而且说想发帖`}
	e, store := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner, nil,
		withSingleTarget(new(string)))

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	entry := lastLog(t, store)
	if entry.Status != StatusSilent {
		t.Errorf("status = %q, want %q (parse failure must downgrade)", entry.Status, StatusSilent)
	}
	if entry.Decision != DecisionSilent {
		t.Errorf("decision = %q, want %q", entry.Decision, DecisionSilent)
	}
}

// LLM 决策 post 但目标不在可发列表 → 安全降级为 silent（不误发到未知渠道）。
func TestExecute_PostToUnknownTargetDowngradesToSilent(t *testing.T) {
	runner := &fakeRunner{llmText: `{"decision":"post","channel":"discord","conversation_id":"","content":"乱发"}`}
	var posted string
	e, store := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, runner, nil,
		withSingleTarget(&posted))

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	entry := lastLog(t, store)
	if entry.Status != StatusSilent {
		t.Errorf("status = %q, want %q (unknown target must downgrade)", entry.Status, StatusSilent)
	}
	if posted != "" {
		t.Errorf("must NOT post to unknown target, got %q", posted)
	}
}

// 准入关卡：首次唤醒放行；无信号的后续唤醒被拒 → 0-step，不调用编排，但仍落日志。
func TestExecute_AdmissionGuardRejectsIdleWake(t *testing.T) {
	runner := &fakeRunner{llmText: `{"decision":"silent"}`}
	admission := func(_ context.Context, _ time.Time) (bool, string) { return false, "" }
	e, store := newTestExecutor(t,
		Config{Enabled: true, Interval: 30, AllowPost: true, IdleWakeEvery: 4},
		runner, admission)

	// 第一次：lastWakeAt 为零值 → 必须放行（bot 启动后应有一次环视机会）
	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute #1: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("first wake must be admitted, calls=%d", runner.calls)
	}

	// 第二次：无信号 → 拒绝，0-step
	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute #2: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("idle wake must not reach orchestration, calls=%d", runner.calls)
	}
	entry := lastLog(t, store)
	if entry.Admitted {
		t.Error("rejected wake must be logged with admitted=false")
	}
	if entry.Status != StatusSilent {
		t.Errorf("status = %q, want %q", entry.Status, StatusSilent)
	}
}

// 准入关卡不能把 bot 永久冻死：连续拒绝达到 IdleWakeEvery 次时强制放行一次。
func TestExecute_AdmissionGuardForcedWakeAfterIdleLimit(t *testing.T) {
	runner := &fakeRunner{llmText: `{"decision":"silent"}`}
	admission := func(_ context.Context, _ time.Time) (bool, string) { return false, "" }
	e, _ := newTestExecutor(t,
		Config{Enabled: true, Interval: 30, AllowPost: true, IdleWakeEvery: 2},
		runner, admission)

	_, _ = e.Execute(context.Background(), nil) // #1 首次放行
	_, _ = e.Execute(context.Background(), nil) // #2 拒绝（idleRejects=1）
	if runner.calls != 1 {
		t.Fatalf("after #2 calls=%d, want 1", runner.calls)
	}
	_, _ = e.Execute(context.Background(), nil) // #3 连续拒绝达上限 → 强制放行
	if runner.calls != 2 {
		t.Fatalf("forced wake did not happen, calls=%d want 2", runner.calls)
	}
}

// 有信号时正常放行。
func TestExecute_AdmissionGuardAdmitsOnSignal(t *testing.T) {
	runner := &fakeRunner{llmText: `{"decision":"silent"}`}
	admission := func(_ context.Context, _ time.Time) (bool, string) { return true, "新增聊天消息 3 条" }
	e, _ := newTestExecutor(t,
		Config{Enabled: true, Interval: 30, AllowPost: true, IdleWakeEvery: 4},
		runner, admission)

	_, _ = e.Execute(context.Background(), nil)
	_, _ = e.Execute(context.Background(), nil)
	if runner.calls != 2 {
		t.Fatalf("both wakes must be admitted when signal present, calls=%d", runner.calls)
	}
}

// 硬频控：连续产生对外行动超过上限后降级为不发言（防自激刷屏）。
func TestExecute_FrequencyCapDegradesToSuppressed(t *testing.T) {
	var posted string
	runner := &fakeRunner{llmText: `{"decision":"post","channel":"Misskey","conversation_id":"","content":"again"}`}
	e, store := newTestExecutor(t,
		Config{Enabled: true, Interval: 30, AllowPost: true, MaxConsecutiveWakes: 1},
		runner, nil, withSingleTarget(&posted))

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute #1: %v", err)
	}
	if got := lastLog(t, store).Status; got != StatusActed {
		t.Fatalf("first wake status = %q, want %q", got, StatusActed)
	}

	// 第二次：已达连续上限 + 处于冷却窗内 → 降级压制（LLM 想发也被拦）
	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute #2: %v", err)
	}
	if v, ok := runner.lastEnv.Get(core.KVSuppressReply); !ok || v != true {
		t.Errorf("second wake must be suppressed by frequency cap, got %v (ok=%v)", v, ok)
	}
	if reason, _ := runner.lastEnv.Get(core.KVSuppressReplyReason); reason != "frequency_cap" {
		t.Errorf("suppress reason = %v, want frequency_cap", reason)
	}
	entry := lastLog(t, store)
	if entry.Status != StatusSuppressed {
		t.Errorf("status = %q, want %q", entry.Status, StatusSuppressed)
	}
	if posted != "again" {
		t.Errorf("first wake should have posted %q, got %q", "again", posted)
	}
}

// 真实外部消息应立刻恢复频控预算（§9.3），否则 bot 在被人搭话时反而说不了话。
func TestNotifyUserActivityResetsBudget(t *testing.T) {
	var posted string
	runner := &fakeRunner{llmText: `{"decision":"post","channel":"Misskey","conversation_id":"","content":"hi"}`}
	e, _ := newTestExecutor(t,
		Config{Enabled: true, Interval: 30, AllowPost: true, MaxConsecutiveWakes: 1},
		runner, nil, withSingleTarget(&posted))

	_, _ = e.Execute(context.Background(), nil) // 用掉预算
	e.NotifyUserActivity()                      // 真实外部活动 → 预算恢复

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute #2: %v", err)
	}
	// 预算恢复后第二次应再次 acted（而非被频控压制）。
	ls, err := e.store.LoadLogs("bot-1")
	if err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(ls.Logs) < 2 {
		t.Fatalf("expected >=2 logs, got %d", len(ls.Logs))
	}
	if ls.Logs[0].Status != StatusActed {
		t.Errorf("after budget reset, status = %q, want %q", ls.Logs[0].Status, StatusActed)
	}
}

// runner 未接线时必须落 error 日志并返回错误，而不是静默什么都不做。
func TestExecute_NilRunnerLogsError(t *testing.T) {
	e, store := newTestExecutor(t, Config{Enabled: true, Interval: 30, AllowPost: true}, nil, nil)

	if _, err := e.Execute(context.Background(), nil); err == nil {
		t.Fatal("expected error when runner is nil")
	}
	if got := lastLog(t, store).Status; got != StatusError {
		t.Errorf("status = %q, want %q", got, StatusError)
	}
}

// ============================================================
// 数据契约测试：被前端 / API handler 直接依赖，无需真实 LLM。
// 验证新配置字段与新日志状态语义能经 Store 往返（写入 → 读出一致）。
// ============================================================

// 配置 JSON 往返：新字段（allow_post / max_consecutive_wakes /
// cooldown_min / idle_wake_every）不得丢失或被零值覆盖。
func TestConfig_JSONRoundTrip(t *testing.T) {
	cfg := Config{
		Enabled:             true,
		Interval:            45,
		AllowPost:           true,
		MaxConsecutiveWakes: 7,
		CooldownMin:         15,
		IdleWakeEvery:       2,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != cfg {
		t.Errorf("config round-trip mismatch:\n got=%+v\nwant=%+v", got, cfg)
	}
}

// Store 持久化：SaveConfig 后 LoadConfig 必须回读全部新字段，
// 否则前端会拿到零值（误以为 allow_post=false）。
func TestStore_ConfigPersistsNewFields(t *testing.T) {
	store := NewStore(t.TempDir())
	in := Config{Enabled: true, Interval: 45, AllowPost: true, MaxConsecutiveWakes: 7, CooldownMin: 15, IdleWakeEvery: 2}
	if err := store.SaveConfig("bot-x", &in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.LoadConfig("bot-x")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AllowPost != true {
		t.Errorf("AllowPost = %v, want true", got.AllowPost)
	}
	if got.MaxConsecutiveWakes != 7 {
		t.Errorf("MaxConsecutiveWakes = %d, want 7", got.MaxConsecutiveWakes)
	}
	if got.CooldownMin != 15 {
		t.Errorf("CooldownMin = %d, want 15", got.CooldownMin)
	}
	if got.IdleWakeEvery != 2 {
		t.Errorf("IdleWakeEvery = %d, want 2", got.IdleWakeEvery)
	}
}

// 日志往返：新状态枚举与 actions / admitted / traceId / decision / target 字段必须完整保留。
func TestStore_LogRoundTripNewStatuses(t *testing.T) {
	store := NewStore(t.TempDir())

	samples := []struct {
		status   string
		actions  []string
		admitted bool
		traceID  string
		reason   string
		decision string
		target   string
	}{
		{StatusActed, []string{"reply"}, true, "hb-1", "", DecisionPost, "Misskey 时间线（发新帖）"},
		{StatusNote, []string{"note"}, true, "hb-2", "", DecisionNote, ""},
		{StatusSilent, []string{}, true, "hb-3", "", DecisionSilent, ""},
		{StatusSuppressed, []string{}, true, "hb-4", "platform_policy", DecisionPost, "Misskey 时间线（发新帖）"},
		{StatusSuppressed, []string{}, true, "hb-4b", "frequency_cap", DecisionPost, "Misskey 时间线（发新帖）"},
		{StatusError, []string{}, true, "hb-5", "", "", ""},
	}
	for i, s := range samples {
		entry := Log{
			ID:       fmt.Sprintf("hb-%d", i),
			Status:   s.status,
			Time:     "2026/8/14 10:00:00",
			Cost:     1.5,
			Actions:  s.actions,
			Result:   "sample " + s.status,
			Admitted: s.admitted,
			TraceID:  s.traceID,
			Reason:   s.reason,
			Decision: s.decision,
			Target:   s.target,
		}
		if err := store.AppendLog("bot-x", entry); err != nil {
			t.Fatalf("append %s: %v", s.status, err)
		}
	}

	ls, err := store.LoadLogs("bot-x")
	if err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(ls.Logs) != len(samples) {
		t.Fatalf("log count = %d, want %d", len(ls.Logs), len(samples))
	}
	byID := map[string]Log{}
	for _, l := range ls.Logs {
		byID[l.ID] = l
	}
	for i, s := range samples {
		id := fmt.Sprintf("hb-%d", i)
		l, ok := byID[id]
		if !ok {
			t.Errorf("missing log id %q", id)
			continue
		}
		if !reflect.DeepEqual(l.Actions, s.actions) {
			t.Errorf("id %q actions = %v, want %v", id, l.Actions, s.actions)
		}
		if l.Admitted != s.admitted {
			t.Errorf("id %q admitted = %v, want %v", id, l.Admitted, s.admitted)
		}
		if l.TraceID != s.traceID {
			t.Errorf("id %q traceId = %q, want %q", id, l.TraceID, s.traceID)
		}
		if l.Reason != s.reason {
			t.Errorf("id %q reason = %q, want %q", id, l.Reason, s.reason)
		}
		if l.Decision != s.decision {
			t.Errorf("id %q decision = %q, want %q", id, l.Decision, s.decision)
		}
		if l.Target != s.target {
			t.Errorf("id %q target = %q, want %q", id, l.Target, s.target)
		}
	}
}
