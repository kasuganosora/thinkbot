package tools

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestNormalizeChatType 固化会话类型归一化：Telegram 超级群必须归入 group。
func TestNormalizeChatType(t *testing.T) {
	cases := map[string]string{
		core.ChatSupergroup: core.ChatGroup,
		core.ChatGroup:      core.ChatGroup,
		core.ChatPrivate:    core.ChatPrivate,
		core.ChatChannel:    core.ChatChannel,
		"unknown":           "unknown",
		"":                  "",
	}
	for in, want := range cases {
		if got := core.NormalizeChatType(in); got != want {
			t.Errorf("NormalizeChatType(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAppliesToSupergroup 固化 P0 回归：Scopes 含 "group" 的工具必须在 supergroup 可用。
//
// 背景：Telegram 绝大多数活跃群都是 supergroup。修复前 appliesTo 按字面量
// == "group" 匹配，导致 memory 等 7 处 group 类工具在超级群里被整体剔除（bot 失忆）。
func TestAppliesToSupergroup(t *testing.T) {
	def := &ToolDef{Scopes: []string{"private", "group"}}

	for _, ct := range []string{core.ChatGroup, core.ChatSupergroup} {
		sctx := &ToolSessionContext{ChatType: ct}
		if !def.appliesTo(sctx) {
			t.Errorf("tool with group scope must apply to ChatType %q", ct)
		}
	}

	// 私聊与非群聊仍应正确区分
	if !(&ToolDef{Scopes: []string{"private"}}).appliesTo(&ToolSessionContext{ChatType: core.ChatPrivate}) {
		t.Error("private-scoped tool must apply to private chat")
	}
	if (&ToolDef{Scopes: []string{"private"}}).appliesTo(&ToolSessionContext{ChatType: core.ChatSupergroup}) {
		t.Error("private-scoped tool must NOT apply to supergroup")
	}
	if def.appliesTo(&ToolSessionContext{ChatType: core.ChatChannel}) {
		t.Error("group-scoped tool must NOT apply to channel")
	}
}

// TestPolicyChatTypeSupergroup 固化策略层的同类回归：配 "group" 的规则须命中 supergroup，反之亦然。
func TestPolicyChatTypeSupergroup(t *testing.T) {
	if !(ToolRule{ChatType: core.ChatGroup}).matches("", core.ChatSupergroup) {
		t.Error(`rule with ChatType "group" must match supergroup`)
	}
	if !(ToolRule{ChatType: core.ChatSupergroup}).matches("", core.ChatGroup) {
		t.Error(`rule with ChatType "supergroup" must match group`)
	}
	if (ToolRule{ChatType: core.ChatGroup}).matches("", core.ChatPrivate) {
		t.Error(`rule with ChatType "group" must NOT match private`)
	}
	// 同 PatternRule
	if !(&PatternRule{ChatType: core.ChatGroup}).matchesContext("", core.ChatSupergroup) {
		t.Error(`pattern rule with ChatType "group" must match supergroup`)
	}
}
