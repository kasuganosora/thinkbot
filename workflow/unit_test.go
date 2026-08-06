package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// 纯逻辑单元测试 — 不依赖 LLM
//
// 覆盖 models.go、types.go、executor.go、scheduler.go 中的辅助函数。
// ============================================================================

// --- ToModel / FromModel 往返 ---

func TestToFromModel_RoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second) // JSON 精度问题
	started := now.Add(-1 * time.Hour)
	finished := now

	original := &Workflow{
		ID:          "wf-rt",
		Status:      WorkflowCompleted,
		Requirement: "build a feature",
		Nodes: []*DAGNode{
			{
				ID:             "n1",
				Name:           "task1",
				Task:           "do something",
				Status:         NodeCompleted,
				Result:         "done",
				MaxRetries:     2,
				MaxIterations:  3,
				Review:         true,
				ReviewFeedback: "looks good",
				ReviewHistory: []ReviewRecord{
					{Iteration: 1, Passed: true, Feedback: "ok"},
				},
				StartedAt:   &started,
				CompletedAt: &finished,
			},
		},
		CreatedAt:  now,
		StartedAt:  &started,
		FinishedAt: &finished,
	}
	original.EnsureIndex()

	model, err := ToModel(original)
	if err != nil {
		t.Fatalf("ToModel failed: %v", err)
	}
	if model.ID != original.ID {
		t.Errorf("model ID mismatch: %s != %s", model.ID, original.ID)
	}
	if model.Data == "" {
		t.Error("model Data should not be empty")
	}
	if model.CreatedAt.IsZero() {
		t.Error("model CreatedAt should not be zero")
	}
	if model.UpdatedAt.IsZero() {
		t.Error("model UpdatedAt should not be zero")
	}

	restored, err := FromModel(model)
	if err != nil {
		t.Fatalf("FromModel failed: %v", err)
	}
	if restored.ID != original.ID {
		t.Errorf("restored ID mismatch: %s != %s", restored.ID, original.ID)
	}
	if restored.Status != original.Status {
		t.Errorf("restored Status mismatch: %s != %s", restored.Status, original.Status)
	}
	if restored.Requirement != original.Requirement {
		t.Errorf("restored Requirement mismatch")
	}
	if len(restored.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(restored.Nodes))
	}
	rn := restored.Nodes[0]
	if rn.ID != "n1" || rn.Result != "done" || rn.Review != true {
		t.Errorf("restored node fields mismatch: %+v", rn)
	}
	if len(rn.ReviewHistory) != 1 {
		t.Errorf("expected 1 review record, got %d", len(rn.ReviewHistory))
	}
	// EnsureIndex should have been called
	if _, ok := restored.GetNode("n1"); !ok {
		t.Error("GetNode should work after FromModel (EnsureIndex)")
	}
}

