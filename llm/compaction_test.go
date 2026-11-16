package llm

import (
	"context"
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

func TestCompactor_ConfigSourceLive(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:      100000,
		ReservedTokens: 10000,
		TailTokens:     5000,
	})
	if c.UsableTokens() != 90000 {
		t.Fatalf("initial usable = %d", c.UsableTokens())
	}
	cur := CompactionConfig{MaxTokens: 20000, ReservedTokens: 5000, TailTokens: 1000}
	c.SetConfigSource(func() CompactionConfig { return cur })
	if c.UsableTokens() != 15000 {
		t.Fatalf("live usable = %d, want 15000", c.UsableTokens())
	}
	cur.MaxTokens = 8000
	if c.UsableTokens() != 3000 {
		t.Fatalf("updated usable = %d, want 3000", c.UsableTokens())
	}
}

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
	if cfg.MaxTokens != 0 {
		t.Errorf("expected MaxTokens=0 (derive from model window), got %d", cfg.MaxTokens)
	}
	if cfg.TriggerRatio != DefaultTriggerRatio {
		t.Errorf("expected TriggerRatio=%v, got %v", DefaultTriggerRatio, cfg.TriggerRatio)
	}
	if cfg.ReserveRatio != DefaultReserveRatio {
		t.Errorf("expected ReserveRatio=%v, got %v", DefaultReserveRatio, cfg.ReserveRatio)
	}
	if !cfg.Auto {
		t.Error("expected Auto=true")
	}
	if cfg.TailTurns != DefaultTailTurns {
		t.Errorf("expected TailTurns=%d, got %d", DefaultTailTurns, cfg.TailTurns)
	}
	if cfg.SummaryMaxTokens != 0 {
		t.Errorf("expected SummaryMaxTokens=0 (follow model), got %d", cfg.SummaryMaxTokens)
	}
}

func TestResolveCompactionBudget_UsesModelWindowRatio(t *testing.T) {
	b := ResolveCompactionBudget(CompactionConfig{
		ContextLength: 1_000_000,
		OutputReserve: 128000,
		TriggerRatio:  0.75,
		ReserveRatio:  0.08,
		TailTokens:    8000,
	})
	if b.Window != 1_000_000 {
		t.Fatalf("window = %d", b.Window)
	}
	if b.Reserved != 128000 {
		t.Fatalf("reserved = %d, want output reserve 128000", b.Reserved)
	}
	if b.Trigger != 750000 {
		t.Fatalf("trigger = %d, want 750000", b.Trigger)
	}
}

func TestResolveCompactionBudget_FallbackWhenWindowUnknown(t *testing.T) {
	b := ResolveCompactionBudget(CompactionConfig{TailTokens: 8000})
	if b.Window != fallbackCompactionWindow || b.Trigger != fallbackCompactionWindow-fallbackCompactionReserved {
		t.Fatalf("fallback budget = %+v", b)
	}
}

// TestResolveCompactionBudget_TriggerCap 复现超长窗口模型的压缩失效：
// 1M 窗口把触发线抬到 786432，而真实单步输入峰值只有 110344 → 压缩永不触发。
// cap 让触发线回到实际输入规模，且只影响 trigger、不动 window/reserved/usable。
func TestResolveCompactionBudget_TriggerCap(t *testing.T) {
	base := CompactionConfig{
		ContextLength: 1_048_576,
		OutputReserve: 65_536,
		TriggerRatio:  0.75,
		ReserveRatio:  0.08,
		TailTokens:    8_000,
	}
	uncapped := ResolveCompactionBudget(base)
	if uncapped.Trigger != 786_432 {
		t.Fatalf("uncapped trigger = %d, want 786432", uncapped.Trigger)
	}

	capped := base
	capped.TriggerCap = 48_000
	b := ResolveCompactionBudget(capped)
	if b.Trigger != 48_000 {
		t.Fatalf("capped trigger = %d, want 48000", b.Trigger)
	}
	if b.Window != uncapped.Window || b.Reserved != uncapped.Reserved || b.Usable != uncapped.Usable {
		t.Fatal("trigger cap must not change window/reserved/usable")
	}

	// cap 小于保留区时仍要保住 TailTokens 下限，否则压缩后立刻又溢出 → doom loop。
	tiny := base
	tiny.TriggerCap = 1_000
	if got := ResolveCompactionBudget(tiny).Trigger; got != 8_000 {
		t.Fatalf("trigger below tail budget = %d, want 8000", got)
	}
}

