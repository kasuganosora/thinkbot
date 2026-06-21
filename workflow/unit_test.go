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
	result, err := parseReviewResult(`{"passed": true}`)
	if err != nil {
		t.Fatalf("parseReviewResult failed: %v", err)
	}
	if !result.Passed {
		t.Error("expected passed=true")
	}
}

func TestParseReviewResult_NotPassedWithFeedback(t *testing.T) {
	result, err := parseReviewResult(`{"passed": false, "feedback": "fix the typo"}`)
	if err != nil {
		t.Fatalf("parseReviewResult failed: %v", err)
	}
	if result.Passed {
		t.Error("expected passed=false")
	}
	if result.Feedback != "fix the typo" {
		t.Errorf("expected feedback 'fix the typo', got %s", result.Feedback)
	}
}

func TestParseReviewResult_InvalidJSON(t *testing.T) {
	_, err := parseReviewResult("not json at all")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseReviewResult_JSONInMarkdown(t *testing.T) {
	// Some LLMs wrap JSON in markdown code blocks
	raw := "```json\n{\"passed\": true}\n```"
	result, err := parseReviewResult(raw)
	if err != nil {
		t.Fatalf("should extract JSON from markdown: %v", err)
	}
	if !result.Passed {
		t.Error("expected passed=true")
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
	if !strings.Contains(result, "审查") {
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