func TestFromModel_InvalidJSON(t *testing.T) {
	model := &dao.WorkflowModel{ID: "bad", Data: "{invalid json"}
	_, err := FromModel(model)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// --- types.go: IsRecoverable ---

func TestWorkflowStatus_IsRecoverable(t *testing.T) {
	tests := []struct {
		status WorkflowStatus
		want   bool
	}{
		{WorkflowAnalyzing, true},
		{WorkflowRunning, true},
		{WorkflowInterrupted, true},
		{WorkflowCompleted, false},
		{WorkflowFailed, false},
		{WorkflowTerminated, false},
	}
	for _, tt := range tests {
		if got := tt.status.IsRecoverable(); got != tt.want {
			t.Errorf("%s.IsRecoverable() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

// --- types.go: ToFlat ---

func TestDAGNode_ToFlat(t *testing.T) {
	now := time.Now()
	node := &DAGNode{
		ID:             "n1",
		Name:           "task1",
		Task:           "do stuff",
		Status:         NodeCompleted,
		Result:         "result text",
		Error:          "",
		Dependencies:   []string{"n0"},
		Review:         true,
		RetryCount:     2,
		IterationCount: 3,
		StartedAt:      &now,
		CompletedAt:    &now,
	}
	flat := node.ToFlat()
	if flat.ID != node.ID || flat.Name != node.Name || flat.Task != node.Task {
		t.Error("basic fields not copied")
	}
	if flat.Status != node.Status {
		t.Error("status not copied")
	}
	if flat.Result != node.Result {
		t.Error("result not copied")
	}
	if len(flat.Dependencies) != 1 || flat.Dependencies[0] != "n0" {
		t.Error("dependencies not copied")
	}
	if flat.RetryCount != 2 || flat.IterationCount != 3 {
		t.Error("counters not copied")
	}
	if !flat.Review {
		t.Error("review flag not copied")
	}
}

// --- executor.go: parseReviewResult ---

func TestParseReviewResult_Passed(t *testing.T) {
	result, usedHeuristic := parseReviewResult(`{"passed": true}`)
	if usedHeuristic {
		t.Error("valid JSON should not use heuristic fallback")
	}
	if !result.Passed {
		t.Error("expected passed=true")
	}
}

func TestParseReviewResult_NotPassedWithFeedback(t *testing.T) {
	result, usedHeuristic := parseReviewResult(`{"passed": false, "feedback": "fix the typo"}`)
	if usedHeuristic {
		t.Error("valid JSON should not use heuristic fallback")
	}
	if result.Passed {
		t.Error("expected passed=false")
	}
	if result.Feedback != "fix the typo" {
		t.Errorf("expected feedback 'fix the typo', got %s", result.Feedback)
	}
}

func TestParseReviewResult_InvalidJSON(t *testing.T) {
	// 纯文本且无明显信号：退化为启发式判定（usedHeuristic=true），保守按不通过处理，
	// 不再错误地把"解析失败"当通过。
	result, usedHeuristic := parseReviewResult("not json at all")
	if !usedHeuristic {
		t.Error("invalid JSON should trigger heuristic fallback")
	}
	if result.Passed {
		t.Error("ambiguous text must NOT be silently treated as pass")
	}
}

func TestParseReviewResult_PlainTextFailTreatedAsFail(t *testing.T) {
	// 复现线上 bug：Review 子代理直接返回纯文本结论（含 "fail" 与"缺少源代码"），
	// 解析失败后旧逻辑误判为通过。修复后必须识别为不通过，触发重跑/收敛。
	raw := "fail\n\n待审查的产物并未完成对 `models/`, `db/`, `config/` 目录下代码的审查与修复。" +
		"产物仅说明了由于缺少源代码无法执行任务，并请求提供源码，未提供任何实际的修复代码，因此未能满足原始任务需求。"
	result, usedHeuristic := parseReviewResult(raw)
	if !usedHeuristic {
		t.Error("plain text must trigger heuristic fallback")
	}
	if result.Passed {
		t.Fatal("plain-text 'fail' conclusion must be treated as NOT passed")
	}
	if result.Feedback == "" {
		t.Error("heuristic fail should carry the raw text as feedback")
	}
}

func TestParseReviewResult_JSONInMarkdown(t *testing.T) {
	// Some LLMs wrap JSON in markdown code blocks
	raw := "```json\n{\"passed\": true}\n```"
	result, usedHeuristic := parseReviewResult(raw)
	if usedHeuristic {
		t.Error("markdown-wrapped JSON should be extracted without heuristic")
	}
	if !result.Passed {
		t.Error("expected passed=true")
	}
}

// TestParseReviewResult_CodeSnippetNotTreatedAsVerdict 防回归：
// 审查报告正文里的代码片段（合法 JSON 但无 passed 字段）绝不能被当成审查结论。
//
// 线上事故（2026-08-04 定位）：ExtractJSON 从第一个 `{` 起括号配平，审查报告常含
// 代码片段；encoding/json 对未知字段宽容，于是 `{"timeout":5000}` 会「解析成功」
// 并得到 Passed=false（bool 零值），旧代码认为这是可信 JSON、连 WARN 都不打，
// 静默把产物判为不通过。必须先探测 passed 字段是否真实存在。
func TestParseReviewResult_CodeSnippetNotTreatedAsVerdict(t *testing.T) {
	raw := "# 审查结论：PASS\n\n代码中的配置对象如下：\n\n```json\n{ \"timeout\": 5000, \"retries\": 3 }\n```\n\n全部检查项均满足。"
	result, usedHeuristic := parseReviewResult(raw)
	if !usedHeuristic {
		t.Fatal("JSON without a 'passed' field must NOT be accepted as a verdict")
	}
	if result.Source == ReviewSourceJSON {
		t.Errorf("source must not be json, got %s", result.Source)
	}
	if !result.Passed {
		t.Error("explicit '审查结论：PASS' must be recognized as passed")
	}
}

// TestParseReviewResult_VerdictLine 防回归：模型的标准输出格式必须被正确识别。
//
// 线上事故：passSignals 缺少裸词 "pass"（failSignals 却有裸词 "fail"），
// 而模型稳定输出「审查结论：PASS ✅」→ 两侧信号同时为 0，落 default 判FAIL。
// 53 条线上样本中 6 条模型判 PASS 的被误杀 5 条。
func TestParseReviewResult_VerdictLine(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"中文PASS带emoji", "# 审查结论：PASS ✅\n\n我以最高标准通审了提交的修复方案。", true},
		{"markdown强调PASS", "# 审查报告\n\n## 判定结果：**PASS**\n\n5 条标准全部满足。", true},
		{"最终结论PASS", "# 审查报告 v3\n\n## 最终结论：✅ PASS\n\n可以放行。", true},
		{"PASS附非阻断", "# 审查结论：**PASS（附 3 项非阻断观察）**\n\n按 5 条判据逐项核查，全部满足。", true},
		{"三级标题PASS", "### 审查结果：✅ **PASS**\n\n审查员已对交付的4 份源码进行了逐行核查。", true},
		{"中文FAIL", "## 审查结论：❌ FAIL\n\n### 根本原因：待审查产物不存在", false},
		{"小写fail", "## 审查结论：**fail**\n\n### 核心问题：产物未交付任何可审查的内容", false},
		{"结论不通过", "## 审查结论：不通过\n\n存在多处逻辑缺陷。", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, _ := parseReviewResult(c.raw)
			if result.Passed != c.want {
				t.Errorf("verdict = %v, want %v (source=%s)", result.Passed, c.want, result.Source)
			}
			if result.Source != ReviewSourceVerdictLine {
				t.Errorf("source = %s, want verdict_line", result.Source)
			}
		})
	}
}

// TestParseReviewResult_JSONOnLastLine 防回归：新 prompt 要求模型「先分析、
// 最后一行输出 JSON」，解析必须优先取末行，否则正文里的代码花括号会先被抓到。
func TestParseReviewResult_JSONOnLastLine(t *testing.T) {
	raw := "## 审查分析\n\n配置片段 `{\"retries\": 3}` 看起来没问题。\n逐项核查全部满足。\n\n{\"passed\": true, \"notes\": \"命名可以更统一\"}"
	result, usedHeuristic := parseReviewResult(raw)
	if usedHeuristic {
		t.Fatalf("trailing JSON must be parsed as authoritative verdict, source=%s", result.Source)
	}
	if !result.Passed {
		t.Error("expected passed=true from trailing JSON")
	}
}

// TestNormalizeVerdictText_NegationNotMiscounted 防回归：含否定前缀的短语
// 必须先被规范化，否则「不通过」里的「通过」会同时点亮 pass 信号，
// 「没有问题」里的「有问题」会点亮 fail 信号。
func TestNormalizeVerdictText_NegationNotMiscounted(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"没有问题应判通过", "产物符合要求，代码没有问题，已验收。", true},
		{"存在问题应判不通过", "产物存在问题，缺少错误处理。", false},
		{"failure-free不应误判", "The build is failure-free and satisfies 符合要求.", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := heuristicReviewVerdict(c.raw)
			if got != c.want {
				t.Errorf("heuristicReviewVerdict = %v, want %v", got, c.want)
			}
		})
	}
}

