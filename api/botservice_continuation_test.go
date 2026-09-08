package api

import "testing"

// TestSplitSessionChannel 锁定工作流续跑的会话渠道解析逻辑：
// onWorkflowCompleted 依赖它把续跑指令路由到正确的渠道（TG 走真实渠道主动推回，
// web/未知退回 WebChannel）。解析错误会导致完成总结错投或丢失。
func TestSplitSessionChannel(t *testing.T) {
	cases := []struct {
		in     string
		kind   string
		target string
	}{
		{"tg:76019910", "tg", "76019910"},
		{"web:abc", "web", "abc"},
		{"mk:channel:user123", "mk", "channel:user123"},
		{"session-no-colon", "", "session-no-colon"},
		{"", "", ""},
	}
	for _, c := range cases {
		k, tgt := splitSessionChannel(c.in)
		if k != c.kind || tgt != c.target {
			t.Errorf("splitSessionChannel(%q) = (%q,%q), want (%q,%q)",
				c.in, k, tgt, c.kind, c.target)
		}
	}
}
