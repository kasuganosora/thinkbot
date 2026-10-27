package memory

import (
	"context"
	"strings"
	"testing"
)

// TestRenderBlockWrapsInPairedTags 覆盖注入记忆块的成对标签：
// 出站硬闸（StripRecallBlocks）与上下文预算摘除都靠这对标签定位整块，
// renderBlock 少写一半，那两处就会静默失效（记忆内容会漏到输出端）。
func TestRenderBlockWrapsInPairedTags(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	if err := repo.Append(ctx, Entry{
		Scope:      ChannelScope("c1"),
		Content:    "正式环境日志主题为 yak_log_json",
		Category:   "fact",
		Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	snap := NewSnapshot(SnapshotConfig{Mode: ModeFrozen})
	if err := snap.Init(ctx, repo, []Scope{ChannelScope("c1")}); err != nil {
		t.Fatal(err)
	}
	text := snap.FullSnapshot()
	if text == "" {
		t.Fatal("empty snapshot")
	}
	open := strings.Index(text, RecallBlockTagOpen)
	closeIdx := strings.Index(text, RecallBlockTagClose)
	if open < 0 || closeIdx < 0 {
		t.Fatalf("memory block must be wrapped in %s…%s, got %q", RecallBlockTagOpen, RecallBlockTagClose, text)
	}
	if open > closeIdx {
		t.Fatalf("open tag must precede close tag, got %q", text)
	}

	// 摘除后必须什么都不剩：整块连内容一起消失
	if left := StripRecallBlocks(text); strings.Contains(left, "yak_log_json") ||
		strings.Contains(left, RecallBlockTagOpen) || strings.Contains(left, RecallBlockTagClose) {
		t.Fatalf("strip left residue: %q", left)
	}
}
