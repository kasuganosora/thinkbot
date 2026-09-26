package llm

import (
	"context"
	"sort"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// beginDeferralRun forks cfg.ToolDeferral for one orchestration run and
// returns the fork plus a func that writes its loaded-tool state back to the
// parent (call once when the run ends). Returns (nil, no-op) without deferral.
//
// Isolation matters because the deferral handed in by the caller may be
// shared by several concurrent runs (same conversation, or — before the
// conversation-key fix of 2026-09-26 — every conversation of a bot): with a
// shared instance a concurrent run's SetTools replaced the tool list that a
// running turn re-read on every step, so its sandbox tools vanished mid-turn.
func beginDeferralRun(cfg *OrchestrateConfig) (*ToolDeferral, func()) {
	if cfg == nil || cfg.ToolDeferral == nil {
		return nil, func() {}
	}
	parent := cfg.ToolDeferral
	run := parent.Fork()
	return run, func() { parent.Adopt(run) }
}

// toolViewGuard detects a model-facing tool list that loses tools within one
// run. Deferred tools are only stripped of their schema (their names stay
// listed) and tool_search legitimately disappears once every deferred tool
// is loaded, so any other disappearing name is a bug worth a WARN.
type toolViewGuard struct {
	initial []string
	lastKey string
}

const toolSearchName = "tool_search"

func newToolViewGuard(view []Tool) *toolViewGuard {
	g := &toolViewGuard{}
	for _, t := range view {
		if t.Name != toolSearchName {
			g.initial = append(g.initial, t.Name)
		}
	}
	sort.Strings(g.initial)
	return g
}

// missing returns the initial tool names absent from view.
func (g *toolViewGuard) missing(view []Tool) []string {
	present := make(map[string]bool, len(view))
	for _, t := range view {
		present[t.Name] = true
	}
	var out []string
	for _, n := range g.initial {
		if !present[n] {
			out = append(out, n)
		}
	}
	return out
}

// check logs a WARN when the view lost tools compared with the start of the
// run (once per distinct missing set, to avoid a log line per step).
func (g *toolViewGuard) check(ctx context.Context, step int, view []Tool) {
	if g == nil {
		return
	}
	miss := g.missing(view)
	key := ""
	for _, n := range miss {
		key += n + ","
	}
	if key == g.lastKey {
		return
	}
	g.lastKey = key
	if len(miss) == 0 {
		return
	}
	if logger := traceid.L(ctx); logger != nil {
		logger.Warnw("tool list shrank within a turn",
			"step", step, "missing", miss,
			"tools_at_start", len(g.initial), "tools_now", len(view))
	}
}
