package llm

import "testing"

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
		lc.recordStep(step, sigForStep(step))
	}
	if lc.shouldContinue(15) {
		t.Errorf("shouldContinue(15) = true, want false (hard cap reached)")
	}
}

func TestLoopController_StallWithinSoftBudget(t *testing.T) {
	// 软预算内连续 3 步相同签名 → stalled。
	lc := newLoopController(30, 90)
	same := "AAAA"
	lc.recordStep(0, same)
	if lc.stalled {
		t.Fatal("stalled after 1 repeat, too eager")
	}
	lc.recordStep(1, same)
	if lc.stalled {
		t.Fatal("stalled after 2 repeats within soft budget, want tolerance of 3")
	}
	lc.recordStep(2, same)
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
	lc.recordStep(5, same)
	if lc.stalled {
		t.Fatal("stalled after 1 repeat past soft, too eager")
	}
	lc.recordStep(6, same)
	if !lc.stalled {
		t.Error("not stalled after 2 identical steps past soft budget (tight limit)")
	}
}

func TestLoopController_DifferentSignaturesResetCount(t *testing.T) {
	// 不同签名穿插不应累积重复计数。
	lc := newLoopController(30, 90)
	lc.recordStep(0, "X")
	lc.recordStep(1, "X")
	lc.recordStep(2, "Y") // 打断
	lc.recordStep(3, "X")
	lc.recordStep(4, "X")
	if lc.stalled {
		t.Error("stalled despite interleaved different signatures")
	}
}

func TestLoopController_EmptySignatureResets(t *testing.T) {
	lc := newLoopController(30, 90)
	lc.recordStep(0, "X")
	lc.recordStep(1, "X")
	lc.recordStep(2, "") // 无工具调用，重置
	lc.recordStep(3, "X")
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
