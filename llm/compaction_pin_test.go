package llm

import (
	"context"
	"testing"
)

func TestCompactor_PinnedLimitsStayAcrossCallerPublish(t *testing.T) {
	c := NewCompactor(CompactionConfig{
		Auto: true, ContextLength: 1000, OutputReserve: 100, TriggerRatio: 0.5, TailTokens: 10, MinMessagesToCompact: 1,
	})
	currentLen := 1000
	c.SetLimitSource(func(ctx context.Context) (int, int) {
		if v := ctx.Value(pinKey{}); v != nil {
			return v.(int), 100
		}
		return currentLen, 100
	})
	pinned := context.WithValue(context.Background(), pinKey{}, 1000)
	currentLen = 8000
	pinnedTrigger := c.budgetIn(pinned).Trigger
	latestTrigger := c.budgetIn(context.Background()).Trigger
	if pinnedTrigger == latestTrigger {
		t.Fatalf("pinned and latest triggers both %d", pinnedTrigger)
	}
	if latestTrigger <= pinnedTrigger {
		t.Fatalf("latest %d pinned %d", latestTrigger, pinnedTrigger)
	}
}

type pinKey struct{}
