package llm

import (
	"context"
	"sync"
)

// toolTurnBudget counts named tool-path invocations inside one orchestration
// run. Deep memory recall uses it so a heavier opt-in path cannot loop for
// the rest of the turn. It does not itself run any recall.
type toolTurnBudget struct {
	mu sync.Mutex
	n  map[string]int
}

type toolTurnBudgetKey struct{}

// WithToolTurnBudget attaches a fresh per-turn counter. A context that
// already has one is returned unchanged so nested calls share the turn.
func WithToolTurnBudget(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(toolTurnBudgetKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, toolTurnBudgetKey{}, &toolTurnBudget{n: map[string]int{}})
}

// IncToolTurnBudget adds one to the named counter and returns the new value.
// When no counter is installed (tests and callers outside an orchestration
// run) every call reports 1, so a single invocation still proceeds and a
// shared turn is what enforces the cap.
func IncToolTurnBudget(ctx context.Context, name string) int {
	if ctx == nil {
		return 1
	}
	b, _ := ctx.Value(toolTurnBudgetKey{}).(*toolTurnBudget)
	if b == nil {
		return 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == nil {
		b.n = map[string]int{}
	}
	b.n[name]++
	return b.n[name]
}
