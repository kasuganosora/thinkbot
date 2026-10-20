package memory

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestFillScopeIDFromSession(t *testing.T) {
	ctx := &llm.ToolExecContext{Context: tools.ContextWithMessageMeta(context.Background(),
		tools.MessageMeta{BotID: "b1", UserID: "u1", ChatID: "timeline"})}
	cases := []struct {
		in   Scope
		want string
	}{
		{Scope{Kind: ScopeChannel}, "timeline"},
		{Scope{Kind: ScopeUser}, "u1"},
		{Scope{Kind: ScopeBot}, "cfg-bot"},
		{Scope{Kind: ScopeGlobal}, ""},
		{Scope{Kind: ScopeChannel, ID: "explicit"}, "explicit"},
	}
	for _, c := range cases {
		if got := fillScopeIDFromSession(ctx, c.in, "cfg-bot"); got.ID != c.want {
			t.Errorf("%v: got %q want %q", c.in, got.ID, c.want)
		}
	}
	// no message meta / bare ToolExecContext: unchanged, no panic
	if got := fillScopeIDFromSession(&llm.ToolExecContext{}, Scope{Kind: ScopeChannel}, ""); got.ID != "" {
		t.Fatalf("bare context: %q", got.ID)
	}
}
