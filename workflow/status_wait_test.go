package workflow

import (
	"context"
	"testing"
	"time"
)

// TestAsBool_LenientParsing 验证宽松布尔解析。
//
// LLM 生成的 JSON 常把布尔写成字符串或数字；严格断言会让 wait 参数被静默忽略，
// 表现为「传了 wait 却还是立刻返回」这种极难排查的问题。
func TestAsBool_LenientParsing(t *testing.T) {
	truthy := []any{true, "true", "True", "1", "yes", 1, float64(1)}
	for _, v := range truthy {
		if !asBool(v) {
			t.Errorf("asBool(%#v) = false, want true", v)
		}
	}
	falsy := []any{nil, false, "false", "", "0", 0, float64(0), "random"}
	for _, v := range falsy {
		if asBool(v) {
			t.Errorf("asBool(%#v) = true, want false", v)
		}
	}
}

// TestAsInt_LenientParsing 验证宽松整数解析（JSON 数字解出来是 float64）。
func TestAsInt_LenientParsing(t *testing.T) {
	cases := map[any]int{
		float64(600): 600,
		int(30):      30,
		int64(45):    45,
		"120":        120,
		"bad":        0,
		nil:          0,
	}
	for in, want := range cases {
		if got := asInt(in); got != want {
			t.Errorf("asInt(%#v) = %d, want %d", in, got, want)
		}
	}
}

// TestStatusWaitTimeouts_SaneBounds 守住等待参数的合理区间。
//
// 无上限等待会把 agent 的一次工具调用永久挂死；轮询过密只是徒增 DB 读。
func TestStatusWaitTimeouts_SaneBounds(t *testing.T) {
	if statusWaitPollInterval < time.Second || statusWaitPollInterval > 10*time.Second {
		t.Errorf("statusWaitPollInterval = %v, out of sane range", statusWaitPollInterval)
	}
	if statusWaitDefaultTimeout <= 0 {
		t.Error("statusWaitDefaultTimeout must be positive, otherwise wait mode never blocks")
	}
	if statusWaitDefaultTimeout > statusWaitMaxTimeout {
		t.Errorf("default timeout %v exceeds max %v", statusWaitDefaultTimeout, statusWaitMaxTimeout)
	}
	if statusWaitMaxTimeout > time.Hour {
		t.Errorf("statusWaitMaxTimeout = %v, too long: a stuck workflow would pin the agent's tool call",
			statusWaitMaxTimeout)
	}
}

// TestTaskBlockingTimeout_FitsInsideBackgroundDrain 守住一条跨包偏序不变量。
//
// task 工具「提交即阻塞」，而 api/handler_chat.go 的 drainAndSaveInBackground 在客户端
// 断开后只用一个 20 分钟的 bgCtx 继续消费事件并落库。若阻塞上限 >= 那个值，
// bgCtx 会先到期 → 最终 assistant 回复（含工作流结果）永不落库，用户重连只看到腰斩的消息。
//
// 这里把 20 分钟写死作为「契约副本」：任何一侧被改动都会让本测试失败，迫使同步复核。
func TestTaskBlockingTimeout_FitsInsideBackgroundDrain(t *testing.T) {
	const backgroundDrainTimeout = 20 * time.Minute // 必须与 api/handler_chat.go 的 bgCtx 保持一致

	if taskBlockingMaxTimeout <= 0 {
		t.Fatal("taskBlockingMaxTimeout must be positive, otherwise task never blocks")
	}
	if taskBlockingMaxTimeout >= backgroundDrainTimeout {
		t.Errorf("taskBlockingMaxTimeout = %v must stay below the background drain timeout %v, "+
			"otherwise a disconnected client loses the final workflow result",
			taskBlockingMaxTimeout, backgroundDrainTimeout)
	}
	// 留够落库余量：至少比后台上限少 1 分钟。
	if backgroundDrainTimeout-taskBlockingMaxTimeout < time.Minute {
		t.Errorf("taskBlockingMaxTimeout = %v leaves less than 1min headroom before %v; "+
			"saving the assistant reply needs slack",
			taskBlockingMaxTimeout, backgroundDrainTimeout)
	}
}

// newWaitTestManager 造一个只带内存仓库的 Manager，够 GetStatus 用。
func newWaitTestManager(t *testing.T, wf *Workflow) *Manager {
	t.Helper()
	repo := NewRepository(nil, nil)
	if err := repo.Save(wf); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	return &Manager{repo: repo}
}

// TestWaitForTerminal_ReturnsImmediatelyWhenAlreadyTerminal 验证已完成的任务不白等。
//
// 若首次不查就先睡一个轮询间隔，每次调用都要凭空多等数秒。
func TestWaitForTerminal_ReturnsImmediatelyWhenAlreadyTerminal(t *testing.T) {
	wf := NewWorkflow("wf-done", "req", nil)
	wf.Status = WorkflowCompleted
	mgr := newWaitTestManager(t, wf)

	start := time.Now()
	res, err := waitForTerminal(context.Background(), mgr, "wf-done", time.Minute, nil)
	if err != nil {
		t.Fatalf("waitForTerminal: %v", err)
	}
	if elapsed := time.Since(start); elapsed > statusWaitPollInterval {
		t.Errorf("took %v; a terminal workflow must return without waiting a poll tick", elapsed)
	}
	if res.TimedOut {
		t.Error("TimedOut should be false for an already-terminal workflow")
	}
	if res.Status != WorkflowCompleted {
		t.Errorf("Status = %v, want completed", res.Status)
	}
}

