package tools

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func makeTool(name string) llm.Tool {
	return llm.Tool{Name: name, Description: "test tool " + name}
}

func makeCtx(channel, chatType, userID string) *ToolSessionContext {
	return &ToolSessionContext{
		Channel:  channel,
		ChatType: chatType,
		UserID:   userID,
	}
}

func TestToolPolicy_NoRules(t *testing.T) {
	p := ToolPolicy{}
	if !p.IsAllowed("any_tool", "misskey", "group", "user1") {
		t.Error("expected all tools allowed with no rules")
	}
}

func TestToolPolicy_GlobalDisable(t *testing.T) {
	p := ToolPolicy{
		Rules: []ToolRule{
			{Disabled: []string{"dangerous_tool"}},
		},
	}

	if p.IsAllowed("dangerous_tool", "any", "any", "user1") {
		t.Error("expected dangerous_tool to be disabled")
	}
	if !p.IsAllowed("safe_tool", "any", "any", "user1") {
		t.Error("expected safe_tool to be allowed")
	}
}

func TestToolPolicy_ChatTypeScope(t *testing.T) {
	p := ToolPolicy{
		Rules: []ToolRule{
			{ChatType: "group", Disabled: []string{"admin_tool"}},
		},
	}

	// group → disabled
	if p.IsAllowed("admin_tool", "misskey", "group", "user1") {
		t.Error("expected admin_tool disabled in group")
	}
	// private → allowed
	if !p.IsAllowed("admin_tool", "misskey", "private", "user1") {
		t.Error("expected admin_tool allowed in private")
	}
}

func TestToolPolicy_ChannelScope(t *testing.T) {
	p := ToolPolicy{
		Rules: []ToolRule{
			{Channel: "telegram", Disabled: []string{"web_search"}},
		},
	}

	if p.IsAllowed("web_search", "telegram", "group", "user1") {
		t.Error("expected web_search disabled on telegram")
	}
	if !p.IsAllowed("web_search", "misskey", "group", "user1") {
		t.Error("expected web_search allowed on misskey")
	}
}

func TestToolPolicy_AllowedUsersBypass(t *testing.T) {
	p := ToolPolicy{
		Rules: []ToolRule{
			{
				ChatType:     "group",
				Disabled:     []string{"admin_tool"},
				AllowedUsers: []string{"admin_user", "vip_user"},
			},
		},
	}

	// Normal user in group → blocked
	if p.IsAllowed("admin_tool", "misskey", "group", "normal_user") {
		t.Error("expected admin_tool blocked for normal_user")
	}
	// Admin user in group → allowed via whitelist
	if !p.IsAllowed("admin_tool", "misskey", "group", "admin_user") {
		t.Error("expected admin_tool allowed for admin_user")
	}
	// VIP user in group → allowed
	if !p.IsAllowed("admin_tool", "misskey", "group", "vip_user") {
		t.Error("expected admin_tool allowed for vip_user")
	}
}

func TestToolPolicy_MultipleRules(t *testing.T) {
	p := ToolPolicy{
		Rules: []ToolRule{
			{
				Channel:  "telegram",
				Disabled: []string{"tool_a"},
			},
			{
				ChatType: "group",
				Disabled: []string{"tool_b"},
				AllowedUsers: []string{"trusted_user"},
			},
		},
	}

	// tool_a blocked on telegram group
	if p.IsAllowed("tool_a", "telegram", "group", "user1") {
		t.Error("expected tool_a blocked on telegram")
	}
	// tool_b blocked in group, but trusted_user allowed
	if p.IsAllowed("tool_b", "misskey", "group", "user1") {
		t.Error("expected tool_b blocked in group for normal user")
	}
	if !p.IsAllowed("tool_b", "misskey", "group", "trusted_user") {
		t.Error("expected tool_b allowed for trusted_user")
	}
	// tool_b allowed in private
	if !p.IsAllowed("tool_b", "misskey", "private", "user1") {
		t.Error("expected tool_b allowed in private")
	}
}

