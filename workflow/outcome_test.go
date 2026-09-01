package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// 节点结果类别（Outcome）与确定性指标
// ============================================================================

func TestParseNodeOutcome(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    NodeOutcome
		wantOK  bool
	}{
		{name: "空值等价ok", input: "", want: OutcomeOK, wantOK: true},
		{name: "ok", input: "ok", want: OutcomeOK, wantOK: true},
		{name: "noop", input: "noop", want: OutcomeNoop, wantOK: true},
		{name: "partial", input: "partial", want: OutcomePartial, wantOK: true},
		{name: "missing_tool", input: "missing_tool", want: OutcomeMissingTool, wantOK: true},
		{name: "missing_data", input: "missing_data", want: OutcomeMissingData, wantOK: true},
		{name: "大小写不敏感", input: "Missing_Tool", want: OutcomeMissingTool, wantOK: true},
		{name: "带空格", input: "  partial  ", want: OutcomePartial, wantOK: true},

		// 非法值必须能被识别出来（调用方告警），不能静默当 ok
		{name: "未知类别", input: "kinda_done", wantOK: false},
		{name: "拼写错误", input: "missing_too", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseNodeOutcome(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutcomeClassification(t *testing.T) {
	tests := []struct {
		outcome    NodeOutcome
		isBlocked  bool
		isDegraded bool
	}{
		{OutcomeOK, false, false},
		{OutcomeNoop, false, true},
		{OutcomePartial, false, true},
		{OutcomeMissingTool, true, false},
		{OutcomeMissingData, true, false},
	}
	for _, tt := range tests {
		if got := tt.outcome.IsBlocked(); got != tt.isBlocked {
			t.Errorf("%s.IsBlocked() = %v, want %v", tt.outcome, got, tt.isBlocked)
		}
		if got := tt.outcome.IsDegraded(); got != tt.isDegraded {
			t.Errorf("%s.IsDegraded() = %v, want %v", tt.outcome, got, tt.isDegraded)
		}
	}
	// 空值（存量数据）不得被误判
	if (NodeOutcome("")).IsBlocked() || (NodeOutcome("")).IsDegraded() {
		t.Error("empty outcome must be neither blocked nor degraded")
	}
}

// TestErrForBlocked 哨兵错误映射。
func TestErrForBlocked(t *testing.T) {
	if errForBlocked(OutcomeMissingTool) == nil {
		t.Error("missing_tool should map to a sentinel error")
	}
	if errForBlocked(OutcomeMissingData) == nil {
		t.Error("missing_data should map to a sentinel error")
	}
	if err := errForBlocked(OutcomeOK); err != nil {
		t.Errorf("ok should map to nil, got %v", err)
	}
}

// TestIsNonRetryable_BlockedOutcomes 复用既有的确定性失败判定。
//
// 关键：不另起一套判断，而是接进 retry_classify 的 isNonRetryable。
// 这样「缺工具」与「额度耗尽」走同一条快速失败路径。
func TestIsNonRetryable_BlockedOutcomes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "缺工具不重试",
			err:  errors.Join(errors.New("node n1 blocked"), ErrMissingTool()),
			want: true,
		},
		{
			name: "缺数据不重试",
			err:  errors.Join(errors.New("node n1 blocked"), ErrMissingData()),
			want: true,
		},
		{
			name: "裸哨兵错误不重试",
			err:  ErrMissingTool(),
			want: true,
		},
		{
			name: "普通错误仍可重试",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNonRetryable(tc.err); got != tc.want {
				t.Errorf("isNonRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestReviewLoop_BlockedOutcomeStopsIteration 缺工具时既不迭代也不重试。
//
// 这是本阶段最核心的行为保证：缺工具是环境事实，重跑一百次也不会有工具。
// 继续迭代只会把整条工作流的预算烧光。
func TestReviewLoop_BlockedOutcomeStopsIteration(t *testing.T) {
	for _, outcome := range []string{"missing_tool", "missing_data"} {
		t.Run(outcome, func(t *testing.T) {
			wf := NewWorkflow("wf", "req", []*DAGNode{
				{ID: "n1", Name: "task1", Review: true, MaxIterations: 3},
			})
			wf.RebuildIndex()

			exec := &mockExecutor{
				execResult: "partial output",
				reviewResults: []*ReviewResult{
					{Passed: false, Feedback: "做不了", Outcome: outcome, OutcomeReason: "缺工具"},
					{Passed: false, Feedback: "第二轮不该发生"},
					{Passed: false, Feedback: "第三轮不该发生"},
				},
			}
			s := newMockScheduler(wf, exec)

			node, _ := wf.GetNode("n1")
			s.reviewLoop(context.Background(), node, "initial")

			// 只 review 一次——不迭代
			if got := exec.revCalls.Load(); got != 1 {
				t.Errorf("expected exactly 1 review call (no iteration), got %d", got)
			}
			// 不重执行——迭代解决不了
			if got := exec.fbCalls.Load(); got != 0 {
				t.Errorf("expected 0 re-executions, got %d", got)
			}
			if node.Outcome != NodeOutcome(outcome) {
				t.Errorf("node.Outcome: got %q, want %q", node.Outcome, outcome)
			}
		})
	}
}

// TestReviewLoop_DegradedCompletionStillSucceeds 降级完成仍算成功，但记录 Outcome。
func TestReviewLoop_DegradedCompletionStillSucceeds(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "task1", Review: true, MaxIterations: 3},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		reviewResults: []*ReviewResult{
			{Passed: true, Outcome: "partial", OutcomeReason: "只覆盖了 3 个文件里的 2 个"},
		},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	result, err := s.reviewLoop(context.Background(), node, "some output")
	if err != nil {
		t.Fatalf("degraded completion should not error: %v", err)
	}
	if result == "" {
		t.Error("result should be preserved")
	}
	if node.Outcome != OutcomePartial {
		t.Errorf("node.Outcome: got %q, want partial", node.Outcome)
	}
}

// TestOutcome_BackwardCompatibleDeserialization 旧数据反序列化不得改变行为。
//
// 存量工作流的 JSON 里没有 outcome / toolProfile 字段，反序列化后必须是
// 「零值」且被解释为 ok / full——不能因为加了字段就让旧数据跑出不同结果。
func TestOutcome_BackwardCompatibleDeserialization(t *testing.T) {
	old := `{"id":"n1","name":"task1","task":"do","status":"completed","result":"done"}`

	var n DAGNode
	if err := json.Unmarshal([]byte(old), &n); err != nil {
		t.Fatalf("unmarshal legacy node: %v", err)
	}
	if n.Outcome != "" {
		t.Errorf("legacy node should have empty outcome, got %q", n.Outcome)
	}
	if n.Outcome.IsBlocked() || n.Outcome.IsDegraded() {
		t.Error("empty outcome must behave as ok")
	}
	if p, err := ParseToolProfile(string(n.ToolProfile)); err != nil || p != ProfileFull {
		t.Errorf("empty toolProfile should parse as full, got %q err=%v", p, err)
	}
	if p := toolsForProfile(n.ToolProfile); p != nil {
		t.Errorf("empty toolProfile should mean no filtering, got %v", p)
	}
}

// TestToFlat_ExposesOutcome 前端视图必须带上 outcome。
//
// workflow 面板走轮询 REST 拿到的就是 NodeFlat，且只按 status 渲染。
// 若不暴露 outcome，一个 completed 但 missing_tool 的节点在用户看来
// 就是普通的 ✓——而它实际什么都没做成。
func TestToFlat_ExposesOutcome(t *testing.T) {
	n := &DAGNode{
		ID:            "n1",
		Name:          "task1",
		Status:        NodeCompleted,
		Outcome:       OutcomeMissingTool,
		OutcomeReason: "缺少 sandbox_exec",
		ToolProfile:   ProfileReadOnly,
	}
	flat := n.ToFlat()

	if flat.Outcome != "missing_tool" {
		t.Errorf("Outcome: got %q, want missing_tool", flat.Outcome)
	}
	if flat.OutcomeReason != "缺少 sandbox_exec" {
		t.Errorf("OutcomeReason: got %q", flat.OutcomeReason)
	}
	if flat.ToolProfile != "readonly" {
		t.Errorf("ToolProfile: got %q, want readonly", flat.ToolProfile)
	}

	// 序列化后字段确实存在（前端收到的是 JSON）
	b, err := json.Marshal(flat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"outcome":"missing_tool"`) {
		t.Errorf("serialized flat must contain outcome, got %s", b)
	}
}

// TestGrade_Basic 确定性指标计算。
func TestGrade_Basic(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	end := time.Now()

	n := &DAGNode{
		ID:             "n1",
		RetryCount:     1,
		IterationCount: 2,
		Result:         "some output",
		ReviewFeedback: "最后一轮意见",
		StartedAt:      &start,
		CompletedAt:    &end,
		ReviewHistory: []ReviewRecord{
			{Iteration: 1, Passed: false, Feedback: "需要补充错误处理"},
			{Iteration: 2, Passed: true, Feedback: "好了"},
		},
	}

	g := Grade(n)
	if g.Retries != 1 {
		t.Errorf("Retries: got %d, want 1", g.Retries)
	}
	if g.ReviewIterations != 2 {
		t.Errorf("ReviewIterations: got %d, want 2", g.ReviewIterations)
	}
	if g.ReviewPassedAt != 2 {
		t.Errorf("ReviewPassedAt: got %d, want 2 (passed on 2nd round)", g.ReviewPassedAt)
	}
	if g.DurationSec <= 0 {
		t.Errorf("DurationSec should be positive, got %f", g.DurationSec)
	}
	if g.ResultLen != len("some output") {
		t.Errorf("ResultLen: got %d", g.ResultLen)
	}
	if g.Loops != 0 {
		t.Errorf("Loops: got %d, want 0 (feedbacks differ a lot)", g.Loops)
	}
}

// TestGrade_LoopDetection 检测原地打转：相邻轮次的审查意见高度相似。
func TestGrade_LoopDetection(t *testing.T) {
	same := "需要补充错误处理逻辑，并且加上单元测试"

	tests := []struct {
		name       string
		history    []ReviewRecord
		wantLoops  int
	}{
		{
			name: "完全相同算一轮打转",
			history: []ReviewRecord{
				{Iteration: 1, Passed: false, Feedback: same},
				{Iteration: 2, Passed: false, Feedback: same},
			},
			wantLoops: 1,
		},
		{
			name: "轻微改写仍算打转",
			history: []ReviewRecord{
				{Iteration: 1, Passed: false, Feedback: same},
				{Iteration: 2, Passed: false, Feedback: same + "，另外完善一下"},
			},
			wantLoops: 1,
		},
		{
			name: "内容不同不算打转",
			history: []ReviewRecord{
				{Iteration: 1, Passed: false, Feedback: "需要补充错误处理逻辑，并且加上单元测试"},
				{Iteration: 2, Passed: false, Feedback: "这次改成重命名变量并抽出公共函数"},
			},
			wantLoops: 0,
		},
		{
			name:      "单轮无历史不算打转",
			history:   []ReviewRecord{{Iteration: 1, Passed: false, Feedback: same}},
			wantLoops: 0,
		},
		{
			name: "空意见跳过不误判",
			history: []ReviewRecord{
				{Iteration: 1, Passed: false, Feedback: ""},
				{Iteration: 2, Passed: false, Feedback: ""},
			},
			wantLoops: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := Grade(&DAGNode{ReviewHistory: tt.history})
			if g.Loops != tt.wantLoops {
				t.Errorf("Loops: got %d, want %d", g.Loops, tt.wantLoops)
			}
		})
	}
}

// TestGrade_FirstPass 首轮即通过时 ReviewPassedAt 为 1，零值字段不应误报。
func TestGrade_FirstPass(t *testing.T) {
	g := Grade(&DAGNode{
		ReviewHistory: []ReviewRecord{{Iteration: 1, Passed: true}},
	})
	if g.ReviewPassedAt != 1 {
		t.Errorf("ReviewPassedAt: got %d, want 1", g.ReviewPassedAt)
	}
	if g.Loops != 0 || g.DurationSec != 0 || g.Retries != 0 {
		t.Errorf("zero-value node should produce zero grades, got %+v", g)
	}
}
