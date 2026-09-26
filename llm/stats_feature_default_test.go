package llm

import (
	"context"
	"testing"
)

func TestWithStatsFeatureIfUnset(t *testing.T) {
	// 无标签 → 补上兜底标签
	ctx := WithStatsFeatureIfUnset(context.Background(), "workflow")
	if got := StatsFeatureFromContext(ctx); got != "workflow" {
		t.Fatalf("feature = %q, want workflow", got)
	}
	// 已有更具体的标签 → 不覆盖
	ctx = WithStatsFeatureIfUnset(WithStatsFeature(context.Background(), "subagent"), "workflow")
	if got := StatsFeatureFromContext(ctx); got != "subagent" {
		t.Fatalf("feature = %q, want subagent (must not override)", got)
	}
	// 不改变 skip 语义（与 WithStatsFeature 不同，后者会清除 skip）
	ctx = WithStatsFeatureIfUnset(WithStatsSkip(context.Background()), "workflow")
	if !shouldSkipStats(ctx) {
		t.Fatal("WithStatsFeatureIfUnset must not clear the skip flag")
	}
}
