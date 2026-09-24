package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNewLoopController_Defaults(t *testing.T) {
	// hard == 0 表示「不限制（无限）」，loopController 解释为 -1。
	lc := newLoopController(30, 0)
	if lc.soft != 30 {
		t.Errorf("soft = %d, want 30", lc.soft)
	}
	if lc.hard != -1 {
		t.Errorf("hard = %d, want -1 (0 = 不限制/无限)", lc.hard)
	}
	for _, step := range []int{0, 5, 100, 100000} {
		if !lc.shouldContinue(step) {
			t.Errorf("unlimited (hard=0): shouldContinue(%d) = false, want true", step)
		}
	}
}

func TestNewLoopController_ExplicitHard(t *testing.T) {
	lc := newLoopController(30, 120)
	if lc.hard != 120 {
		t.Errorf("hard = %d, want 120", lc.hard)
	}
}

func TestNewLoopController_HardClampedToSoft(t *testing.T) {
	// hard 小于 soft 时夹紧为 soft。
	lc := newLoopController(30, 10)
	if lc.hard != 30 {
		t.Errorf("hard = %d, want 30 (clamped to soft)", lc.hard)
	}
}

func TestNewLoopController_Unlimited(t *testing.T) {
	lc := newLoopController(-1, 0)
	if lc.hard != -1 {
		t.Errorf("hard = %d, want -1 (unlimited)", lc.hard)
	}
	for _, step := range []int{0, 5, 100, 100000} {
		if !lc.shouldContinue(step) {
			t.Errorf("unlimited: shouldContinue(%d) = false, want true", step)
		}
	}
}

func TestLoopController_ExtendsToHardWhenProgressing(t *testing.T) {
	// 每步都是「不同」的工具调用（持续推进），应一路放行到 hard-1，hard 处停。
	lc := newLoopController(5, 15)
	for step := 0; step < 15; step++ {
		if !lc.shouldContinue(step) {
			t.Fatalf("progressing: shouldContinue(%d) = false, want true (hard=15)", step)
		}
		// 每步喂入不同签名，模拟持续推进。
		lc.recordStep(step, sigForStep(step), "")
	}
	if lc.shouldContinue(15) {
		t.Errorf("shouldContinue(15) = true, want false (hard cap reached)")
	}
}

func TestLoopController_StallWithinSoftBudget(t *testing.T) {
	// 软预算内连续 3 步相同签名 → stalled。
	lc := newLoopController(30, 90)
	same := "AAAA"
	lc.recordStep(0, same, "")
	if lc.stalled {
		t.Fatal("stalled after 1 repeat, too eager")
	}
	lc.recordStep(1, same, "")
	if lc.stalled {
		t.Fatal("stalled after 2 repeats within soft budget, want tolerance of 3")
	}
	lc.recordStep(2, same, "")
	if !lc.stalled {
		t.Error("not stalled after 3 identical steps within soft budget")
	}
	if lc.shouldContinue(3) {
		t.Error("shouldContinue = true after stall")
	}
}

func TestLoopController_TightStallAfterSoftBudget(t *testing.T) {
	// 超出软预算后收紧：连续 2 步相同签名即 stalled。
	lc := newLoopController(5, 30)
	same := "BBBB"
	// step 5、6 均 >= soft(5)。
	lc.recordStep(5, same, "")
	if lc.stalled {
		t.Fatal("stalled after 1 repeat past soft, too eager")
	}
	lc.recordStep(6, same, "")
	if !lc.stalled {
		t.Error("not stalled after 2 identical steps past soft budget (tight limit)")
	}
}

func TestLoopController_DifferentSignaturesResetCount(t *testing.T) {
	// 不同签名穿插不应累积重复计数。
	lc := newLoopController(30, 90)
	lc.recordStep(0, "X", "")
	lc.recordStep(1, "X", "")
	lc.recordStep(2, "Y", "") // 打断
	lc.recordStep(3, "X", "")
	lc.recordStep(4, "X", "")
	if lc.stalled {
		t.Error("stalled despite interleaved different signatures")
	}
}