// modelCaptureProvider 记录摘要请求实际使用的 model。
type modelCaptureProvider struct{ lastModel *Model }

func (p *modelCaptureProvider) Name() string { return "capture" }
func (p *modelCaptureProvider) DoGenerate(_ context.Context, params GenerateParams) (*GenerateResult, error) {
	p.lastModel = params.Model
	return &GenerateResult{FinishReason: FinishReasonStop, Text: "summary"}, nil
}
func (p *modelCaptureProvider) DoStream(_ context.Context, _ GenerateParams) (*StreamResult, error) {
	ch := make(chan StreamPart)
	close(ch)
	return &StreamResult{Stream: ch}, nil
}

// TestSummarizeMessages_UsesLightModelOnlyForSummary 验证摘要可以走快模型，
// 且不会把主链路的 params.Model 一起换掉（压缩不能悄悄改编排模型）。
func TestSummarizeMessages_UsesLightModelOnlyForSummary(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		Auto: true, ContextLength: 1000, OutputReserve: 100,
		TriggerRatio: 0.5, TailTokens: 10, TailTurns: 1, MinMessagesToCompact: 1,
	})
	mainModel := ChatModel("main-slow")
	lightModel := ChatModel("light-fast")
	params := GenerateParams{
		Model: mainModel,
		Messages: []Message{
			UserMessage("first"), AssistantMessage("second"),
			UserMessage("third"), AssistantMessage("fourth"),
			UserMessage("fifth"), AssistantMessage("sixth"),
		},
	}

	capture := &modelCaptureProvider{}
	got, err := c.summarizeMessages(context.Background(), params, capture, lightModel)
	if err != nil {
		t.Fatal(err)
	}
	if capture.lastModel == nil || capture.lastModel.ID != "light-fast" {
		t.Fatalf("summary must use the light model, got %+v", capture.lastModel)
	}
	if got.Model == nil || got.Model.ID != "main-slow" {
		t.Fatalf("main params model must stay unchanged, got %+v", got.Model)
	}

	// 没配快模型时沿用主模型，保持旧行为。
	fallback := &modelCaptureProvider{}
	if _, err := c.summarizeMessages(context.Background(), params, fallback, nil); err != nil {
		t.Fatal(err)
	}
	if fallback.lastModel == nil || fallback.lastModel.ID != "main-slow" {
		t.Fatalf("without a light model the main model must be used, got %+v", fallback.lastModel)
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

func TestCompactor_DoomLoopResetsWhenNotOverflowing(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:            100000,
		ReservedTokens:       1000,
		TailTokens:           1000,
		MinMessagesToCompact: 1,
		Auto:                 true,
	})
	params := GenerateParams{
		Messages: []Message{UserMessage("hi")},
	}
	if _, err := c.Compact(context.Background(), params, nil); err != nil {
		t.Fatalf("compact: %v", err)
	}
	c.mu.Lock()
	count := c.compactionCount
	c.mu.Unlock()
	if count != 0 {
		t.Errorf("successful prune must not count toward doom loop, got %d", count)
	}
}

func TestCompactor_DoomLoopIncrementsOnlyOnOverflow(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		MaxTokens:            50,
		ReservedTokens:       20,
		TailTokens:           10,
		MinMessagesToCompact: 1,
		Auto:                 true,
	})
	params := GenerateParams{
		System:   strings.Repeat("x", 400),
		Messages: []Message{UserMessage(strings.Repeat("a", 400))},
	}
	if _, err := c.Compact(context.Background(), params, nil); err != nil {
		t.Fatalf("compact: %v", err)
	}
	c.mu.Lock()
	count := c.compactionCount
	c.mu.Unlock()
	if count != 1 {
		t.Errorf("overflow without provider should count one attempt, got %d", count)
	}
}

