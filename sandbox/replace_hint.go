package sandbox

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ============================================================================
// replace_in_file mismatch hint
//
// "old_str not found" alone made the model retry the same wrong old_str up
// to 7 times in one turn (2026-09-27). The error now shows the closest
// matching region of the file with line numbers (or says the text differs
// only in whitespace), so the next attempt can copy the exact text.
// ============================================================================

const (
	hintMaxContentBytes = 2 << 20 // skip the search for huge files
	hintMaxLineRunes    = 400     // longer lines are compared by prefix
	hintMaxWindowLines  = 40      // cap on the snippet size
	hintMinSimilarity   = 0.5
	hintMaxAnchors      = 3
	hintMaxCandidates   = 30
)

// oldStrNotFoundError builds the actionable mismatch error.
func oldStrNotFoundError(path, content, oldStr string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "old_str not found in file %q.", path)
	if hint := nearestSnippetHint(content, oldStr); hint != "" {
		b.WriteString(" ")
		b.WriteString(hint)
	} else {
		b.WriteString(" No similar text was found: the file may have changed since you read it. Read the file again (read_file) and copy old_str exactly from the current content.")
	}
	b.WriteString(" Do not retry with the same old_str.")
	return fmt.Errorf("%s", b.String())
}

// nearestSnippetHint returns a hint describing the closest region of content
// to oldStr, or "" when nothing is reasonably similar.
func nearestSnippetHint(content, oldStr string) string {
	if len(content) > hintMaxContentBytes || strings.TrimSpace(oldStr) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	want := strings.Split(strings.Trim(oldStr, "\n"), "\n")

	// Whitespace-insensitive exact match: the usual cause (indentation,
	// tabs vs spaces, trailing spaces).
	if start, ok := findIgnoringWhitespace(lines, want); ok {
		return fmt.Sprintf("The text exists but with different whitespace/indentation at lines %d-%d. Copy it exactly (including leading spaces/tabs) from here:\n%s",
			start+1, start+len(want), numberedSnippet(lines, start, len(want)))
	}

	start, score := bestWindow(lines, want)
	if start < 0 || score < hintMinSimilarity {
		return ""
	}
	n := len(want)
	if n > hintMaxWindowLines {
		n = hintMaxWindowLines
	}
	if start+n > len(lines) {
		n = len(lines) - start
	}
	return fmt.Sprintf("Closest match (lines %d-%d, %.0f%% similar) — the file currently contains:\n%s\nBuild old_str from these exact lines, or read the file again if it changed.",
		start+1, start+n, score*100, numberedSnippet(lines, start, n))
}

func numberedSnippet(lines []string, start, n int) string {
	var b strings.Builder
	for i := start; i < start+n && i < len(lines); i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	return strings.TrimRight(b.String(), "\n")
}

func squashSpace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func findIgnoringWhitespace(lines, want []string) (int, bool) {
	if len(want) == 0 || len(want) > len(lines) {
		return 0, false
	}
	w := make([]string, len(want))
	for i, l := range want {
		w[i] = squashSpace(l)
	}
	for s := 0; s+len(want) <= len(lines); s++ {
		ok := true
		for i := range w {
			if squashSpace(lines[s+i]) != w[i] {
				ok = false
				break
			}
		}
		if ok {
			return s, true
		}
	}
	return 0, false
}

// bestWindow finds the window of len(want) lines most similar to want. It
// anchors on the most distinctive (longest) lines of want to keep the cost
// linear in the file size.
func bestWindow(lines, want []string) (int, float64) {
	type anchor struct{ idx, length int }
	var anchors []anchor
	for i, l := range want {
		if t := strings.TrimSpace(l); t != "" {
			anchors = append(anchors, anchor{i, len([]rune(t))})
		}
	}
	if len(anchors) == 0 {
		return -1, 0
	}
	sort.SliceStable(anchors, func(i, j int) bool { return anchors[i].length > anchors[j].length })
	if len(anchors) > hintMaxAnchors {
		anchors = anchors[:hintMaxAnchors]
	}

	type cand struct {
		start int
		sim   float64
	}
	var cands []cand
	for _, a := range anchors {
		for j, l := range lines {
			sim := lineSimilarity(want[a.idx], l)
			if sim >= hintMinSimilarity {
				cands = append(cands, cand{j - a.idx, sim})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].sim > cands[j].sim })
	if len(cands) > hintMaxCandidates {
		cands = cands[:hintMaxCandidates]
	}

	best, bestScore := -1, 0.0
	seen := map[int]bool{}
	for _, c := range cands {
		s := c.start
		if s < 0 {
			s = 0
		}
		if s >= len(lines) || seen[s] {
			continue
		}
		seen[s] = true
		total := 0.0
		for i, w := range want {
			if s+i < len(lines) {
				total += lineSimilarity(w, lines[s+i])
			}
		}
		if score := total / float64(len(want)); score > bestScore {
			best, bestScore = s, score
		}
	}
	return best, bestScore
}

// lineSimilarity compares two lines ignoring surrounding whitespace:
// 1 - editDistance/maxLen.
func lineSimilarity(a, b string) float64 {
	ra := []rune(strings.TrimSpace(a))
	rb := []rune(strings.TrimSpace(b))
	if len(ra) > hintMaxLineRunes {
		ra = ra[:hintMaxLineRunes]
	}
	if len(rb) > hintMaxLineRunes {
		rb = rb[:hintMaxLineRunes]
	}
	if len(ra) == 0 && len(rb) == 0 {
		return 1
	}
	maxLen := max(len(ra), len(rb))
	diff := len(ra) - len(rb)
	if diff < 0 {
		diff = -diff
	}
	if float64(diff)/float64(maxLen) > 1-hintMinSimilarity {
		return 0 // cannot reach the threshold
	}
	return 1 - float64(runeEditDistance(ra, rb))/float64(maxLen)
}

func runeEditDistance(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// checkWorkspacePath rejects absolute paths outside the workspace root.
//
// File tools resolve every path inside the workspace: "/tmp/notes.md" used
// to become /data/tmp/notes.md silently, while exec/run_code see the real
// /tmp — so a workflow node wrote /tmp/tt-notes/odpt-api.md with write_file
// and the next node's `cat /tmp/tt-notes/odpt-api.md` failed (3 times on
// 2026-09-27). Refusing such paths makes every tool agree on one file.
func checkWorkspacePath(param, p string) error {
	q := strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if !strings.HasPrefix(q, "/") || q == VirtualRoot || strings.HasPrefix(q, VirtualRoot+"/") {
		return nil
	}
	return fmt.Errorf("%s %q is outside the workspace. File tools only access the workspace %s: this path would silently map to %s%s, "+
		"while exec/run_code would use the real %s — two different files. Use a workspace path such as %q (or the relative %q) "+
		"in file tools AND in exec commands, so every tool (and every workflow node) sees the same file",
		param, p, VirtualRoot, VirtualRoot, q, q, VirtualRoot+q, strings.TrimPrefix(q, "/"))
}
