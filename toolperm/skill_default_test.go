package toolperm

import "testing"

// TestSkillToolsDefaultAllowed: skill_search / use_skill are basic tools, so
// they stay available on every platform — including platforms already in
// whitelist mode (e.g. telegram/misskey with explicit rules, where skill_search
// used to be denied until an admin added a rule by hand) — while an explicit
// deny still wins.
func TestSkillToolsDefaultAllowed(t *testing.T) {
	svc := newTestService(t)
	for _, tool := range []string{"skill_search", "use_skill"} {
		if ToolRisk(tool) != RiskBasic {
			t.Fatalf("%s should be basic, got %s", tool, ToolRisk(tool))
		}
		// platform without rules
		if !svc.Evaluate("bot-s", tool, "misskey", "u1") {
			t.Errorf("%s must be allowed on a platform without rules", tool)
		}
	}

	// telegram in whitelist mode (an allow rule for some other tool)
	if _, err := svc.CreateRule("bot-s", RuleReq{
		Tool: "web_search", Platform: "telegram", UserIDs: []string{"admin"},
		Decision: DecisionAllow, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"skill_search", "use_skill"} {
		if !svc.Evaluate("bot-s", tool, "telegram", "someone") {
			t.Errorf("%s must be allowed on a whitelist-mode platform", tool)
		}
	}

	// explicit deny wins
	if _, err := svc.CreateRule("bot-s", RuleReq{
		Tool: "use_skill", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}
	if svc.Evaluate("bot-s", "use_skill", "telegram", "someone") {
		t.Error("explicit deny of use_skill must win over the basic default")
	}
	// tool=* deny locks everything, basic tools included
	if _, err := svc.CreateRule("bot-s", RuleReq{
		Tool: "*", Platform: "misskey", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-2),
	}); err != nil {
		t.Fatal(err)
	}
	if svc.Evaluate("bot-s", "skill_search", "misskey", "u1") {
		t.Error("tool=* deny must also deny skill_search")
	}
}