// --- executor.go: buildIterationTask ---

func TestBuildIterationTask(t *testing.T) {
	task := buildIterationTask("original task", "previous result", "fix this")
	if !strings.Contains(task, "original task") {
		t.Error("should contain original task")
	}
	if !strings.Contains(task, "previous result") {
		t.Error("should contain previous result")
	}
	if !strings.Contains(task, "fix this") {
		t.Error("should contain feedback")
	}
}

// --- executor.go: buildReviewSystemPrompt ---

func TestBuildReviewSystemPrompt_CustomPrompt(t *testing.T) {
	result := buildReviewSystemPrompt("custom review prompt")
	if result != "custom review prompt" {
		t.Errorf("expected custom prompt, got %s", result)
	}
}

func TestBuildReviewSystemPrompt_Default(t *testing.T) {
	result := buildReviewSystemPrompt("")
	if !strings.Contains(result, "reviewer") {
		t.Error("default prompt should contain review instructions")
	}
}

// --- executor.go: buildReviewTask ---

func TestBuildReviewTask(t *testing.T) {
	node := &DAGNode{ID: "n1", Name: "test node", Task: "do something"}
	result := buildReviewTask(node, "product text")
	if !strings.Contains(result, "do something") {
		t.Error("should contain task")
	}
	if !strings.Contains(result, "test node") {
		t.Error("should contain node name")
	}
	if !strings.Contains(result, "product text") {
		t.Error("should contain product")
	}
}

