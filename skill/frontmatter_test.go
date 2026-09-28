package skill

import (
	"reflect"
	"testing"
)

// The sandbox skill 12306 on prod (09-28): `name: "12306"` registered the skill
// as `"12306"` and its enabled-state key skill."12306".enabled was rejected.
const skill12306 = `---
name: "12306"
description: Query China Railway 12306 for train schedules, remaining tickets, and station info. Use when user asks about train/高铁/火车 tickets, schedules, or availability within China.
description_zh: "查询 12306 国内列车时刻、余票与站点信息"
description_en: "Query China Railway 12306 train schedules and ticket availability"
---

# 12306
`

func TestFrontMatter_Quoted12306(t *testing.T) {
	s, err := newSkillFromContent(skill12306, "workspace", "/data/skills/12306")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "12306" {
		t.Fatalf("name = %q, want 12306", s.Name)
	}
	if want := "Query China Railway 12306 for train schedules, remaining tickets, and station info. Use when user asks about train/高铁/火车 tickets, schedules, or availability within China."; s.Description != want {
		t.Fatalf("description = %q", s.Description)
	}
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(s)
	if _, err := mgr.UseSkill("12306"); err != nil {
		t.Fatalf("use_skill 12306: %v", err)
	}
}

func TestFrontMatter_Scalars(t *testing.T) {
	cases := []struct {
		name, fm string
		want     SkillMeta
	}{
		{"single quotes with escaped quote", "name: 'it''s-a-skill'\ndescription: 'd'", SkillMeta{Name: "it's-a-skill", Description: "d"}},
		{"double quotes with escapes", `name: "a\"b"` + "\n" + `description: "caf\u00e9\tx"`, SkillMeta{Name: `a"b`, Description: "café\tx"}},
		{"trailing comments", "name: pdf   # the pdf skill\ndescription: \"PDF tools\" # quoted\ndelegation: preferred # big", SkillMeta{Name: "pdf", Description: "PDF tools", Delegation: "preferred"}},
		{"quoted delegation", "name: x\ndescription: y\ndelegation: \"preferred\"", SkillMeta{Name: "x", Description: "y", Delegation: "preferred"}},
		{"single-quoted delegation", "name: x\ndescription: y\ndelegation: 'preferred'", SkillMeta{Name: "x", Description: "y", Delegation: "preferred"}},
		{"surrounding whitespace", "name:    spaced   \ndescription:   text  ", SkillMeta{Name: "spaced", Description: "text"}},
		{"hash inside a word is not a comment", "name: csharp\ndescription: Use C# here", SkillMeta{Name: "csharp", Description: "Use C# here"}},
		{"nested keys are ignored", "name: top\nmetadata:\n  name: nested\n  description: nested\ndescription: top-desc", SkillMeta{Name: "top", Description: "top-desc"}},
		{"compatibility flow list", "name: x\ndescription: y\ncompatibility: [\"exec\", 'read_file']", SkillMeta{Name: "x", Description: "y", Compatibility: []string{"exec", "read_file"}}},
		{"compatibility block list", "name: x\ndescription: y\ncompatibility:\n  - exec\n  - \"read_file\"", SkillMeta{Name: "x", Description: "y", Compatibility: []string{"exec", "read_file"}}},
		{"compatibility comma string", "name: x\ndescription: y\ncompatibility: exec, read_file", SkillMeta{Name: "x", Description: "y", Compatibility: []string{"exec", "read_file"}}},
		{"null description", "name: x\ndescription: ~", SkillMeta{Name: "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta, body := parseFrontMatter("---\n" + c.fm + "\n---\nbody\n")
			if body != "body\n" {
				t.Fatalf("body = %q", body)
			}
			if !reflect.DeepEqual(meta, c.want) {
				t.Fatalf("meta = %+v, want %+v", meta, c.want)
			}
		})
	}
}

func TestFrontMatter_EnabledQuotedAndPlain(t *testing.T) {
	for _, fm := range []string{"enabled: false", `enabled: "false"`, "enabled: 'no' # off"} {
		meta, _ := parseFrontMatter("---\nname: x\ndescription: y\n" + fm + "\n---\n")
		if meta.Enabled == nil || *meta.Enabled {
			t.Errorf("%s: enabled = %v, want false", fm, meta.Enabled)
		}
	}
}

// Front matter that is not valid YAML (an unquoted value containing ": ")
// goes through the line parser, which must decode scalars the same way.
func TestFrontMatter_FallbackForInvalidYAML(t *testing.T) {
	doc := "---\nname: \"12306\"\ndescription: Trains: schedules and tickets # comment\ndelegation: 'preferred'\n---\nbody"
	var probe SkillMeta
	if decodeFrontMatterYAML("name: \"12306\"\ndescription: Trains: schedules and tickets\n", &probe) {
		t.Fatal("fixture must be invalid YAML to exercise the fallback")
	}
	meta, body := parseFrontMatter(doc)
	if meta.Name != "12306" || meta.Description != "Trains: schedules and tickets" || meta.Delegation != "preferred" {
		t.Fatalf("fallback meta = %+v", meta)
	}
	if body != "body" {
		t.Fatalf("body = %q", body)
	}
}

func TestFrontMatter_QuotedDelegationMakesSkillHeavy(t *testing.T) {
	s, err := newSkillFromContent("---\nname: \"small\"\ndescription: \"tiny\"\ndelegation: \"preferred\"\n---\nshort body", "fs", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "small" || !s.IsHeavy() {
		t.Fatalf("name=%q heavy=%v", s.Name, s.IsHeavy())
	}
}

func TestFrontMatter_BOMAndCRLF(t *testing.T) {
	meta, body := parseFrontMatter("\ufeff---\r\nname: \"12306\"\r\ndescription: 'x'\r\n---\r\nbody")
	if meta.Name != "12306" || meta.Description != "x" || body != "body" {
		t.Fatalf("meta=%+v body=%q", meta, body)
	}
}

func TestYAMLScalarFallback(t *testing.T) {
	cases := map[string]string{
		`"a: b" trailing`:  "a: b",
		`'it''s: x' tail`:  "it's: x",
		`plain: x # c`:     "plain: x",
		`  "12306"  `:      "12306",
		`value#nocomment`:  "value#nocomment",
		`"unterminated`:    `"unterminated`,
		`Trains: go # now`: "Trains: go",
	}
	for in, want := range cases {
		if got := yamlScalar(in); got != want {
			t.Errorf("yamlScalar(%q) = %q, want %q", in, got, want)
		}
	}
}