func TestLoopController_EmptySignatureResets(t *testing.T) {
	lc := newLoopController(30, 90)
	lc.recordStep(0, "X", "")
	lc.recordStep(1, "X", "")
	lc.recordStep(2, "", "") // 无工具调用，重置
	lc.recordStep(3, "X", "")
	if lc.repeatCount != 1 {
		t.Errorf("repeatCount = %d, want 1 after empty-sig reset", lc.repeatCount)
	}
}

func TestToolCallSignature_OrderStable(t *testing.T) {
	a := []ToolCall{
		{ToolName: "read_file", Input: map[string]any{"path": "a.go"}},
		{ToolName: "grep", Input: map[string]any{"q": "x"}},
	}
	b := []ToolCall{
		{ToolName: "grep", Input: map[string]any{"q": "x"}},
		{ToolName: "read_file", Input: map[string]any{"path": "a.go"}},
	}
	if toolCallSignature(a) != toolCallSignature(b) {
		t.Error("signature not stable across tool-call order")
	}
}

func TestToolCallSignature_DifferentArgsDiffer(t *testing.T) {
	a := []ToolCall{{ToolName: "read_file", Input: map[string]any{"path": "a.go"}}}
	b := []ToolCall{{ToolName: "read_file", Input: map[string]any{"path": "b.go"}}}
	if toolCallSignature(a) == toolCallSignature(b) {
		t.Error("different args produced same signature")
	}
}

func TestToolCallSignature_Empty(t *testing.T) {
	if toolCallSignature(nil) != "" {
		t.Error("nil tool calls should produce empty signature")
	}
}

// sigForStep 生成每步唯一的签名，用于模拟持续推进。
func sigForStep(step int) string {
	return toolCallSignature([]ToolCall{
		{ToolName: "read_file", Input: map[string]any{"n": step}},
	})
}

func TestLoopController_DerailmentTriggers(t *testing.T) {
	// 复现 2026-08 事故：每一步调的签名都不同（同签名重复检测失明），
	// 但助手文本反复含自我纠正信号 → 连续达到阈值即判定脱轨、停止循环。
	lc := newLoopController(10, 100)
	derailText := "停止这些无效调用，回到任务"
	// 前三步连续出现自我纠正信号，应一路放行（尚未达阈值 3）。
	for step := 0; step < 3; step++ {
		if !lc.shouldContinue(step) {
			t.Fatalf("should still continue at step %d", step)
		}
		lc.recordStep(step, sigForStep(step), derailText)
	}
	// 第三步写入后达到阈值 → 判定脱轨并停止循环。
	if !lc.derailed {
		t.Fatal("expected derailed=true after 3 consecutive self-correction steps")
	}
	if lc.shouldContinue(3) {
		t.Error("shouldContinue = true after derailment")
	}
	if !lc.stoppedByGuard(3) {
		t.Error("stoppedByGuard should be true after derailment")
	}
	if lc.describeLoopStop(3) == "" {
		t.Error("describeLoopStop empty after derailment")
	}
}

func TestLoopController_DerailmentResetsOnNormalText(t *testing.T) {
	lc := newLoopController(10, 100)
	derailText := "回到正题"
	lc.recordStep(0, sigForStep(0), derailText)
	lc.recordStep(1, sigForStep(1), derailText)
	// 正常任务文本（无自我纠正信号）应重置脱轨计数
	lc.recordStep(2, sigForStep(2), "现在改 home.js 的导航结构")
	lc.recordStep(3, sigForStep(3), derailText)
	lc.recordStep(4, sigForStep(4), derailText)
	if lc.derailed {
		t.Error("derailed=true despite a normal step breaking the pattern")
	}
}

