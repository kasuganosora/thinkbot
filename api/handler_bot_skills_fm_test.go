package api

import "testing"

// The admin page must decode names exactly like the skill loader.
func TestParseSkillFrontMatter_YAMLQuotes(t *testing.T) {
	name, desc := parseSkillFrontMatter("---\nname: \"12306\"\ndescription: '查询 12306' # zh\n---\nbody")
	if name != "12306" || desc != "查询 12306" {
		t.Fatalf("name=%q desc=%q", name, desc)
	}
	if n, _ := parseSkillFrontMatter("no front matter"); n != "" {
		t.Fatalf("name = %q", n)
	}
}
