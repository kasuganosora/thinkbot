package skill

import (
	"errors"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func newSuggestManager() *SkillManager {
	mgr := NewSkillManager(nil, nil, nil)
	for _, s := range []*Skill{
		{Name: "ctrip-wendao", Description: "携程问道：机票酒店查询 travel search", Content: "x", Enabled: true},
		{Name: "amap-lbs-skill", Description: "高德地图 POI / route planning", Content: "x", Enabled: true},
		{Name: "bangumi", Description: "Bangumi anime database lookup", Content: "x", Enabled: true},
		{Name: "weather", Description: "weather forecast", Content: "x", Enabled: true},
		{Name: "hidden", Description: "travel stuff", Content: "x", Enabled: false},
	} {
		mgr.Register(s)
	}
	return mgr
}

func TestSuggestSkills(t *testing.T) {
	mgr := newSuggestManager()
	cases := []struct {
		query string
		want  string
	}{
		{"ctrip", "ctrip-wendao"},           // name token
		{"ctrip-wendao-v2", "ctrip-wendao"}, // lenient OR over name tokens
		{"bangumy", "bangumi"},              // typo (edit distance)
		{"携程", "ctrip-wendao"},              // description (CJK)
		{"高德 skill", "amap-lbs-skill"},      // "skill" ignored
	}
	for _, c := range cases {
		got := mgr.SuggestSkills(c.query, 5)
		if len(got) == 0 || got[0] != c.want {
			t.Errorf("SuggestSkills(%q) = %v, want first %q", c.query, got, c.want)
		}
		for _, n := range got {
			if n == "hidden" {
				t.Errorf("SuggestSkills(%q) returned disabled skill", c.query)
			}
		}
	}
	if got := mgr.SuggestSkills("zzzz qqqq", 5); len(got) != 0 {
		t.Errorf("unrelated query should suggest nothing, got %v", got)
	}
}

func TestUseSkillNotFoundSuggests(t *testing.T) {
	mgr := newSuggestManager()
	_, err := mgr.UseSkill("ctrip")
	var nf *SkillNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected SkillNotFoundError, got %v", err)
	}
	if len(nf.Suggestions) == 0 || nf.Suggestions[0] != "ctrip-wendao" {
		t.Fatalf("suggestions = %v", nf.Suggestions)
	}
	msg := err.Error()
	for _, want := range []string{"skill_search", "ctrip-wendao", "not found"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
	// No suggestions: still guides to skill_search, no catalog dump.
	_, err = mgr.UseSkill("zzzz")
	if err == nil || !strings.Contains(err.Error(), "skill_search") || strings.Contains(err.Error(), "weather") {
		t.Errorf("unexpected error for unknown name: %v", err)
	}
}

func TestSkillSearchEmptyResultSuggests(t *testing.T) {
	mgr := newSuggestManager()
	tool := mgr.BuildSkillSearchTool()
	res, err := tool.Execute(&llm.ToolExecContext{}, SkillSearchInput{Query: "bangumi calendar"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	sugg, _ := m["suggestions"].([]string)
	if len(sugg) == 0 || sugg[0] != "bangumi" {
		t.Fatalf("expected bangumi suggestion on strict miss, got %v (%v)", m["suggestions"], m)
	}
	if tool.DiscoveryProbe == nil {
		t.Fatal("skill_search should expose a DiscoveryProbe for tool_search")
	}
	if got := tool.DiscoveryProbe("携程"); len(got) == 0 || got[0] != "ctrip-wendao" {
		t.Errorf("probe(携程) = %v", got)
	}
}

func TestSkillToolDescriptionsRequireSearchFirst(t *testing.T) {
	mgr := newSuggestManager()
	use := mgr.BuildUseSkillTool()
	if !strings.Contains(use.Description, "skill_search") || !strings.Contains(use.Description, "tool_search") {
		t.Errorf("use_skill description should require skill_search and rule out tool_search: %q", use.Description)
	}
	if p := mgr.BuildTriggerPrompt(); p != "" && !strings.Contains(p, "tool_search") {
		t.Errorf("trigger prompt should say tool_search never finds skills: %q", p)
	}
}