// --- scheduler.go: computeFinalStatus ---

func TestComputeFinalStatus_AllCompleted(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	wf.Nodes[1].Status = NodeCompleted

	s := &Scheduler{wf: wf, logger: noopLogger()}
	if status := s.computeFinalStatus(); status != WorkflowCompleted {
		t.Errorf("expected completed, got %s", status)
	}
}

func TestComputeFinalStatus_HasFailed(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	wf.Nodes[1].Status = NodeFailed

	s := &Scheduler{wf: wf, logger: noopLogger()}
	if status := s.computeFinalStatus(); status != WorkflowFailed {
		t.Errorf("expected failed, got %s", status)
	}
}

func TestComputeFinalStatus_AllCompletedOrSkipped(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	wf.Nodes[1].Status = NodeSkipped

	s := &Scheduler{wf: wf, logger: noopLogger()}
	if status := s.computeFinalStatus(); status != WorkflowCompleted {
		t.Errorf("expected completed (skipped is ok), got %s", status)
	}
}

func TestComputeFinalStatus_Terminated(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted

	s := &Scheduler{wf: wf, logger: noopLogger(), terminated: true}
	if status := s.computeFinalStatus(); status != WorkflowTerminated {
		t.Errorf("expected terminated, got %s", status)
	}
}

func TestComputeFinalStatus_NotAllTerminal(t *testing.T) {
	// This shouldn't normally happen at end of Run(), but the function should handle it
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeCompleted
	wf.Nodes[1].Status = NodePending // still running somehow

	s := &Scheduler{wf: wf, logger: noopLogger()}
	if status := s.computeFinalStatus(); status != WorkflowFailed {
		t.Errorf("expected failed for non-terminal, got %s", status)
	}
}

// --- scheduler.go: handleTerminate ---

func TestHandleTerminate_SkipsNonTerminal(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2"},
		{ID: "n3"},
	})
	wf.RebuildIndex()
	wf.Nodes[0].Status = NodeRunning
	wf.Nodes[1].Status = NodePending
	wf.Nodes[2].Status = NodeCompleted // already terminal, should not be touched

	s := &Scheduler{wf: wf, logger: noopLogger(), metrics: &ManagerMetrics{}}
	s.handleTerminate()

	if wf.Nodes[0].Status != NodeSkipped {
		t.Errorf("n1 should be skipped, got %s", wf.Nodes[0].Status)
	}
	if wf.Nodes[1].Status != NodeSkipped {
		t.Errorf("n2 should be skipped, got %s", wf.Nodes[1].Status)
	}
	if wf.Nodes[2].Status != NodeCompleted {
		t.Errorf("n3 should remain completed, got %s", wf.Nodes[2].Status)
	}
	if wf.Nodes[0].Error == "" {
		t.Error("skipped node should have error message")
	}
}

// --- scheduler.go: Terminate ---

func TestTerminate(t *testing.T) {
	s := &Scheduler{
		terminate: make(chan struct{}),
		metrics:   &ManagerMetrics{},
		logger:    noopLogger(),
	}
	if s.isTerminated() {
		t.Error("should not be terminated initially")
	}
	s.Terminate()
	if !s.isTerminated() {
		t.Error("should be terminated after Terminate()")
	}
	// Channel should be closed
	select {
	case <-s.terminate:
		// OK
	default:
		t.Error("terminate channel should be closed")
	}
	// Double terminate should not panic
	s.Terminate()
}

// --- scheduler.go: RequestRetry ---

func TestRequestRetry_Success(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}, Status: NodeSkipped},
	})
	wf.RebuildIndex()
	n1, _ := wf.GetNode("n1")
	n1.Status = NodeFailed

	s := &Scheduler{wf: wf, logger: noopLogger()}
	err := s.RequestRetry("n1")
	if err != nil {
		t.Fatalf("RequestRetry failed: %v", err)
	}

	if n1.Status != NodePending {
		t.Errorf("n1 should be pending, got %s", n1.Status)
	}
	if n1.Error != "" {
		t.Error("n1 error should be cleared")
	}
	if n1.Result != "" {
		t.Error("n1 result should be cleared")
	}
	if n1.RetryCount != 0 {
		t.Error("n1 retry count should be reset")
	}

	// n2 should be unskipped
	n2, _ := wf.GetNode("n2")
	if n2.Status != NodePending {
		t.Errorf("n2 should be pending after unskip, got %s", n2.Status)
	}
}

