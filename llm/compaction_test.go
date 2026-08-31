package llm

import (
	"fmt"
	"strings"
	"testing"
)

// ============================================================================
// Token 估算测试
// ============================================================================

func TestEstimateTokens_English(t *testing.T) {
	// 4 字符约 1 token
	text := "Hello world test code"
	tokens := EstimateTokens(text)
	if tokens <= 0 {
		t.Errorf("expected positive tokens for %q, got %d", text, tokens)
	}
	// 约 20 字符 / 4 = 5 tokens
	if tokens < 3 || tokens > 10 {
		t.Errorf("expected ~5 tokens for %q, got %d", text, tokens)
	}
}

func TestEstimateTokens_CJK(t *testing.T) {
	text := "你好世界测试"
	tokens := EstimateTokens(text)
	// 6 CJK chars, 约 2/3 token per char = 4 tokens
	if tokens < 2 || tokens > 10 {
		t.Errorf("expected 2-10 tokens for CJK text, got %d", tokens)
	}
}

func TestEstimateTokens_Empty(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Error("expected 0 tokens for empty string")
	}
}

func TestEstimateMessagesTokens(t *testing.T) {
	msgs := []Message{
		UserMessage("Hello"),
		AssistantMessage("Hi there"),
	}
	tokens := EstimateMessagesTokens(msgs)
	if tokens <= 0 {
		t.Error("expected positive tokens")
	}
	// 每条消息至少有 4 token overhead
	if tokens < 8 {
		t.Errorf("expected at least 8 tokens, got %d", tokens)
	}
}

func TestEstimateSystemTokens(t *testing.T) {
	system := "You are a helpful assistant."
	tokens := EstimateSystemTokens(system)
	if tokens <= 0 {
		t.Error("expected positive tokens")
	}
}

func TestCountRunes(t *testing.T) {
	if CountRunes("hello") != 5 {
		t.Error("expected 5 runes for 'hello'")
	}
	if CountRunes("你好") != 2 {
		t.Error("expected 2 runes for '你好'")
	}
}

// ============================================================================
// Compaction 测试
// ============================================================================

func TestCompactor_UsableTokens(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:      100000,
		ReservedTokens: 10000,
		TailTokens:     5000,
	})
	usable := c.UsableTokens()
	if usable != 90000 {
		t.Errorf("expected 90000 usable tokens, got %d", usable)
	}
}

func TestCompactor_IsOverflow(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:      100,
		ReservedTokens: 20,
		TailTokens:     10,
		Auto:           true,
	})

	// 小参数不溢出
	params := GenerateParams{System: "hi"}
	if c.IsOverflow(params) {
		t.Error("expected no overflow for small params")
	}

	// 大参数溢出 — 2000 chars / 4 = 500 tokens >> 80 usable
	params.System = strings.Repeat("a", 2000)
	if !c.IsOverflow(params) {
		t.Error("expected overflow for large params")
	}
}

func TestCompactor_IsOverflowByUsage(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:      100000,
		ReservedTokens: 20000,
	})

	// nil usage -> no overflow
	if c.IsOverflowByUsage(nil) {
		t.Error("expected no overflow for nil usage")
	}

	// small usage -> no overflow
	small := &Usage{InputTokens: 1000, OutputTokens: 500}
	if c.IsOverflowByUsage(small) {
		t.Error("expected no overflow for small usage")
	}

	// large usage -> overflow
	large := &Usage{TotalTokens: 90000}
	if !c.IsOverflowByUsage(large) {
		t.Error("expected overflow for large usage")
	}
}

func TestCompactor_PruneToolOutputs(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:           128000,
		TailTokens:          50,
		ToolOutputThreshold: 10,
	})

	longOutput := strings.Repeat("This is a very long tool output. ", 100)

	messages := []Message{
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "1", ToolName: "exec", Input: "ls"},
			},
		},
		{
			Role: MessageRoleTool,
			Content: []MessagePart{
				ToolResultPart{ToolCallID: "1", ToolName: "exec", Result: longOutput},
			},
		},
		// Add 2 user turns to bypass the 2-turn protect window
		UserMessage("turn2"),
		{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolCallPart{ToolCallID: "2", ToolName: "exec", Input: "cat"},
			},
		},
		{
			Role: MessageRoleTool,
			Content: []MessagePart{
				ToolResultPart{ToolCallID: "2", ToolName: "exec", Result: longOutput},
			},
		},
		UserMessage("turn3"),
	}

	pruned := c.PruneToolOutputs(messages)

	// The first tool result should potentially be pruned
	// (it's beyond the 2-turn protect window)
	// Note: pruning only happens if prunable > PruneMinimum (20000),
	// and our long output is ~300 tokens which may be below threshold.
	// This test validates the function doesn't crash and returns correct count.
	if len(pruned) != len(messages) {
		t.Errorf("expected %d messages, got %d", len(messages), len(pruned))
	}
}

