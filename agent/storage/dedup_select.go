package storage

import (
	"sort"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// dedupSelectConfig controls which entries one dedup pass sends to the LLM.
type dedupSelectConfig struct {
	topK           int
	minSim         float64
	maxSeeds       int
	fallbackRecent int
	maxGroup       int
	maxMergedChars int
	noopSim        float64
}

// strongPairSim: neighbours at least this similar are reserved for each other
// before greedy grouping, so seed order cannot split a near-duplicate pair.
const strongPairSim = 0.85

// buildDedupGroups groups each unchecked seed with its neighbours. An entry
// joins at most one group per pass (merges of one pass never overlap). Seeds
// without any neighbour are returned in lonely: nothing to merge them with,
// so they are marked checked without an LLM call. Groups have 2..maxGroup
// entries, the seed first.
//
// Deterministic two-phase selection: first every seed→neighbour edge with
// similarity >= strongPairSim is paired greedily (highest similarity first,
// ties by ids), each entry in at most one pair; then groups grow in seed order
// and an entry reserved for another partner is never taken by a third group.
func buildDedupGroups(seeds []memory.Entry, neighbours func(memory.Entry) []memory.Entry, maxGroup int) (groups [][]memory.Entry, lonely []string) {
	if maxGroup < 2 {
		maxGroup = 2
	}
	nbs := make(map[string][]memory.Entry, len(seeds))
	type edge struct {
		a, b memory.Entry
		s    float64
	}
	var edges []edge
	for _, seed := range seeds {
		if _, done := nbs[seed.ID]; done {
			continue
		}
		list := neighbours(seed)
		nbs[seed.ID] = list
		for _, n := range list {
			if n.ID == seed.ID || memory.IsMirrorID(n.ID) || memory.IsMirrorID(seed.ID) {
				continue
			}
			if s := memory.EmbedSimilarity(seed.Content, n.Content); s >= strongPairSim {
				edges = append(edges, edge{seed, n, s})
			}
		}
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].s != edges[j].s {
			return edges[i].s > edges[j].s
		}
		if edges[i].a.ID != edges[j].a.ID {
			return edges[i].a.ID < edges[j].a.ID
		}
		return edges[i].b.ID < edges[j].b.ID
	})
	partner := map[string]memory.Entry{}
	for _, e := range edges {
		if _, ok := partner[e.a.ID]; ok {
			continue
		}
		if _, ok := partner[e.b.ID]; ok {
			continue
		}
		partner[e.a.ID] = e.b
		partner[e.b.ID] = e.a
	}

	assigned := map[string]bool{}
	for _, seed := range seeds {
		if assigned[seed.ID] {
			continue
		}
		assigned[seed.ID] = true
		group := []memory.Entry{seed}
		if p, ok := partner[seed.ID]; ok && !assigned[p.ID] {
			assigned[p.ID] = true
			group = append(group, p)
		}
		for _, n := range nbs[seed.ID] {
			if len(group) >= maxGroup {
				break
			}
			if assigned[n.ID] || memory.IsMirrorID(n.ID) {
				continue
			}
			if p, ok := partner[n.ID]; ok && p.ID != seed.ID {
				continue // reserved for its strong partner
			}
			assigned[n.ID] = true
			group = append(group, n)
		}
		if len(group) < 2 {
			lonely = append(lonely, seed.ID)
			continue
		}
		groups = append(groups, group)
	}
	return groups, lonely
}

// filterNeighbours keeps the candidates at least minSim similar to seed
// (same hashed vectors as memory_vec), best first, at most k.
func filterNeighbours(seed memory.Entry, cands []memory.Entry, minSim float64, k int) []memory.Entry {
	type scored struct {
		e memory.Entry
		s float64
	}
	var keep []scored
	for _, c := range cands {
		if c.ID == seed.ID || memory.IsMirrorID(c.ID) {
			continue
		}
		if s := memory.EmbedSimilarity(seed.Content, c.Content); s >= minSim {
			keep = append(keep, scored{c, s})
		}
	}
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].s > keep[j].s })
	if k > 0 && len(keep) > k {
		keep = keep[:k]
	}
	out := make([]memory.Entry, len(keep))
	for i, x := range keep {
		out[i] = x.e
	}
	return out
}

// buildFallbackGroups is the selection without memory_vec: unchecked seeds
// in chunks, each chunk padded with the few most recent checked entries, so
// a group stays small (<= maxGroup) instead of the whole scope.
func buildFallbackGroups(seeds, recent []memory.Entry, maxGroup int) (groups [][]memory.Entry, lonely []string) {
	var clean []memory.Entry
	for _, s := range seeds {
		if memory.IsMirrorID(s.ID) {
			lonely = append(lonely, s.ID)
			continue
		}
		clean = append(clean, s)
	}
	if len(recent) > maxGroup-2 {
		recent = recent[:maxGroup-2]
	}
	chunk := maxGroup - len(recent)
	if chunk < 2 {
		chunk = 2
	}
	for i := 0; i < len(clean); i += chunk {
		end := i + chunk
		if end > len(clean) {
			end = len(clean)
		}
		g := append(append([]memory.Entry(nil), clean[i:end]...), recent...)
		if len(g) > maxGroup {
			g = g[:maxGroup]
		}
		if len(g) < 2 {
			lonely = append(lonely, clean[i].ID)
			continue
		}
		groups = append(groups, g)
	}
	return groups, lonely
}
