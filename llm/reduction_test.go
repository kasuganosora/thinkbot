package llm

import (
	"strings"
	"testing"
)

// --- Phase 1: TruncateToolResults ---

func TestTruncateToolResults_NoTruncation(t *testing.T) {
	cfg := ReductionConfig{MaxOutputTokens: 7500}
	results := []ToolResultPart{
		{ToolCallID: "1", ToolName: "search", Result: "short result"},
		{ToolCallID: "2", ToolName: "read", Result: "another short result"},
	}
	out := TruncateToolResults(results, cfg)
	if out[0].Result != "short result" {
		t.Errorf("expected unchanged, got %v", out[0].Result)
	}
	if out[1].Result != "another short result" {
		t.Errorf("expected unchanged, got %v", out[1].Result)
	}
}

func TestTruncateToolResults_LargeOutput(t *testing.T) {
	cfg := ReductionConfig{MaxOutputTokens: 100}     // low threshold to trigger
	largeText := strings.Repeat("hello world ", 500) // ~6000 chars ~1500 tokens
	results := []ToolResultPart{
		{ToolCallID: "1", ToolName: "read", Result: largeText},
	}
	out := TruncateToolResults(results, cfg)

	resultStr, ok := out[0].Result.(string)
	if !ok {
		t.Fatalf("expected string result, got %T", out[0].Result)
	}
	if !strings.Contains(resultStr, "[output truncated") {
		t.Errorf("expected truncation marker, got: %s", previewString(resultStr, 200))
	}
	if !strings.Contains(resultStr, "exceeded limit of 100") {
		t.Errorf("expected threshold mention, got: %s", previewString(resultStr, 200))
	}
}

func TestTruncateToolResults_ExcludeTools(t *testing.T) {
	cfg := ReductionConfig{
		MaxOutputTokens: 10,
		ExcludeTools:    []string{"protected"},
	}
	largeText := strings.Repeat("x", 1000)
	results := []ToolResultPart{
		{ToolCallID: "1", ToolName: "protected", Result: largeText},
		{ToolCallID: "2", ToolName: "normal", Result: largeText},
	}
	out := TruncateToolResults(results, cfg)

	// Protected tool: unchanged
	if out[0].Result != largeText {
		t.Errorf("expected protected tool to be unchanged")
	}
	// Normal tool: truncated
	resultStr, _ := out[1].Result.(string)
	if !strings.Contains(resultStr, "[output truncated") {
		t.Errorf("expected normal tool to be truncated")
	}
}

func TestTruncateToolResults_Disabled(t *testing.T) {
	cfg := ReductionConfig{MaxOutputTokens: 0}
	results := []ToolResultPart{
		{ToolCallID: "1", ToolName: "t", Result: strings.Repeat("x", 10000)},
	}
	out := TruncateToolResults(results, cfg)
	if out[0].Result != results[0].Result {
		t.Errorf("expected unchanged when disabled")
	}
}

func TestTruncateToolResults_PreviewContainsOriginal(t *testing.T) {
	cfg := ReductionConfig{MaxOutputTokens: 50}
	text := "HEADER_CONTENT\n" + strings.Repeat("padding ", 200)
	results := []ToolResultPart{
		{ToolCallID: "1", ToolName: "read", Result: text},
	}
	out := TruncateToolResults(results, cfg)
	resultStr, _ := out[0].Result.(string)
	if !strings.Contains(resultStr, "HEADER_CONTENT") {
		t.Errorf("expected preview to contain start of original output")
	}
}

// --- Phase 2: ReduceHistory ---

func TestReduceHistory_BelowThreshold(t *testing.T) {
	cfg := ReductionConfig{ClearThresholdTokens: 100000}
	msgs := []Message{
		AssistantMessage("hello"),
	}
	out := ReduceHistory(msgs, cfg)
	// Should return the same slice (no compaction needed)
	if &out[0] != &msgs[0] {
		t.Errorf("expected same slice when below threshold")
	}
}

func TestReduceHistory_CompactsOldToolResults(t *testing.T) {
	// Build a message sequence with old large tool results.
	largeResult := strings.Repeat("data line\n", 2000) // ~20K chars ~5K tokens

	msgs := []Message{
		// Old step 1: should be compacted
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "step1"}}},
		ToolMessage(ToolResultPart{ToolCallID: "tc1", ToolName: "read", Result: largeResult}),
		// Old step 2: should be compacted
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "step2"}}},
		ToolMessage(ToolResultPart{ToolCallID: "tc2", ToolName: "exec", Result: largeResult}),
		// Recent step 3: retained (within RetainRecentSteps=1)
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "step3"}}},
		ToolMessage(ToolResultPart{ToolCallID: "tc3", ToolName: "read", Result: largeResult}),
	}

	cfg := ReductionConfig{
		ClearThresholdTokens: 100, // very low to trigger
		RetainRecentSteps:    1,   // only retain last step
	}

	out := ReduceHistory(msgs, cfg)
	if len(out) != len(msgs) {
		t.Fatalf("expected same message count, got %d vs %d", len(out), len(msgs))
	}

	// Check that old tool results were compacted.
	oldResult1, _ := out[1].Content[0].(ToolResultPart)
	if !strings.Contains(resultToString(oldResult1.Result), "[compacted") {
		t.Errorf("expected old tool result (idx 1) to be compacted")
	}
	oldResult2, _ := out[3].Content[0].(ToolResultPart)
	if !strings.Contains(resultToString(oldResult2.Result), "[compacted") {
		t.Errorf("expected old tool result (idx 3) to be compacted")
	}

	// Recent tool result should be unchanged.
	recentResult, _ := out[5].Content[0].(ToolResultPart)
	if resultToString(recentResult.Result) != largeResult {
		t.Errorf("expected recent tool result to be unchanged")
	}
}

