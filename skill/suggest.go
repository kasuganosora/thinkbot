package skill

import (
	"sort"
	"strings"
	"unicode"
)

// ============================================================================
// SuggestSkills — lenient "did you mean" matching
//
// skill_search is strict (AND over keywords) on purpose: it is the model's
// deliberate discovery step. Two places need a lenient matcher instead:
//   - use_skill with an unknown name ("ctrip" for "ctrip-wendao", "bangumi"
//     before the skill was installed): the error should name close matches
//     instead of dumping the whole catalog;
//   - tool_search's DiscoveryProbe: a query that matches no tool but does
//     match a skill should point the model at skill_search.
// ============================================================================

// suggestMaxDistance is the edit distance under which a skill name counts as
// a typo of the requested name (only for names of at least 4 runes).
const suggestMaxDistance = 2

// SuggestSkills returns the names of enabled skills that plausibly match
// query, best first, at most limit (<=0 → 5):
//  1. the strict SearchSkills hits (all keywords match);
//  2. otherwise skills matching ANY keyword or name token (name hits first,
//     then by number of matched keywords);
//  3. plus names within a small edit distance of the whole query (typos).
func (m *SkillManager) SuggestSkills(query string, limit int) []string {
	if limit <= 0 {
		limit = 5
	}
	if strict := m.SearchSkills(query, limit); len(strict) > 0 {
		names := make([]string, 0, len(strict))
		for _, h := range strict {
			names = append(names, h.Name)
		}
		return names
	}

	keywords := suggestKeywords(query)
	if len(keywords) == 0 {
		return nil
	}
	whole := strings.ToLower(strings.TrimSpace(query))

	type cand struct {
		name  string
		score int
	}
	var cands []cand

	m.mu.RLock()
	for _, s := range m.skills {
		if !s.Enabled {
			continue
		}
		name := strings.ToLower(s.Name)
		desc := strings.ToLower(s.Description)
		score := 0
		for _, kw := range keywords {
			switch {
			case strings.Contains(name, kw) || (len([]rune(kw)) >= 3 && strings.Contains(kw, name)):
				score += skillSearchNameWeight
			case strings.Contains(desc, kw):
				score += skillSearchDescWeight
			}
		}
		if len([]rune(whole)) >= 4 && levenshtein(whole, name) <= suggestMaxDistance {
			score += skillSearchNameWeight
		}
		if score > 0 {
			cands = append(cands, cand{name: s.Name, score: score})
		}
	}
	m.mu.RUnlock()

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].name < cands[j].name
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.name)
	}
	return names
}

// suggestKeywords splits a query (or a requested skill name such as
// "ctrip-wendao") into lowercase keywords, dropping very short tokens and
// the word "skill" itself.
func suggestKeywords(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '_' || r == '/' || r == ',' || r == '.' || r == ':'
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		if f == "skill" || f == "skills" || seen[f] {
			continue
		}
		// Single ASCII letters/digits are noise; single CJK runes are words.
		if r := []rune(f); len(r) < 2 && r[0] < 0x80 {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// levenshtein returns the edit distance between a and b (rune based).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