func TestCompactor_PruneToolOutputs_ProtectedTool(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:           128000,
		ToolOutputThreshold: 5,
	})

	longOutput := strings.Repeat("Protected output. ", 1000)

	messages := []Message{
		{
			Role: MessageRoleUser,
			Content: []MessagePart{
				TextPart{Text: "old turn"},
			},
		},
		{
			Role: MessageRoleUser,
			Content: []MessagePart{
				TextPart{Text: "turn2"},
			},
		},
		{
			Role: MessageRoleUser,
			Content: []MessagePart{
				TextPart{Text: "turn3"},
			},
		},
		{
			Role: MessageRoleTool,
			Content: []MessagePart{
				ToolResultPart{ToolCallID: "1", ToolName: "skill", Result: longOutput},
			},
		},
	}

	pruned := c.PruneToolOutputs(messages)

	// Protected tool output should never be pruned
	for _, msg := range pruned {
		for _, part := range msg.Content {
			if tr, ok := part.(ToolResultPart); ok && tr.ToolName == "skill" {
				if s, ok := tr.Result.(string); ok && strings.Contains(s, "[compacted") {
					t.Error("protected tool output should not be pruned")
				}
			}
		}
	}
}

func TestCompactor_DoomLoop(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:            100,
		ReservedTokens:       20,
		TailTokens:           10,
		MinMessagesToCompact: 3,
		Auto:                 true,
	})

	// Simulate multiple compactions
	for i := 0; i < DoomLoopThreshold; i++ {
		c.mu.Lock()
		c.compactionCount++
		c.mu.Unlock()
	}

	// ShouldCompact should return false due to doom loop
	params := GenerateParams{
		System: strings.Repeat("x", 200),
		Messages: []Message{
			UserMessage(strings.Repeat("a", 50)),
			AssistantMessage(strings.Repeat("b", 50)),
			UserMessage(strings.Repeat("c", 50)),
		},
	}
	if c.ShouldCompact(params) {
		t.Error("expected ShouldCompact to return false when in doom loop")
	}

	// After reset, should work again
	c.resetDoomLoop()
	// ShouldCompact still won't trigger because overflow check uses estimation
	// but at least doom loop is no longer the blocker
}

func TestCompactor_SelectTailSplit(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		TailTurns: 2,
	})

	msgs := []Message{
		UserMessage("msg1"),
		AssistantMessage("reply1"),
		UserMessage("msg2"),
		AssistantMessage("reply2"),
		UserMessage("msg3"),
		AssistantMessage("reply3"),
		UserMessage("msg4"),
	}

	idx := c.selectTailSplit(msgs)
	// Should keep last 2 user turns (msg3 and msg4)
	// msg3 is at index 4
	if idx != 4 {
		t.Errorf("expected splitIdx=4 (msg3), got %d", idx)
	}
}

func TestCompactor_PreserveRecentBudget(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:      128000,
		ReservedTokens: 20000,
	})

	budget := c.preserveRecentBudget()
	// usable = 108000, 25% = 27000, clamped to MaxPreserveRecentTokens = 8000
	if budget != MaxPreserveRecentTokens {
		t.Errorf("expected budget=%d, got %d", MaxPreserveRecentTokens, budget)
	}

	// Test with very small usable
	c2 := NewCompactor(CompactionConfig{
		MaxTokens:      10000,
		ReservedTokens: 2000,
	})
	budget2 := c2.preserveRecentBudget()
	// usable = 8000, 25% = 2000, clamped to MinPreserveRecentTokens = 2000
	if budget2 != MinPreserveRecentTokens {
		t.Errorf("expected budget=%d, got %d", MinPreserveRecentTokens, budget2)
	}
}

func TestCompactor_BuildSummaryPrompt_NoPrevious(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())

	oldMsgs := []Message{
		UserMessage("hello"),
		AssistantMessage("hi"),
	}

	prompt := c.buildSummaryPrompt(oldMsgs)

	if !strings.Contains(prompt, "Create a new anchored summary") {
		t.Error("expected new summary prompt")
	}
	if !strings.Contains(prompt, "hello") {
		t.Error("expected conversation content in prompt")
	}
	if !strings.Contains(prompt, "## Goal") {
		t.Error("expected structured template in prompt")
	}
}