func TestToolPolicy_FilterTools(t *testing.T) {
	p := ToolPolicy{
		Rules: []ToolRule{
			{ChatType: "group", Disabled: []string{"tool_a", "tool_c"}},
		},
	}

	tools := []llm.Tool{
		makeTool("tool_a"),
		makeTool("tool_b"),
		makeTool("tool_c"),
		makeTool("tool_d"),
	}

	sctx := makeCtx("misskey", "group", "user1")
	result := p.FilterTools(tools, sctx)

	if len(result) != 2 {
		t.Fatalf("expected 2 tools after filter, got %d", len(result))
	}
	names := []string{result[0].Name, result[1].Name}
	if names[0] != "tool_b" || names[1] != "tool_d" {
		t.Errorf("unexpected tools: %v", names)
	}
}

func TestToolPolicy_FilterToolsWithWhitelist(t *testing.T) {
	p := ToolPolicy{
		Rules: []ToolRule{
			{
				ChatType:     "group",
				Disabled:     []string{"tool_a"},
				AllowedUsers: []string{"vip_user"},
			},
		},
	}

	tools := []llm.Tool{
		makeTool("tool_a"),
		makeTool("tool_b"),
	}

	// Normal user: tool_a filtered
	result := p.FilterTools(tools, makeCtx("misskey", "group", "normal_user"))
	if len(result) != 1 || result[0].Name != "tool_b" {
		t.Errorf("expected only tool_b for normal_user, got %v", result)
	}

	// VIP user: tool_a allowed
	result = p.FilterTools(tools, makeCtx("misskey", "group", "vip_user"))
	if len(result) != 2 {
		t.Errorf("expected both tools for vip_user, got %d", len(result))
	}
}

func TestToolPolicy_JSONRoundTrip(t *testing.T) {
	original := ToolPolicy{
		Rules: []ToolRule{
			{
				Channel:      "telegram",
				ChatType:     "group",
				Disabled:     []string{"tool_a", "tool_b"},
				AllowedUsers: []string{"admin"},
			},
		},
	}

	jsonStr, err := ToolPolicyJSON(original)
	if err != nil {
		t.Fatalf("ToolPolicyJSON failed: %v", err)
	}

	parsed := ParseToolPolicy(jsonStr)
	if len(parsed.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(parsed.Rules))
	}
	r := parsed.Rules[0]
	if r.Channel != "telegram" || r.ChatType != "group" {
		t.Errorf("unexpected channel/chatType: %s/%s", r.Channel, r.ChatType)
	}
	if len(r.Disabled) != 2 || r.Disabled[0] != "tool_a" {
		t.Errorf("unexpected disabled: %v", r.Disabled)
	}
	if len(r.AllowedUsers) != 1 || r.AllowedUsers[0] != "admin" {
		t.Errorf("unexpected allowedUsers: %v", r.AllowedUsers)
	}
}

func TestToolPolicy_ParseEmpty(t *testing.T) {
	p := ParseToolPolicy("")
	if len(p.Rules) != 0 {
		t.Error("expected empty policy for empty string")
	}
}

func TestToolPolicy_ParseInvalid(t *testing.T) {
	p := ParseToolPolicy("not valid json")
	if len(p.Rules) != 0 {
		t.Error("expected empty policy for invalid JSON")
	}
}

func TestToolPolicyFunc(t *testing.T) {
	policy := ToolPolicy{
		Rules: []ToolRule{
			{Disabled: []string{"blocked"}},
		},
	}

	provider := ToolPolicyFunc(func(botID string) ToolPolicy {
		if botID == "bot-001" {
			return policy
		}
		return ToolPolicy{}
	})

	if provider.GetPolicy("bot-001").IsAllowed("blocked", "", "", "") {
		t.Error("expected blocked for bot-001")
	}
	if !provider.GetPolicy("bot-002").IsAllowed("blocked", "", "", "") {
		t.Error("expected allowed for bot-002")
	}
}
