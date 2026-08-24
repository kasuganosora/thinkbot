package llm

import (
	"context"
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
	}
	for _, c := range cases {
		if got := isUserIntentGrounded(c.req); got != c.want {
			t.Errorf("isUserIntentGrounded(%q) = %v, want %v", c.req, got, c.want)
		}
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

	// 未根植：拦截，不执行
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

	// 根植：执行
	called = false
	cfg2 := &OrchestrateConfig{UserRequest: "在 misskey 上关注 @foo"}
	res2 := runTool(context.Background(), tc, tool, nil, cfg2)
	if res2.IsError {
		t.Fatalf("expected success when grounded, got %q", res2.Result)
	}
	if !called {
		t.Error("tool should execute when grounded")
	}
}
