package storage

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

func TestDedupGroupsKeepStrongPairRegardlessOfSeedOrder(t *testing.T) {
	a := memory.Entry{ID: "a", Content: "周末和朋友去海边露营，带了帐篷、睡袋和一个小炉子，晚上看了星星"}
	b := memory.Entry{ID: "b", Content: "周末和朋友去海边露营，带了帐篷、睡袋和小炉子，晚上一起看星星"}
	x := memory.Entry{ID: "x", Content: "周末去海边露营前查了天气预报，说是晴天"}
	ab := memory.EmbedSimilarity(a.Content, b.Content)
	if ab < strongPairSim || ab > 0.95 {
		t.Fatalf("fixture a~b similarity %.3f, want about 0.88", ab)
	}
	t.Logf("a~b similarity %.3f", ab)
	all := []memory.Entry{a, b, x}
	// Neighbours as the vec search would give them (best first), with x
	// seeing a first, so plain greedy grouping starting at x steals a.
	nb := map[string][]memory.Entry{"x": {a, b}, "a": {b, x}, "b": {a, x}}
	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, ord := range orders {
		seeds := []memory.Entry{all[ord[0]], all[ord[1]], all[ord[2]]}
		groups, _ := buildDedupGroups(seeds, func(e memory.Entry) []memory.Entry { return nb[e.ID] }, 2)
		together := false
		for _, g := range groups {
			ids := map[string]bool{}
			for _, e := range g {
				ids[e.ID] = true
			}
			if ids["a"] && ids["b"] {
				together = true
			}
		}
		if !together {
			t.Fatalf("seed order %v split the %.2f pair: %v", ord, ab, groupIDs(groups))
		}
		again, _ := buildDedupGroups(seeds, func(e memory.Entry) []memory.Entry { return nb[e.ID] }, 2)
		if len(again) != len(groups) || groupIDs(again)[0] != groupIDs(groups)[0] {
			t.Fatalf("not deterministic: %v vs %v", groupIDs(groups), groupIDs(again))
		}
	}
}

func groupIDs(groups [][]memory.Entry) []string {
	var out []string
	for _, g := range groups {
		s := ""
		for _, e := range g {
			s += e.ID
		}
		out = append(out, s)
	}
	return out
}