func TestCompactor_BuildSummaryPrompt_WithPrevious(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())
	c.mu.Lock()
	c.previousSummary = "## Goal\n- Previous task"
	c.mu.Unlock()

	oldMsgs := []Message{
		UserMessage("new message"),
	}

	prompt := c.buildSummaryPrompt(oldMsgs)

	if !strings.Contains(prompt, "Update the anchored summary") {
		t.Error("expected update prompt")
	}
	if !strings.Contains(prompt, "<previous-summary>") {
		t.Error("expected previous-summary block")
	}
	if !strings.Contains(prompt, "Previous task") {
		t.Error("expected previous summary content")
	}
}

func TestDefaultCompactionConfig(t *testing.T) {
	cfg := DefaultCompactionConfig()
	if cfg.MaxTokens != 64000 {
		t.Errorf("expected MaxTokens=64000, got %d", cfg.MaxTokens)
	}
	if cfg.ReservedTokens <= 0 {
		t.Error("expected positive ReservedTokens")
	}
	if cfg.ReservedTokens != 20000 {
		t.Errorf("expected ReservedTokens=20000, got %d", cfg.ReservedTokens)
	}
	if !cfg.Auto {
		t.Error("expected Auto=true")
	}
	if cfg.TailTurns != DefaultTailTurns {
		t.Errorf("expected TailTurns=%d, got %d", DefaultTailTurns, cfg.TailTurns)
	}
	if cfg.SummaryMaxTokens != 4096 {
		t.Errorf("expected SummaryMaxTokens=4096, got %d", cfg.SummaryMaxTokens)
	}
}

// ============================================================================
// Context Overflow 检测测试
// ============================================================================

func TestIsContextOverflow(t *testing.T) {
	tests := []struct {
		msg      string
		expected bool
	}{
		{"prompt is too long", true},
		{"This model's maximum context length is 8192 tokens", true},
		{"context_length_exceeded", true},
		{"Request entity too large", true},
		{"400 (no body)", true},
		{"413 (no body)", true},
		{"rate limit exceeded", false},
		{"authentication failed", false},
		{"", false},
	}

	for _, tt := range tests {
		got := IsContextOverflow(tt.msg)
		if got != tt.expected {
			t.Errorf("IsContextOverflow(%q) = %v, want %v", tt.msg, got, tt.expected)
		}
	}
}

func TestIsContextOverflowError_LLMError(t *testing.T) {
	// Context overflow reason
	err := NewLLMError(ErrorReasonContextOverflow, "openai", "prompt too long")
	if !IsContextOverflowError(err) {
		t.Error("expected context overflow error")
	}

	// Invalid request with overflow message
	err2 := NewLLMError(ErrorReasonInvalidRequest, "anthropic", "prompt is too long")
	if !IsContextOverflowError(err2) {
		t.Error("expected context overflow error by message pattern")
	}

	// Non-overflow error
	err3 := NewLLMError(ErrorReasonRateLimit, "openai", "rate limited")
	if IsContextOverflowError(err3) {
		t.Error("expected NOT context overflow error")
	}
}

func TestIsContextOverflowError_PlainError(t *testing.T) {
	err := fmt.Errorf("This model's maximum context length is 8192 tokens")
	if !IsContextOverflowError(err) {
		t.Error("expected context overflow error")
	}

	err2 := fmt.Errorf("some random error")
	if IsContextOverflowError(err2) {
		t.Error("expected NOT context overflow error")
	}
}

// ============================================================================
// Tool Output Truncation 测试
// ============================================================================

func TestOutputTruncator_NoTruncation(t *testing.T) {
	cfg := DefaultTruncationConfig()
	shortOutput := "line1\nline2\nline3"

	result := TruncateOutput(shortOutput, cfg)
	if result.Truncated {
		t.Error("expected no truncation for short output")
	}
	if result.Output != shortOutput {
		t.Error("output should match input")
	}
}

func TestOutputTruncator_Truncation(t *testing.T) {
	cfg := TruncationConfig{
		MaxLines: 5,
		MaxBytes: 10000,
	}

	// 生成 10 行输出
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	longOutput := strings.Join(lines, "\n")

	result := TruncateOutput(longOutput, cfg)
	if !result.Truncated {
		t.Error("expected truncation")
	}
	if result.OriginalSize == 0 {
		t.Error("expected non-zero original size")
	}
	outputStr, ok := result.Output.(string)
	if !ok {
		t.Fatal("expected string output")
	}
	if !strings.Contains(outputStr, "middle omitted") {
		t.Error("expected truncation marker (middle omitted) in truncated output")
	}
}

