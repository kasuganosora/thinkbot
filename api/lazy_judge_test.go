package api

import "testing"

// TestParseLazyJudgeJSON 验证裁决 JSON 提取的健壮性：容忍 ```json 围栏、
// 前后散文、纯散文（应报错）。
func TestParseLazyJudgeJSON(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantLazy bool
		wantErr  bool
	}{
		{"plain", `{"lazy":true,"entity_asserted":true,"confidence":0.9,"reason":"x"}`, true, false},
		{"fenced", "```json\n{\"lazy\":false,\"entity_asserted\":false,\"confidence\":0.2,\"reason\":\"concept\"}\n```", false, false},
		{"prose-wrap", "Sure, here is my judgment:\n{\"lazy\":true,\"confidence\":0.8,\"reason\":\"no tool\"} hope this helps", true, false},
		{"garbage", "no json object here at all", false, true},
		{"empty", "", false, true},
	}
	for _, c := range cases {
		r, err := parseLazyJudgeJSON(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got %+v", c.name, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if r.Lazy != c.wantLazy {
			t.Errorf("%s: lazy=%v want %v", c.name, r.Lazy, c.wantLazy)
		}
	}
}