func TestLoopController_DerailmentNotTriggeredByLegitTask(t *testing.T) {
	// 正常长任务（无自我纠正文本）不应被误杀。
	lc := newLoopController(5, 30)
	normal := "继续编辑文件并验证"
	for step := 0; step < 20; step++ {
		lc.recordStep(step, sigForStep(step), normal)
	}
	if lc.derailed {
		t.Error("legit long task wrongly flagged as derailed")
	}
}

func TestIsUserIntentGrounded(t *testing.T) {
	cases := []struct {
		req  string
		want bool
	}{
		{"给 cfblog 加无障碍树支持", false}, // 代码任务，无社交意图
		{"", false},                       // 空
		{"在 misskey 上关注 @foo", true},      // 动作动词
		{"关注某人", true},                    // 动作动词
		{"把更新发到 misskey", true},           // 发到某渠道
		{"Follow Alice on Misskey", true}, // 英文
		{"给这篇帖子点个赞", true},                // react
		{"帮我查一下 misskey 上的用户", false},     // 只读查询，不含写动作动词
		{"那你尝试下发一个misskey 验证下", true},    // 口语动词「发一个」
		{"发条 misskey，内容你定", true},          // 口语动词「发条」
		{"你修完后我帮你部署", false},             // 部署，非社交动作
	}
	for _, c := range cases {
		if got := isUserIntentGrounded(c.req); got != c.want {
			t.Errorf("isUserIntentGrounded(%q) = %v, want %v", c.req, got, c.want)
		}
	}
}

func TestCollectRecentUserMsgs(t *testing.T) {
	mk := func(text string) Message {
		return UserMessage(text)
	}
	if got := collectRecentUserMsgs(nil); got != nil {
		t.Error("nil msgs should collect nothing")
	}
	// 只有 assistant 消息 → 不算（防止模型自我说服绕过护栏）
	onlyAssistant := []Message{{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "发帖发帖"}}}}
	if got := collectRecentUserMsgs(onlyAssistant); got != nil {
		t.Error("assistant messages must not count as user intent context")
	}
	// 常规历史 → 收集到最近 3 条 user 消息（时间正序）
	msgs := []Message{
		mk("你好"),
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "你好喵"}}},
		mk("帮我修个 bug"),
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "修好了"}}},
		mk("发条 misskey，内容你定"),
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "正在处理"}}},
		mk("内容你定就行"),
	}
	got := collectRecentUserMsgs(msgs)
	if len(got) != 3 {
		t.Fatalf("expected 3 recent user msgs, got %d: %v", len(got), got)
	}
	if got[0] != "帮我修个 bug" || got[1] != "发条 misskey，内容你定" || got[2] != "内容你定就行" {
		t.Errorf("expected chronological order, got %v", got)
	}
	// 窗口外授权：20 条之后的旧消息不该被收集到
	var far []Message
	far = append(far, mk("发条 misskey")) // 授权在第 1 条
	for i := 0; i < 20; i++ {
		far = append(far, mk(fmt.Sprintf("后续闲聊 %d", i)))
	}
	for _, m := range collectRecentUserMsgs(far) {
		if m == "发条 misskey" {
			t.Error("authorization outside lookback window must not be collected")
		}
	}
}

// mockIntentJudgeClient 可编程的快判客户端桩。
type mockIntentJudgeClient struct {
	resp   string
	err    error
	gotSys string
	gotUsr string
}

func (m *mockIntentJudgeClient) Chat(ctx context.Context, system, user string) (string, error) {
	m.gotSys = system
	m.gotUsr = user
	if m.err != nil {
		return "", m.err
	}
	return m.resp, nil
}

