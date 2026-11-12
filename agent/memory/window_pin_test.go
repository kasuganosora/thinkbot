package memory

import (
	"context"
	"testing"
)

type pinKey struct{}

func TestWindow_PinnedTurnIgnoresNewerSnapshot(t *testing.T) {
	current := 8000
	reserve := 2000
	w := NewWindow(WindowConfig{MaxContextTokens: 100, ReservedTokens: 1, OutputReserve: 10, MemoryBudgetRatio: 1, MaxMemoryTokens: 100000})
	w.SetTurnLimits(func(ctx context.Context) (int, int) {
		if v := ctx.Value(pinKey{}); v != nil {
			pair := v.([2]int)
			return pair[0], pair[1]
		}
		return current, reserve
	})
	pinned := context.WithValue(context.Background(), pinKey{}, [2]int{1000, 100})
	current, reserve = 8000, 2000
	if got := w.AvailableIn(pinned); got != 899 {
		t.Fatalf("pinned budget %d", got)
	}
	if got := w.AvailableIn(context.Background()); got != 5999 {
		t.Fatalf("latest budget %d", got)
	}
}
