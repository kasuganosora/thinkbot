package memory

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ============================================================================
// Actionable memory-tool errors
//
// 2026-09-27: replace failed 4 times with "No entry matched '<old_text>'":
// the model passed a long paraphrase of the entry (or an older version of
// it) as old_text instead of a short exact substring. The error now lists
// the closest entries (id + exact current text) and replace also accepts
// memory_id/id, so the next call can target the entry directly.
// ============================================================================

// memoryActions lists valid actions (kept in sync with the tool schema).
const memoryActions = "add, replace, remove, search, recent, count, batch"

// missingActionError explains the required action parameter.
func missingActionError(m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hint := ""
	switch {
	case m["old_text"] != nil && m["content"] != nil:
		hint = ` These arguments look like a replace: add "action":"replace".`
	case m["content"] != nil:
		hint = ` To store new information use "action":"add".`
	case m["query"] != nil:
		hint = ` To look something up use "action":"search".`
	}
	return fmt.Errorf("action is required (one of: %s); got keys %v.%s", memoryActions, keys, hint)
}

const (
	maxMatchCandidates  = 3
	minCandidateScore   = 0.2
	candidatePreviewLen = 160
)

// closestEntries ranks entries by similarity to text (character-bigram Dice,
// so it works for Chinese and paraphrases) and returns the best few.
func closestEntries(entries []Entry, text string) []Entry {
	want := bigrams(text)
	if len(want) == 0 {
		return nil
	}
	type scored struct {
		e Entry
		s float64
	}
	var ss []scored
	for _, e := range entries {
		if s := dice(want, bigrams(e.Content)); s >= minCandidateScore {
			ss = append(ss, scored{e, s})
		}
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].s > ss[j].s })
	if len(ss) > maxMatchCandidates {
		ss = ss[:maxMatchCandidates]
	}
	out := make([]Entry, len(ss))
	for i, s := range ss {
		out[i] = s.e
	}
	return out
}

func bigrams(s string) map[string]int {
	var rs []rune
	for _, r := range strings.ToLower(s) {
		if !unicode.IsSpace(r) {
			rs = append(rs, r)
		}
	}
	m := make(map[string]int, len(rs))
	for i := 0; i+1 < len(rs); i++ {
		m[string(rs[i:i+2])]++
	}
	return m
}

func dice(a, b map[string]int) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter, na, nb := 0, 0, 0
	for k, v := range a {
		na += v
		if w, ok := b[k]; ok {
			inter += min(v, w)
		}
	}
	for _, v := range b {
		nb += v
	}
	return 2 * float64(inter) / float64(na+nb)
}

// noMatchResponse builds the replace/remove "no entry matched" result with
// the closest candidates.
func noMatchResponse(scope Scope, oldText string, candidates []Entry) map[string]any {
	msg := fmt.Sprintf("No entry in scope %s contains old_text '%s'. old_text must be an exact substring of the CURRENT entry text "+
		"(a short distinctive phrase copied from it works best), not a paraphrase or an older version.", scope.Key(), truncRunes(oldText, 80))
	out := map[string]any{"success": false}
	if len(candidates) > 0 {
		list := make([]map[string]string, 0, len(candidates))
		for _, c := range candidates {
			list = append(list, map[string]string{"id": c.ID, "content": truncRunes(c.Content, candidatePreviewLen)})
		}
		out["closest_entries"] = list
		msg += " Closest entries are listed in closest_entries: retry with memory_id=<id> (replace and remove accept it) or copy old_text exactly from their content."
	} else {
		msg += " Use action=search to find the entry first."
	}
	out["error"] = msg
	return out
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
