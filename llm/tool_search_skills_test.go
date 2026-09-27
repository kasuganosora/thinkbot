package llm

import (
	"strings"
	"testing"
)

func skillSearchTestTool(probe func(string) []string) Tool {
	t := deferTestTool("skill_search", false)
	t.DiscoveryProbe = probe
	return t
}

func runToolSearch(t *testing.T, d *ToolDeferral, q string) string {
	t.Helper()
	out, err := d.searchTool().Execute(&ToolExecContext{}, map[string]any{"query": q})
	if err != nil {
		t.Fatal(err)
	}
	return out.(string)
}

func TestToolSearch_SkillQueryRedirects(t *testing.T) {
	d := NewToolDeferral(true)
	var probed string
	d.SetTools([]Tool{
		deferTestTool("exec", false),
		deferTestTool("mcp__srv__skillful", true), // would match "skill" as a tool
		skillSearchTestTool(func(q string) []string { probed = q; return []string{"bangumi"} }),
	})
	out := runToolSearch(t, d, "bangumi skill")
	if !strings.Contains(out, "skill_search") || !strings.Contains(out, "never finds Skills") {
		t.Fatalf("expected redirect to skill_search, got %q", out)
	}
	if !strings.Contains(out, "bangumi") || probed != "bangumi" {
		t.Errorf("expected probe with stripped query and hit listed, probed=%q out=%q", probed, out)
	}
	if d.loaded["mcp__srv__skillful"] {
		t.Error("skill query must not load deferred tools")
	}
	if !strings.Contains(d.searchTool().Description, "never finds Skills") {
		t.Error("tool_search description should carry the skill note when skill_search is callable")
	}
}

func TestToolSearch_NoSkillSearchNoRedirect(t *testing.T) {
	d := NewToolDeferral(true)
	d.SetTools([]Tool{deferTestTool("exec", false), deferTestTool("mcp__srv__foo", true)})
	out := runToolSearch(t, d, "skill")
	if strings.Contains(out, "skill_search") {
		t.Errorf("must not point to skill_search when it is not callable: %q", out)
	}
	if strings.Contains(d.searchTool().Description, "Skills") {
		t.Error("no skill note without skill_search")
	}
}

func TestToolSearch_NoToolMatchButSkillProbe(t *testing.T) {
	d := NewToolDeferral(true)
	d.SetTools([]Tool{
		deferTestTool("exec", false),
		deferTestTool("mcp__srv__foo", true),
		skillSearchTestTool(func(q string) []string {
			if q == "携程" {
				return []string{"ctrip-wendao"}
			}
			return nil
		}),
	})
	out := runToolSearch(t, d, "携程")
	if !strings.Contains(out, "ctrip-wendao") || !strings.Contains(out, "skill_search") {
		t.Fatalf("expected skill hint, got %q", out)
	}
	// Tool hits are not diluted with skill hints.
	out = runToolSearch(t, d, "foo")
	if strings.Contains(out, "Skills that look relevant") {
		t.Errorf("tool hit should not carry skill hints: %q", out)
	}
}

func TestToolSearch_DeferredSkillSearchNotUsed(t *testing.T) {
	d := NewToolDeferral(true)
	ss := skillSearchTestTool(func(string) []string { return []string{"x"} })
	ss.DeferredLoad = true
	d.SetTools([]Tool{deferTestTool("exec", false), ss})
	if d.hasSkillSearch() {
		t.Fatal("unloaded deferred skill_search is not callable")
	}
}
