package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// 回归背景：Compactor 按会话跨轮复用（增量摘要需要跨轮持久），但 doom-loop
// 计数是单轮保护。ShouldCompactIn 命中 doom-loop 时直接短路，压缩流程内部的
// 清零因此再也不会执行：修复前某轮累计到阈值后，该会话后续所有轮次都会
// 永久禁用压缩，长任务历史一路膨胀。新回合必须显式 ResetDoomLoop。

func turnResetCompactor() *Compactor {
	cfg := DefaultCompactionConfig()
	cfg.ContextLength = 1_048_576
	cfg.TriggerCap = 10_000
	cfg.MinMessagesToCompact = 2
	return NewCompactor(cfg)
}

// 纯文本消息，避免 PruneToolOutputs 介入导致「压缩后不再溢出」而提前清零。
func turnResetOverflowParams() GenerateParams {
	msgs := make([]Message, 0, 8)
	for i := 0; i < 8; i++ {
		msgs = append(msgs, Message{
			Role:    MessageRoleAssistant,
			Content: []MessagePart{TextPart{Text: strings.Repeat("payload ", 900)}},
		})
	}
	return GenerateParams{System: "system prompt", Messages: msgs}
}

func TestCompactor_ResetDoomLoop_UnlocksNextTurn(t *testing.T) {
	c := turnResetCompactor()
	ctx := context.Background()
	params := turnResetOverflowParams()
	if !c.ShouldCompactIn(ctx, params) {
		t.Fatal("fixture must overflow the trigger")
	}

	// 走真实 PrepareStep 钩子模拟「连续压缩却压不下去」：mainProvider / target
	// 均为 nil 时 compactWithTarget 在计数后直接返回，不产生摘要。
	hook := CompactionPrepareStepWithTarget(c, nil, nil)(ctx)
	for i := 0; i < DoomLoopThreshold; i++ {
		p := params
		if got := hook(&p); got == nil {
			t.Fatalf("compaction hook stopped at iteration %d before reaching the doom-loop threshold", i)
		}
	}
	if got := c.compactionCountSnapshot(); got != DoomLoopThreshold {
		t.Fatalf("compactionCount = %d, want %d", got, DoomLoopThreshold)
	}
	if c.ShouldCompactIn(ctx, params) {
		t.Fatal("doom-loop protection must still work within the same turn")
	}

	// 新回合开始：计数清零，压缩恢复可用。
	c.ResetDoomLoop()
	if got := c.compactionCountSnapshot(); got != 0 {
		t.Fatalf("new turn must reset the counter, got %d", got)
	}
	if !c.ShouldCompactIn(ctx, params) {
		t.Fatal("new turn must be able to compact again")
	}
}

func TestCompactor_ResetDoomLoop_KeepsIncrementalSummary(t *testing.T) {
	c := turnResetCompactor()
	c.mu.Lock()
	c.previousSummary = "anchor from previous turn"
	c.compactionCount = DoomLoopThreshold
	c.mu.Unlock()

	c.ResetDoomLoop()

	c.mu.Lock()
	summary, count := c.previousSummary, c.compactionCount
	c.mu.Unlock()
	if summary != "anchor from previous turn" {
		t.Fatalf("ResetDoomLoop must not drop previousSummary, got %q", summary)
	}
	if count != 0 {
		t.Fatalf("compactionCount = %d, want 0", count)
	}
}

func TestCompactor_ShouldCompactIn_LogsCompactionCount(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	c := turnResetCompactor().SetLogger(zap.New(core).Sugar())
	c.mu.Lock()
	c.compactionCount = 2
	c.mu.Unlock()

	_ = c.ShouldCompactIn(context.Background(), turnResetOverflowParams())

	var seen bool
	for _, entry := range logs.All() {
		if entry.Message != "compaction: check" {
			continue
		}
		seen = true
		fields := entry.ContextMap()
		if got := fmt.Sprintf("%v", fields["compaction_count"]); got != "2" {
			t.Fatalf("compaction_count = %v, want 2", fields["compaction_count"])
		}
		if fields["should_compact"] != true {
			t.Fatalf("should_compact = %v, want true below the threshold", fields["should_compact"])
		}
	}
	if !seen {
		t.Fatal("compaction: check was not logged at INFO level")
	}

	// 达到阈值后，日志必须能直接读出锁死原因和当前计数。
	c.mu.Lock()
	c.compactionCount = DoomLoopThreshold
	c.mu.Unlock()
	_ = c.ShouldCompactIn(context.Background(), turnResetOverflowParams())
	var locked bool
	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		if fields["skip_reason"] == "doom_loop" {
			locked = true
			if got := fmt.Sprintf("%v", fields["compaction_count"]); got != fmt.Sprintf("%d", DoomLoopThreshold) {
				t.Fatalf("locked compaction_count = %v, want %d", fields["compaction_count"], DoomLoopThreshold)
			}
		}
	}
	if !locked {
		t.Fatal("doom-loop decision must expose skip_reason in the INFO log")
	}
}