func TestReduceHistory_ExcludeTools(t *testing.T) {
	largeResult := strings.Repeat("x", 5000)
	msgs := []Message{
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "old"}}},
		ToolMessage(
			ToolResultPart{ToolCallID: "1", ToolName: "protected", Result: largeResult},
			ToolResultPart{ToolCallID: "2", ToolName: "normal", Result: largeResult},
		),
		// Need enough recent steps so the old ones are eligible
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "recent1"}}},
		{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "recent2"}}},
	}

	cfg := ReductionConfig{
		ClearThresholdTokens: 10,
		RetainRecentSteps:    2,
		ExcludeTools:         []string{"protected"},
	}

	out := ReduceHistory(msgs, cfg)
	toolMsg := out[1]
	tr1 := toolMsg.Content[0].(ToolResultPart)
	tr2 := toolMsg.Content[1].(ToolResultPart)

	if resultToString(tr1.Result) != largeResult {
		t.Errorf("expected protected tool result to be unchanged")
	}
	if !strings.Contains(resultToString(tr2.Result), "[compacted") {
		t.Errorf("expected normal tool result to be compacted")
	}
}

func TestReduceHistory_Disabled(t *testing.T) {
	cfg := ReductionConfig{ClearThresholdTokens: 0}
	msgs := []Message{
		ToolMessage(ToolResultPart{ToolCallID: "1", ToolName: "t", Result: "x"}),
	}
	out := ReduceHistory(msgs, cfg)
	if &out[0] != &msgs[0] {
		t.Errorf("expected same slice when disabled")
	}
}

func TestFindRetainBoundary(t *testing.T) {
	msgs := []Message{
		{Role: MessageRoleUser},
		{Role: MessageRoleAssistant}, // idx 1: step 1
		{Role: MessageRoleTool},
		{Role: MessageRoleAssistant}, // idx 3: step 2
		{Role: MessageRoleTool},
		{Role: MessageRoleAssistant}, // idx 5: step 3
		{Role: MessageRoleAssistant}, // idx 6: step 4
	}

	// Retain last 2 steps → boundary at idx 5 (step 3)
	idx := findRetainBoundary(msgs, 2)
	if idx != 5 {
		t.Errorf("expected boundary at 5, got %d", idx)
	}

	// Retain last 4 steps → boundary at idx 1 (step 1)
	idx = findRetainBoundary(msgs, 4)
	if idx != 1 {
		t.Errorf("expected boundary at 1, got %d", idx)
	}

	// Retain more steps than exist → boundary at 0
	idx = findRetainBoundary(msgs, 10)
	if idx != 0 {
		t.Errorf("expected boundary at 0, got %d", idx)
	}
}

// --- Callback factories ---

func TestNewOnToolResultsCallback(t *testing.T) {
	cfg := ReductionConfig{MaxOutputTokens: 50}
	cb := NewOnToolResultsCallback(cfg)
	if cb == nil {
		t.Fatal("expected non-nil callback")
	}

	large := strings.Repeat("x", 1000)
	results := []ToolResultPart{
		{ToolCallID: "1", ToolName: "t", Result: large},
	}
	out := cb(0, results)
	resultStr, _ := out[0].Result.(string)
	if !strings.Contains(resultStr, "[output truncated") {
		t.Errorf("expected truncation via callback")
	}
}

func TestNewReducePrepareStepCallback(t *testing.T) {
	cfg := ReductionConfig{
		ClearThresholdTokens: 10,
		RetainRecentSteps:    1,
	}
	cb := NewReducePrepareStepCallback(cfg)
	if cb == nil {
		t.Fatal("expected non-nil callback")
	}

	large := strings.Repeat("data\n", 1000)
	params := &GenerateParams{
		Messages: []Message{
			{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "old"}}},
			ToolMessage(ToolResultPart{ToolCallID: "1", ToolName: "t", Result: large}),
			{Role: MessageRoleAssistant, Content: []MessagePart{TextPart{Text: "recent"}}},
		},
	}

	out := cb(params)
	if out == nil {
		t.Fatal("expected non-nil return")
	}
	tr := out.Messages[1].Content[0].(ToolResultPart)
	if !strings.Contains(resultToString(tr.Result), "[compacted") {
		t.Errorf("expected compaction via callback")
	}
}