func TestCheckUserIntentGrounded(t *testing.T) {
	ctx := context.Background()

	// 关键词快速通道：命中高置信词，不调 LLM
	mock := &mockIntentJudgeClient{resp: "NO 不该到这一步"}
	res := checkUserIntentGrounded(ctx, "misskey_follow_user", "在 misskey 上关注 @foo", nil,
		&OrchestrateConfig{IntentJudge: mock})
	if !res.grounded {
		t.Error("keyword fast-path should ground intent without LLM call")
	}
	if mock.gotUsr != "" {
		t.Error("keyword fast-path must not invoke the LLM judge")
	}

	// 未注入 judge → fail-closed
	res = checkUserIntentGrounded(ctx, "misskey_follow_user", "给 cfblog 加无障碍树支持", nil, nil)
	if res.grounded {
		t.Error("no judge configured must fail closed")
	}

	// 关键词未命中 + judge 判 YES（口语授权「那你就发呗」由 LLM 翻案——
	// 注意这句不含任何关键词，专门覆盖快速通道漏词的场景）
	mock = &mockIntentJudgeClient{resp: "YES 用户明确要求发帖"}
	res = checkUserIntentGrounded(ctx, "misskey_create_note", "那你就发呗", nil,
		&OrchestrateConfig{IntentJudge: mock})
	if !res.grounded {
		t.Errorf("judge YES should ground, reason=%q", res.reason)
	}
	if !strings.Contains(mock.gotUsr, "那你就发呗") {
		t.Errorf("judge should receive the user request, got %q", mock.gotUsr)
	}

	// 关键词未命中 + judge 判 NO（只读查询不放行）
	mock = &mockIntentJudgeClient{resp: "NO 用户只是查询"}
	res = checkUserIntentGrounded(ctx, "misskey_create_note", "帮我查下 misskey 上的用户", nil,
		&OrchestrateConfig{IntentJudge: mock})
	if res.grounded {
		t.Error("judge NO must not ground")
	}

	// judge 报错 → fail-closed（同样用不含关键词的措辞，确保真的走了 judge）
	mock = &mockIntentJudgeClient{err: errors.New("provider down")}
	res = checkUserIntentGrounded(ctx, "misskey_create_note", "那你就发呗", nil,
		&OrchestrateConfig{IntentJudge: mock})
	if res.grounded {
		t.Error("judge error must fail closed")
	}
}

func TestRunTool_RequiresUserIntent(t *testing.T) {
	called := false
	tool := &Tool{
		Name:               "misskey_follow_user",
		RequiresUserIntent: true,
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			called = true
			return "ok", nil
		},
	}
	tc := ToolCall{ToolCallID: "c1", ToolName: "misskey_follow_user", Input: map[string]any{"userId": "x"}}

	// 未根植（无 judge → fail-closed）：拦截，不执行
	cfg := &OrchestrateConfig{UserRequest: "给 cfblog 加无障碍树支持"}
	res := runTool(context.Background(), tc, tool, nil, cfg)
	if !res.IsError {
		t.Fatal("expected blocked result when user request is not grounded")
	}
	if called {
		t.Error("tool must NOT execute when not grounded")
	}
	msg, _ := res.Result.(string)
	if !strings.Contains(msg, "拦截") {
		t.Errorf("refusal message missing, got %q", res.Result)
	}

	// 根植（关键词快速通道）：执行
	called = false
	cfg2 := &OrchestrateConfig{UserRequest: "在 misskey 上关注 @foo"}
	res2 := runTool(context.Background(), tc, tool, nil, cfg2)
	if res2.IsError {
		t.Fatalf("expected success when grounded, got %q", res2.Result)
	}
	if !called {
		t.Error("tool should execute when grounded")
	}

	// 根植（LLM 快判 YES，口语授权措辞）：执行
	called = false
	cfg3 := &OrchestrateConfig{
		UserRequest: "发条 misskey，内容你定",
		IntentJudge: &mockIntentJudgeClient{resp: "YES 用户明确要求发帖"},
	}
	res3 := runTool(context.Background(), tc, tool, nil, cfg3)
	if res3.IsError {
		t.Fatalf("expected success when judge grounds, got %q", res3.Result)
	}
	if !called {
		t.Error("tool should execute when judge grounds")
	}
}