func TestRequestRetry_NodeNotFound(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{{ID: "n1"}})
	wf.RebuildIndex()
	s := &Scheduler{wf: wf, logger: noopLogger()}
	err := s.RequestRetry("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent node")
	}
}

func TestRequestRetry_WrongStatus(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{{ID: "n1"}})
	wf.RebuildIndex()
	n1, _ := wf.GetNode("n1")
	n1.Status = NodeCompleted

	s := &Scheduler{wf: wf, logger: noopLogger()}
	err := s.RequestRetry("n1")
	if err == nil {
		t.Error("expected error for completed node")
	}
}

// --- scheduler.go: SubmitRetry / drainRetryRequests ---

func TestSubmitRetry_AndDrain(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1"},
		{ID: "n2", Dependencies: []string{"n1"}, Status: NodeSkipped},
	})
	wf.RebuildIndex()
	n1, _ := wf.GetNode("n1")
	n1.Status = NodeFailed

	s := &Scheduler{
		wf:            wf,
		logger:        noopLogger(),
		retryRequests: make(chan string, 16),
	}

	// Submit retry
	s.SubmitRetry("n1")

	// Drain should process the request
	s.drainRetryRequests()

	// n1 should now be pending
	if n1.Status != NodePending {
		t.Errorf("n1 should be pending after drain, got %s", n1.Status)
	}
}

func TestSubmitRetry_ChannelFull(t *testing.T) {
	s := &Scheduler{
		logger:        noopLogger(),
		retryRequests: make(chan string, 1), // capacity 1
	}
	s.SubmitRetry("a")
	s.SubmitRetry("b") // should be dropped (channel full), no panic
}

// --- scheduler.go: String ---

func TestScheduler_String(t *testing.T) {
	wf := NewWorkflow("wf-123", "req", nil)
	s := &Scheduler{wf: wf, maxParallel: 5}
	result := s.String()
	if !strings.Contains(result, "wf-123") {
		t.Errorf("String should contain workflow ID: %s", result)
	}
	if !strings.Contains(result, "5") {
		t.Errorf("String should contain parallel count: %s", result)
	}
}

// --- scheduler.go: isTerminated ---

func TestIsTerminated(t *testing.T) {
	s := &Scheduler{logger: noopLogger(), terminate: make(chan struct{})}
	if s.isTerminated() {
		t.Error("should not be terminated")
	}
	s.terminated = true
	if !s.isTerminated() {
		t.Error("should be terminated")
	}
}

// --- scheduler.go: emitNodeEvent ---

func TestEmitNodeEvent(t *testing.T) {
	wf := NewWorkflow("wf-emit", "req", []*DAGNode{{ID: "n1"}})
	wf.RebuildIndex()
	bus := &captureBus{}
	emitter := outbound.NewEventEmitter(bus, "")
	s := &Scheduler{wf: wf, emitter: emitter, logger: noopLogger()}

	s.emitNodeEvent(context.Background(), outbound.EventWorkflowNodeStarted, map[string]any{
		"node_id": "n1",
	})

	if !bus.hasEvent(t, outbound.EventWorkflowNodeStarted) {
		t.Error("expected node started event")
	}
}

// --- scheduler.go: emitCascadeSkipEvent ---

func TestEmitCascadeSkipEvent(t *testing.T) {
	wf := NewWorkflow("wf", "req", nil)
	wf.RebuildIndex()
	bus := &captureBus{}
	emitter := outbound.NewEventEmitter(bus, "")
	s := &Scheduler{wf: wf, emitter: emitter, logger: noopLogger()}

	// With skipped nodes
	s.emitCascadeSkipEvent(context.Background(), "n1", []string{"n2", "n3"})
	if !bus.hasEvent(t, outbound.EventWorkflowNodeSkipped) {
		t.Error("expected node skipped event")
	}

	// Without skipped nodes — should not emit
	bus2 := &captureBus{}
	emitter2 := outbound.NewEventEmitter(bus2, "")
	s2 := &Scheduler{wf: wf, emitter: emitter2, logger: noopLogger()}
	s2.emitCascadeSkipEvent(context.Background(), "n1", nil)
	if bus2.hasEvent(t, outbound.EventWorkflowNodeSkipped) {
		t.Error("should not emit event when no nodes skipped")
	}
}
