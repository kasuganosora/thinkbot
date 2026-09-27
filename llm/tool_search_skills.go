package llm

import (
	"fmt"
	"strings"
)

// ============================================================================
// tool_search ↔ skill_search separation
//
// tool_search discovers lazily-loaded TOOLS. Skills are a different kind of
// capability (instruction packages) that are discovered with skill_search and
// loaded with use_skill. In production the model repeatedly used tool_search
// to look for skills ("skill", "skill_search", "bangumi"), got "No tool
// matches" back, and concluded the skill did not exist — or mixed
// tool_search and skill_search in one turn. tool_search therefore:
//   - says in its description that it never finds skills (only when a
//     skill_search tool is actually callable in this run);
//   - answers a skill-related query with a redirect to skill_search instead
//     of searching tools;
//   - when nothing (or something) matched but a tool's DiscoveryProbe reports
//     matching skills, names those skills and the tool to use.
// ============================================================================

// skillSearchToolName is the name of the skill discovery tool (package skill).
// llm cannot import skill (skill imports llm), so the name is repeated here.
const skillSearchToolName = "skill_search"

// skillQueryTerms mark a tool_search query as being about skills.
var skillQueryTerms = []string{"skill", "技能"}

// isSkillQuery reports whether a tool_search query is really a skill lookup.
func isSkillQuery(query string) bool {
	q := strings.ToLower(query)
	for _, t := range skillQueryTerms {
		if strings.Contains(q, t) {
			return true
		}
	}
	return false
}

// stripSkillTerms removes skill-marker words from a query so the remainder can
// be handed to a skill probe ("bangumi skill" → "bangumi").
func stripSkillTerms(query string) string {
	fields := strings.Fields(strings.ToLower(query))
	out := fields[:0]
	for _, f := range fields {
		switch f {
		case "skill", "skills", "技能", "skill_search", "use_skill", "search", "find", "查找", "搜索":
			continue
		}
		f = strings.ReplaceAll(f, "技能", "")
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// callableSkillSearchLocked returns the skill_search tool when it is directly
// callable in this run (present and not an unloaded deferred tool), else nil.
// Caller must hold d.mu.
func (d *ToolDeferral) callableSkillSearchLocked() *Tool {
	for i := range d.full {
		t := &d.full[i]
		if t.Name != skillSearchToolName {
			continue
		}
		if t.DeferredLoad && !d.loaded[t.Name] {
			return nil
		}
		return t
	}
	return nil
}

// hasSkillSearch reports whether skill_search is callable in this run.
func (d *ToolDeferral) hasSkillSearch() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.callableSkillSearchLocked() != nil
}

// probeHit is a non-tool capability reported by a tool's DiscoveryProbe.
type probeHit struct {
	tool  string   // tool that reaches the capability (e.g. skill_search)
	names []string // capability names (e.g. skill names)
}

// maxProbeNames caps how many probe names are listed in one reply.
const maxProbeNames = 8

// probe asks every callable tool with a DiscoveryProbe about query. Probes run
// outside d.mu (they may take their own locks).
func (d *ToolDeferral) probe(query string) []probeHit {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	d.mu.Lock()
	type probeFn struct {
		tool string
		fn   func(string) []string
	}
	var fns []probeFn
	for i := range d.full {
		t := d.full[i]
		if t.DiscoveryProbe == nil || (t.DeferredLoad && !d.loaded[t.Name]) {
			continue
		}
		fns = append(fns, probeFn{tool: t.Name, fn: t.DiscoveryProbe})
	}
	d.mu.Unlock()

	var hits []probeHit
	for _, p := range fns {
		names := p.fn(query)
		if len(names) == 0 {
			continue
		}
		if len(names) > maxProbeNames {
			names = names[:maxProbeNames]
		}
		hits = append(hits, probeHit{tool: p.tool, names: names})
	}
	return hits
}

// skillRedirectReply answers a skill-related tool_search query.
func skillRedirectReply(query string, hits []probeHit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tool_search only finds tools; it never finds Skills, so %q was not searched here. ", query)
	b.WriteString("To find a Skill, call skill_search with a few keywords describing the task (e.g. the site, file format or domain), ")
	b.WriteString("then call use_skill with an exact name from its results. skill_search is already in your tool list — call it directly, do not tool_search for it.")
	writeProbeHits(&b, hits)
	return b.String()
}

// writeProbeHits appends "matching skills" lines for probe hits.
func writeProbeHits(b *strings.Builder, hits []probeHit) {
	for _, h := range hits {
		if h.tool == skillSearchToolName {
			fmt.Fprintf(b, "\nSkills that look relevant: %s. Confirm with skill_search, then load one with use_skill.", strings.Join(h.names, ", "))
			continue
		}
		fmt.Fprintf(b, "\n%s reports matches: %s. Call %s to use them.", h.tool, strings.Join(h.names, ", "), h.tool)
	}
}

// toolSearchSkillNote is appended to the tool_search description when
// skill_search is callable, so the model never uses tool_search for skills.
const toolSearchSkillNote = " tool_search only finds TOOLS: it never finds Skills. To find a Skill use skill_search, then load it with use_skill."
