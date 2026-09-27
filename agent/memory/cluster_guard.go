package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// ============================================================================
// Detail-preservation guard for memory_dedup (ClusterMerge)
//
// After memory_dedup's reasoning_effort was lowered (09-26), merged entries
// got much shorter and started dropping concrete details: in the TG channel
// scope 16 of 25 clusters lost at least a third of their identifiers (before:
// 3 of 26), e.g. a user-profile merge dropped two TB-xxxx authorisation codes,
// a GitHub URL and file names. The archived sources are no longer recalled,
// so the detail is effectively lost.
//
// Identifiers (URLs, codes that mix letters and digits, file names, numbers
// of four or more digits) are cheap to check deterministically, so every
// merged_content must keep all identifiers of its sources verbatim. Clusters
// that don't get one repair round (the model is told exactly what is
// missing); clusters still missing identifiers are dropped, which leaves
// their sources active and untouched.
// ============================================================================

var (
	guardURLRe   = regexp.MustCompile(`https?://[^\s<>"'()（）「」『』【】\[\]，。、；！？]+`)
	guardTokenRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_\-./@#:+]*[A-Za-z0-9]`)
	guardFileRe  = regexp.MustCompile(`[A-Za-z][\w\-]*\.[a-z][a-z0-9]{0,4}$`)
)

// maxMissingListed caps the identifiers listed per cluster in a repair
// request and in logs.
const maxMissingListed = 20

// detailIdentifiers returns the identifiers in text that a merge must keep
// verbatim: URLs, tokens mixing letters and digits (codes, hashes, versions,
// IDs; at least 5 chars), file names (name.ext) and numbers with 4+ digits.
func detailIdentifiers(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimRight(s, ".,;:!?")
		if s == "" || seen[strings.ToLower(s)] {
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	for _, u := range guardURLRe.FindAllString(text, -1) {
		add(u)
	}
	rest := guardURLRe.ReplaceAllString(text, " ")
	for _, t := range guardTokenRe.FindAllString(rest, -1) {
		t = strings.TrimRight(t, ".")
		hasDigit, hasLetter := false, false
		for _, r := range t {
			switch {
			case unicode.IsDigit(r):
				hasDigit = true
			case unicode.IsLetter(r):
				hasLetter = true
			}
		}
		switch {
		case hasDigit && hasLetter && len(t) >= 5:
			add(t)
		case !hasLetter && hasDigit && len(t) >= 4 && isDigits(t):
			add(t)
		case hasLetter && len(t) >= 4 && guardFileRe.MatchString(t):
			add(t)
		}
	}
	return out
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// missingIdentifiers returns the identifiers of the cluster's sources that
// merged_content does not contain (case-insensitive substring match). Source
// IDs unknown to byID are ignored.
func missingIdentifiers(cl ClusterResult, byID map[string]string) []string {
	merged := strings.ToLower(cl.MergedContent)
	var missing []string
	seen := map[string]bool{}
	for _, id := range cl.SourceIDs {
		content, ok := byID[id]
		if !ok {
			continue
		}
		for _, ident := range detailIdentifiers(content) {
			k := strings.ToLower(ident)
			if seen[k] {
				continue
			}
			seen[k] = true
			if !strings.Contains(merged, k) {
				missing = append(missing, ident)
			}
		}
	}
	return missing
}

// clusterCheck is a cluster that lost identifiers.
type clusterCheck struct {
	cluster ClusterResult
	missing []string
}

// splitByDetail separates clusters that keep every identifier of their
// sources from those that don't.
func splitByDetail(clusters []ClusterResult, byID map[string]string) (ok []ClusterResult, bad []clusterCheck) {
	for _, cl := range clusters {
		if m := missingIdentifiers(cl, byID); len(m) > 0 {
			bad = append(bad, clusterCheck{cluster: cl, missing: m})
			continue
		}
		ok = append(ok, cl)
	}
	return ok, bad
}

// repairPrompt asks the model to rewrite the clusters that lost identifiers.
func repairPrompt(bad []clusterCheck) string {
	var sb strings.Builder
	sb.WriteString("Some merged entries dropped concrete details of their sources. A merged entry must keep every detail; ")
	sb.WriteString("density comes only from removing repetition, never from dropping facts.\n\n")
	sb.WriteString("Rewrite ONLY the clusters below. Each merged_content must contain every listed item copied exactly ")
	sb.WriteString("(you may restructure the sentence around them), plus the other facts of its sources.\n\n")
	for i, b := range bad {
		ids := append([]string(nil), b.cluster.SourceIDs...)
		sort.Strings(ids)
		missing := b.missing
		if len(missing) > maxMissingListed {
			missing = missing[:maxMissingListed]
		}
		fmt.Fprintf(&sb, "%d. source_ids %s, missing: %s\n", i+1, strings.Join(ids, ", "), strings.Join(missing, " | "))
	}
	sb.WriteString("\nOutput a JSON array with the corrected clusters only (same schema, same source_ids), nothing else. ")
	sb.WriteString("If a cluster cannot keep all its details in one entry, leave it out.")
	return sb.String()
}

// sourceKey identifies a cluster by its sorted source IDs.
func sourceKey(ids []string) string {
	s := append([]string(nil), ids...)
	sort.Strings(s)
	return strings.Join(s, "\x00")
}