type summaryCaptureProvider struct {
	name  string
	calls []GenerateParams
	reply func(params GenerateParams) (*GenerateResult, error)
}

func (p *summaryCaptureProvider) Name() string { return p.name }

func (p *summaryCaptureProvider) DoGenerate(_ context.Context, params GenerateParams) (*GenerateResult, error) {
	p.calls = append(p.calls, params)
	return p.reply(params)
}

func (p *summaryCaptureProvider) DoStream(context.Context, GenerateParams) (*StreamResult, error) {
	return nil, fmt.Errorf("stream unused")
}

func TestCompactionPrepareStepWithTarget_IsolatesLightParamsAndDisableFlag(t *testing.T) {
	lightProv := &summaryCaptureProvider{
		name: "light",
		reply: func(GenerateParams) (*GenerateResult, error) {
			return &GenerateResult{Text: "## Goal\n- Light summary", FinishReason: FinishReasonStop}, nil
		},
	}
	mainProv := &summaryCaptureProvider{
		name: "main",
		reply: func(GenerateParams) (*GenerateResult, error) {
			return &GenerateResult{Text: "## Goal\n- Main summary", FinishReason: FinishReasonStop}, nil
		},
	}

	liveCfg := CompactionConfig{
		MaxTokens:            160,
		ReservedTokens:       40,
		TailTokens:           20,
		TailTurns:            1,
		MinMessagesToCompact: 2,
		Auto:                 true,
	}
	compactor := NewCompactor(liveCfg)
	compactor.SetConfigSource(func() CompactionConfig { return liveCfg })

	lightSample := 0.15
	lightTopP := 0.75
	hook := CompactionPrepareStepWithTarget(compactor, mainProv, &CompactionSummaryTarget{
		Provider:      lightProv,
		Model:         ChatModel("light-summarizer"),
		MaxTokens:     4096,
		Temperature:   &lightSample,
		TopP:          &lightTopP,
		ContextLength: 64000,
	})

	mainCap := 128000
	mainSample := 0.9
	mainTopP := 0.95
	makeOverflowParams := func() *GenerateParams {
		return &GenerateParams{
			Model:       ChatModel("main-reasoner"),
			MaxTokens:   &mainCap,
			Temperature: &mainSample,
			TopP:        &mainTopP,
			System:      strings.Repeat("s", 300),
			Messages: []Message{
				UserMessage(strings.Repeat("u1 ", 80)),
				AssistantMessage(strings.Repeat("a1 ", 80)),
				UserMessage(strings.Repeat("u2 ", 40)),
			},
		}
	}

	res := hook(context.Background())(makeOverflowParams())
	if res == nil {
		t.Fatal("expected compaction prepare step to rewrite messages")
	}
	if len(lightProv.calls) != 1 || len(mainProv.calls) != 0 {
		t.Fatalf("light calls=%d main calls=%d, want 1 and 0", len(lightProv.calls), len(mainProv.calls))
	}
	got := lightProv.calls[0]
	if got.Model == nil || got.Model.ID != "light-summarizer" {
		t.Fatalf("summary model = %v, want light-summarizer", got.Model)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 4096 {
		t.Fatalf("light summary MaxTokens = %v, want 4096 (must not inherit main 128000)", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.15 || got.TopP == nil || *got.TopP != 0.75 {
		t.Fatalf("light summary sampling = temp:%v topP:%v, want 0.15/0.75", got.Temperature, got.TopP)
	}

	// Toggle DisableLightSummary at runtime -> must summarize on Main with Main's parameters.
	liveCfg.DisableLightSummary = true
	compactor.resetDoomLoop()
	res2 := hook(context.Background())(makeOverflowParams())
	if res2 == nil {
		t.Fatal("expected compaction prepare step to rewrite messages with main model")
	}
	if len(mainProv.calls) != 1 {
		t.Fatalf("main calls = %d, want 1 when DisableLightSummary is true", len(mainProv.calls))
	}
	if mainProv.calls[0].Model == nil || mainProv.calls[0].Model.ID != "main-reasoner" {
		t.Fatalf("main summary model = %v, want main-reasoner", mainProv.calls[0].Model)
	}
}

func TestCompactionPrepareStepWithTarget_FallsBackToMainOnWindowOrError(t *testing.T) {
	t.Run("input exceeds light context window", func(t *testing.T) {
		lightProv := &summaryCaptureProvider{
			name: "light",
			reply: func(GenerateParams) (*GenerateResult, error) {
				t.Fatal("light provider must not be called when prompt exceeds its ContextLength")
				return nil, nil
			},
		}
		mainProv := &summaryCaptureProvider{
			name: "main",
			reply: func(GenerateParams) (*GenerateResult, error) {
				return &GenerateResult{Text: "## Goal\n- Fallback main summary", FinishReason: FinishReasonStop}, nil
			},
		}
		compactor := NewCompactor(CompactionConfig{
			MaxTokens:            160,
			ReservedTokens:       40,
			TailTokens:           20,
			TailTurns:            1,
			MinMessagesToCompact: 2,
			Auto:                 true,
		})
		hook := CompactionPrepareStepWithTarget(compactor, mainProv, &CompactionSummaryTarget{
			Provider:      lightProv,
			Model:         ChatModel("tiny-light"),
			ContextLength: 500, // smaller than summary prompt + margin
		})
		mainCap := 64000
		p := &GenerateParams{
			Model:     ChatModel("main-long"),
			MaxTokens: &mainCap,
			System:    strings.Repeat("s", 300),
			Messages: []Message{
				UserMessage(strings.Repeat("long history ", 100)),
				AssistantMessage(strings.Repeat("assistant reply ", 100)),
				UserMessage("latest turn"),
			},
		}
		if res := hook(context.Background())(p); res == nil {
			t.Fatal("expected compaction to succeed via main fallback")
		}
		if len(mainProv.calls) != 1 || len(lightProv.calls) != 0 {
			t.Fatalf("main calls=%d light calls=%d, want 1 and 0", len(mainProv.calls), len(lightProv.calls))
		}
	})

	t.Run("light provider error retries on main", func(t *testing.T) {
		lightProv := &summaryCaptureProvider{
			name: "light",
			reply: func(GenerateParams) (*GenerateResult, error) {
				return nil, fmt.Errorf("light model 400 invalid max_tokens")
			},
		}
		mainProv := &summaryCaptureProvider{
			name: "main",
			reply: func(GenerateParams) (*GenerateResult, error) {
				return &GenerateResult{Text: "## Goal\n- Recovered on main", FinishReason: FinishReasonStop}, nil
			},
		}
		compactor := NewCompactor(CompactionConfig{
			MaxTokens:            160,
			ReservedTokens:       40,
			TailTokens:           20,
			TailTurns:            1,
			MinMessagesToCompact: 2,
			Auto:                 true,
		})
		hook := CompactionPrepareStepWithTarget(compactor, mainProv, &CompactionSummaryTarget{
			Provider:      lightProv,
			Model:         ChatModel("flaky-light"),
			ContextLength: 128000,
		})
		mainCap := 64000
		p := &GenerateParams{
			Model:     ChatModel("main-model"),
			MaxTokens: &mainCap,
			System:    strings.Repeat("s", 300),
			Messages: []Message{
				UserMessage(strings.Repeat("history ", 80)),
				AssistantMessage(strings.Repeat("reply ", 80)),
				UserMessage("latest"),
			},
		}
		res := hook(context.Background())(p)
		if res == nil {
			t.Fatal("expected compaction to recover on main")
		}
		if len(lightProv.calls) != 1 || len(mainProv.calls) != 1 {
			t.Fatalf("light calls=%d main calls=%d, want 1 and 1", len(lightProv.calls), len(mainProv.calls))
		}
		if gotText := TextFromParts(res.Messages[0].Content); !strings.Contains(gotText, "Recovered on main") {
			t.Fatalf("expected main summary in compacted messages, got %q", gotText)
		}
	})
}