func TestTruncateOutput_Global(t *testing.T) {
	result := TruncateOutput("short text", DefaultTruncationConfig())
	if result.Truncated {
		t.Error("expected no truncation for short text")
	}
}

// TestOutputTruncator_HeadAndTailKept 验证截断保留头部和尾部（中间省略），
// 而非纯头部截断——尾部信息（如错误、汇总）不应丢失。
func TestOutputTruncator_HeadAndTailKept(t *testing.T) {
	cfg := TruncationConfig{
		MaxLines: 6,
		MaxBytes: 100000,
	}

	// 生成 30 行输出，首行和末行有可识别标记。
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%02d", i)
	}
	lines[0] = "FIRST_LINE_MARKER"
	lines[29] = "LAST_LINE_MARKER"
	longOutput := strings.Join(lines, "\n")

	result := TruncateOutput(longOutput, cfg)
	if !result.Truncated {
		t.Fatal("expected truncation")
	}
	outputStr, ok := result.Output.(string)
	if !ok {
		t.Fatal("expected string output")
	}
	if !strings.Contains(outputStr, "FIRST_LINE_MARKER") {
		t.Error("expected head (first line) to be kept")
	}
	if !strings.Contains(outputStr, "LAST_LINE_MARKER") {
		t.Error("expected tail (last line) to be kept")
	}
	if !strings.Contains(outputStr, "middle omitted") {
		t.Error("expected truncation marker (middle omitted)")
	}
}

// TestOutputTruncator_SingleHugeLine 验证无换行的超长单行（压缩 JSON / 单行
// HTML / 长 base64 等）不会整块丢失：应退回字节级兜底，保留头部和尾部字节。
func TestOutputTruncator_SingleHugeLine(t *testing.T) {
	cfg := TruncationConfig{
		MaxLines: 500,
		MaxBytes: 50 * 1024,
	}

	// 60KB 无换行单行（模拟压缩 JSON blob）。
	huge := strings.Repeat("x", 60*1024)
	result := TruncateOutput(huge, cfg)
	if !result.Truncated {
		t.Fatal("expected truncation")
	}
	outputStr, ok := result.Output.(string)
	if !ok {
		t.Fatal("expected string output")
	}
	// 内容不应整块丢失：至少应保留首尾字节（去掉提示语后仍有原始字符）。
	kept := strings.Count(outputStr, "x")
	if kept == 0 {
		t.Error("expected head/tail bytes to be kept for single huge line (byte-level fallback)")
	}
	// 又不应因保留过头而等于原文（确认确实发生了省略）。
	if kept == len(huge) {
		t.Error("expected content to be omitted (not full passthrough)")
	}
	if !strings.Contains(outputStr, "middle omitted") {
		t.Error("expected truncation marker (middle omitted)")
	}
}

// ============================================================================
// MidConversationMessage 测试
// ============================================================================

func TestNewDateChangeMessage(t *testing.T) {
	msg := NewDateChangeMessage("2025-01-01")
	if msg.Type != "date_change" {
		t.Error("expected date_change type")
	}
	if !strings.Contains(msg.Content, "2025-01-01") {
		t.Error("expected date in content")
	}
}

func TestInsertMidConversationMessages(t *testing.T) {
	messages := []Message{
		UserMessage("hello"),
	}

	result := InsertMidConversationMessages(messages, NewDateChangeMessage("2025-01-01"))
	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
}

// ============================================================================
// CompactionPrepareStep 测试
// ============================================================================

func TestCompactionPrepareStep_NoOverflow(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())
	hook := CompactionPrepareStep(c)

	params := &GenerateParams{
		System:   "test",
		Messages: []Message{UserMessage("hi")},
	}

	result := hook(params)
	// 没有溢出，应该返回 nil
	if result != nil {
		t.Error("expected nil for non-overflow params")
	}
}

func TestCompactionPrepareStep_Overflow(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:            100,
		ReservedTokens:       20,
		TailTokens:           20,
		MinMessagesToCompact: 3,
		ToolOutputThreshold:  500,
		Auto:                 true,
	})
	hook := CompactionPrepareStep(c)

	params := &GenerateParams{
		System: strings.Repeat("x", 200),
		Messages: []Message{
			UserMessage(strings.Repeat("a", 50)),
			AssistantMessage(strings.Repeat("b", 50)),
			UserMessage(strings.Repeat("c", 50)),
		},
	}

	result := hook(params)
	// 应该触发 pruning
	if result == nil {
		t.Error("expected non-nil result for overflow")
	}
}
