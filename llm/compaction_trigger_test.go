package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// 回归背景：工具输出在截断阈值内会保持原始 map/slice/struct，旧估算把这类
// 结果算成 0 token，导致上下文体积在压缩判定里几乎隐形、自动压缩永不触发。

func bigMapResult(repeat int) map[string]any {
	return map[string]any{
		"content": strings.Repeat("abcdefgh", repeat),
		"count":   repeat,
	}
}

func mapToolParams(repeat, messages int) GenerateParams {
	msgs := make([]Message, 0, messages)
	for i := 0; i < messages; i++ {
		msgs = append(msgs, Message{
			Role: MessageRoleAssistant,
			Content: []MessagePart{
				ToolResultPart{ToolName: "read_file", Result: bigMapResult(repeat)},
			},
		})
	}
	return GenerateParams{System: "system prompt", Messages: msgs}
}

func cappedCompactor() *Compactor {
	cfg := DefaultCompactionConfig()
	cfg.ContextLength = 1_048_576 // 超长窗口：无 cap 时触发线会被抬到 786432
	cfg.TriggerCap = 48_000
	return NewCompactor(cfg)
}

func TestEstimatePartResultTokens_ComplexTypes(t *testing.T) {
	payload := bigMapResult(1000)
	blob, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := EstimateTokens(string(blob))
	got := EstimatePartResultTokens(payload)
	if got != want {
		t.Fatalf("map result estimated = %d, want %d (JSON text)", got, want)
	}
	if got < 100 {
		t.Fatalf("complex tool result must not collapse to ~0 tokens, got %d", got)
	}
	if EstimatePartResultTokens([]string{"alpha", "beta"}) == 0 {
		t.Fatal("slice result must be counted")
	}
	if EstimatePartResultTokens(struct{ Text string }{Text: "hello world"}) == 0 {
		t.Fatal("struct result must be counted")
	}
	if EstimatePartResultTokens(nil) != 0 {
		t.Fatal("nil result must be 0")
	}
	if EstimatePartResultTokens("plain text") != EstimateTokens("plain text") {
		t.Fatal("string result must keep the old path")
	}
	if EstimatePartResultTokens([]byte("plain text")) != EstimateTokens("plain text") {
		t.Fatal("[]byte result must keep the old path")
	}
}

func TestEstimateParamsTokens_MapToolResultsAreCounted(t *testing.T) {
	params := mapToolParams(3000, 6)
	got := EstimateParamsTokens(params)
	// 6 × 24KB 文本，估算应为万级；修复前所有 map 输出都是 0，只剩几百 token。
	if got < 10_000 {
		t.Fatalf("estimated tokens = %d, expected tool outputs to be counted (want >= 10000)", got)
	}
}

func TestCompactor_ShouldCompactIn_EstimatedOverflow(t *testing.T) {
	c := cappedCompactor()
	params := mapToolParams(12_000, 6) // 估算远超 48000
	if !c.ShouldCompactIn(context.Background(), params) {
		t.Fatal("estimated tokens over trigger must request compaction")
	}
}

func TestCompactor_ShouldCompactIn_UsageFallback(t *testing.T) {
	c := cappedCompactor()
	// 估算远低于触发线，但上一步 provider 报告的真实输入已经超线。
	params := mapToolParams(10, 6)
	params.Messages = append(params.Messages, Message{
		Role:    MessageRoleAssistant,
		Content: []MessagePart{TextPart{Text: "ok"}},
		Usage:   &Usage{InputTokens: 60_000},
	})
	if !c.ShouldCompactIn(context.Background(), params) {
		t.Fatal("real usage over trigger must request compaction even when the estimate is low")
	}
}

func TestCompactor_ShouldCompactIn_NoOverflow(t *testing.T) {
	c := cappedCompactor()
	params := mapToolParams(10, 6)
	params.Messages = append(params.Messages, Message{
		Role:    MessageRoleAssistant,
		Content: []MessagePart{TextPart{Text: "ok"}},
		Usage:   &Usage{InputTokens: 100},
	})
	if c.ShouldCompactIn(context.Background(), params) {
		t.Fatal("both estimate and usage below trigger must not request compaction")
	}
}

func TestCompactor_ShouldCompactIn_LogsDecisionWithSkipReason(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	cfg := DefaultCompactionConfig()
	cfg.ContextLength = 1_048_576
	cfg.TriggerCap = 48_000
	cfg.Auto = false
	c := NewCompactor(cfg).SetLogger(zap.New(core).Sugar())

	params := mapToolParams(10, 6)
	if c.ShouldCompactIn(context.Background(), params) {
		t.Fatal("Auto=false must not request compaction")
	}

	var found bool
	for _, entry := range logs.All() {
		if entry.Message != "compaction: check" {
			continue
		}
		found = true
		fields := entry.ContextMap()
		if fields["skip_reason"] != "auto_disabled" {
			t.Fatalf("skip_reason = %v, want auto_disabled", fields["skip_reason"])
		}
		if _, ok := fields["estimated_tokens"]; !ok {
			t.Fatal("compaction: check must expose estimated_tokens")
		}
		if _, ok := fields["trigger"]; !ok {
			t.Fatal("compaction: check must expose trigger")
		}
		if fields["should_compact"] != false {
			t.Fatalf("should_compact = %v, want false", fields["should_compact"])
		}
	}
	if !found {
		t.Fatal("compaction: check was not logged at INFO level")
	}
}

func TestLatestStepUsage(t *testing.T) {
	msgs := []Message{
		{Role: MessageRoleAssistant, Usage: &Usage{InputTokens: 100}},
		{Role: MessageRoleUser},
		{Role: MessageRoleAssistant, Usage: &Usage{InputTokens: 900}},
		{Role: MessageRoleUser},
	}
	u := latestStepUsage(msgs)
	if u == nil || u.InputTokens != 900 {
		t.Fatalf("latestStepUsage = %+v, want the most recent usage (900)", u)
	}
	if latestStepUsage([]Message{{Role: MessageRoleUser}}) != nil {
		t.Fatal("messages without usage must return nil")
	}
}
