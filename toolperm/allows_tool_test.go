package toolperm

import (
	"context"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// TestAllowsTool_MatchesFilterTools proves the single-tool probe used by
// sandbox_exec(background=auto) reaches exactly the same verdict as the
// tool-list filter. If these ever diverge, an "exec allowed / spawn denied"
// rule could be bypassed by auto-upgrading into a background session.
func TestAllowsTool_MatchesFilterTools(t *testing.T) {
	svc := newTestService(t)
	botID := "bot-probe"
	// Allow everything on telegram, then explicitly deny the spawn tool.
	if _, err := svc.CreateRule(botID, RuleReq{
		Tool: "*", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionAllow, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRule(botID, RuleReq{
		Tool: "sandbox_session_spawn", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}

	ev := svc.NewEvaluator()
	sctx := &agenttools.ToolSessionContext{
		BotID: botID, SourceChannelType: "telegram",
		ChatID: "chat-1", UserID: "u1",
		UserIdentifiers: []string{"u1"},
	}
	list := []llm.Tool{
		{Name: "sandbox_exec"},
		{Name: "sandbox_session_spawn"},
		{Name: "sandbox_session_poll"},
	}
	filtered, err := ev.FilterTools(context.Background(), list, sctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, tl := range filtered {
		kept[tl.Name] = true
	}

	probe, ok := ev.(agenttools.ToolAccessProbe)
	if !ok {
		t.Fatal("toolperm evaluator must implement ToolAccessProbe (zero-log fast path)")
	}
	for _, name := range []string{"sandbox_exec", "sandbox_session_spawn", "sandbox_session_poll"} {
		got := probe.AllowsTool(context.Background(), sctx, name)
		if got != kept[name] {
			t.Errorf("AllowsTool(%q)=%v but FilterTools kept=%v", name, got, kept[name])
		}
	}
	if probe.AllowsTool(context.Background(), sctx, "sandbox_session_spawn") {
		t.Error("spawn was denied by rule; auto-upgrade probe must report denied")
	}
	if !probe.AllowsTool(context.Background(), sctx, "sandbox_exec") {
		t.Error("exec should still be allowed")
	}
}

func TestAllowsTool_NilSessionContextAllows(t *testing.T) {
	svc := newTestService(t)
	ev := svc.NewEvaluator().(agenttools.ToolAccessProbe)
	// FilterTools treats a nil sctx as "cannot decide, allow"; the probe must
	// agree or a system/internal call would lose the capability unexpectedly.
	if !ev.AllowsTool(context.Background(), nil, "sandbox_session_spawn") {
		t.Error("nil sctx must match FilterTools' conservative allow")
	}
}

func TestAllowsTool_SystemSessionBypassesNonBroadcastRule(t *testing.T) {
	svc := newTestService(t)
	botID := "bot-sys"
	if _, err := svc.CreateRule(botID, RuleReq{
		Tool: "sandbox_session_spawn", Platform: "web", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}
	ev := svc.NewEvaluator().(agenttools.ToolAccessProbe)
	sctx := &agenttools.ToolSessionContext{
		BotID: botID, SourceChannelType: "web", IsSystem: true,
	}
	if !ev.AllowsTool(context.Background(), sctx, "sandbox_session_spawn") {
		t.Error("system sessions are exempt from non-broadcast tool rules in FilterTools; probe must match")
	}
}