// TestWaitForTerminal_HonorsContextCancel 验证响应取消。
//
// 用户点停止 / 客户端断开时必须立刻退出，不能继续空转到超时。
func TestWaitForTerminal_HonorsContextCancel(t *testing.T) {
	wf := NewWorkflow("wf-running", "req", nil)
	wf.Status = WorkflowRunning
	mgr := newWaitTestManager(t, wf)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	// 给一个远大于取消时间的超时：若不响应取消，这个用例会挂到超时。
	res, err := waitForTerminal(ctx, mgr, "wf-running", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("waitForTerminal: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v; cancel must abort the wait promptly", elapsed)
	}
	if !res.TimedOut {
		t.Error("TimedOut should be true when the wait was aborted")
	}
	// 快照仍应可用，方便调用方知道任务当前在哪。
	if res.StatusResult == nil || res.Status != WorkflowRunning {
		t.Error("cancelled wait must still return the latest snapshot")
	}
}

// TestWaitForTerminal_TimeoutIsNotAnError 验证超时以标记表达而非 error。
//
// 若超时报错，agent 会被迫走异常分支；返回快照 + timedOut 才能让它自行决定继续等还是换策略。
func TestWaitForTerminal_TimeoutIsNotAnError(t *testing.T) {
	wf := NewWorkflow("wf-slow", "req", nil)
	wf.Status = WorkflowAnalyzing
	mgr := newWaitTestManager(t, wf)

	res, err := waitForTerminal(context.Background(), mgr, "wf-slow", 10*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("timeout must not be reported as error, got: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut should be true after the deadline elapsed")
	}
	if res.Hint == "" {
		t.Error("Hint should tell the agent what to do next")
	}
	if res.StatusResult == nil {
		t.Fatal("timed-out wait must still carry a snapshot")
	}
}

// TestWaitForTerminal_UnknownWorkflowErrors 验证查不到工作流时确实报错。
func TestWaitForTerminal_UnknownWorkflowErrors(t *testing.T) {
	mgr := newWaitTestManager(t, NewWorkflow("wf-exists", "req", nil))
	if _, err := waitForTerminal(context.Background(), mgr, "wf-missing", time.Second, nil); err == nil {
		t.Fatal("expected an error for an unknown workflow id")
	}
}

// TestWaitForTerminal_ProgressCarriesWorkflowID 守住前端面板能在执行期显示的前提。
//
// 背景（真实回归）：task 改为「提交即阻塞」后，tool_result 要等工作流跑到终态才到达。
// 前端只从工具事件里提取 workflowId 来挂载工作流面板，若进度事件不带 workflowId，
// 整个执行期间（可能数十分钟）面板都不会出现，用户完全看不到进度。
//
// 因此：onProgress 推送的 payload 必须携带 workflowId。
func TestWaitForTerminal_ProgressCarriesWorkflowID(t *testing.T) {
	const wfID = "wf-progress"
	wf := NewWorkflow(wfID, "req", nil)
	wf.Status = WorkflowRunning
	mgr := newWaitTestManager(t, wf)

	var gotWorkflowID string
	sawProgress := false

	// 模拟 submitToolDef 里的 onProgress：必须走 taskProgressPayload，把 workflowId 一起推给前端。
	onProgress := func(st *StatusResult, waited time.Duration) {
		sawProgress = true
		payload := taskProgressPayload(wfID, "工作流执行中… 状态="+string(st.Status))
		if v, ok := payload["workflowId"].(string); ok {
			gotWorkflowID = v
		}
	}

	// 用一个略大于轮询间隔的超时，确保至少触发一次 onProgress回调。
	ctx, cancel := context.WithTimeout(context.Background(), statusWaitPollInterval+2*time.Second)
	defer cancel()
	if _, err := waitForTerminal(ctx, mgr, wfID, statusWaitPollInterval+2*time.Second, onProgress); err != nil {
		t.Fatalf("waitForTerminal: %v", err)
	}

	if !sawProgress {
		t.Fatal("onProgress was never invoked; the frontend would get no progress at all")
	}
	if gotWorkflowID != wfID {
		t.Errorf("progress payload must carry workflowId = %q, got %q; "+
			"without it the workflow panel never mounts during execution", wfID, gotWorkflowID)
	}
}

// TestTaskProgressPayload_CarriesWorkflowID 直接验证 task 工具进度事件的构造契约。
//
// 这是上一条不变量的单元版本：前端只从工具事件里提取 workflowId 来挂载面板，
// 因此每条进度 payload 都必须带上它；同时 stream/chunk 是前端消费的既有字段，不能丢。
func TestTaskProgressPayload_CarriesWorkflowID(t *testing.T) {
	const wfID = "wf-abc123"
	p := taskProgressPayload(wfID, "工作流执行中…")

	if got, _ := p["workflowId"].(string); got != wfID {
		t.Errorf("payload workflowId = %q, want %q; without it the panel never mounts during execution", got, wfID)
	}
	if got, _ := p["stream"].(string); got != "stdout" {
		t.Errorf("payload stream = %q, want \"stdout\" (frontend routes output by this field)", got)
	}
	if got, _ := p["chunk"].(string); got == "" {
		t.Error("payload chunk must be non-empty; the frontend appends it to the tool card output")
	}
}
