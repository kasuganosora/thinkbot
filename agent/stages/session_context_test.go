package stages

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// TestLLMStage_ToolExecCarriesTurnSessionContext：工具执行期能拿到本轮的会话上下文
// （平台 / 用户），spawn 据此让子代理按父回合身份做权限评估。
func TestLLMStage_ToolExecCarriesTurnSessionContext(t *testing.T) {
	var got agenttools.ToolSessionContext
	var ok bool
	exec := llm.Tool{
		Name: "exec", Description: "exec", Parameters: map[string]any{"type": "object"},
		Execute: func(ctx *llm.ToolExecContext, _ any) (any, error) {
			got, ok = agenttools.SessionContextFromContext(ctx)
			return "ok", nil
		},
	}
	prov := &turnRecordingProvider{steps: map[string][][]string{}}
	s := NewLLMStage("llm", prov, LLMConfig{
		Model: llm.ChatModel("m"), MaxSteps: 5, ToolResolver: sourceToolResolver{tg: []llm.Tool{exec}},
	}, nil, zap.NewNop().Sugar())

	env := core.NewEnvelope(core.Message{
		ID: "tg-1", BotID: "bot-1", Source: "telegram", Channel: "76017910", UserID: "76017910",
		ChatType: core.ChatPrivate, Text: "fix the code",
		Metadata: map[string]any{"channel_type": "telegram", "username": "sion"},
	})
	if _, err := s.Process(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("tool execution context has no session context")
	}
	if got.SourceChannelType != "telegram" || got.UserID != "76017910" || got.BotID != "bot-1" || got.IsSubagent {
		t.Fatalf("unexpected session context: %+v", got)
	}
	if len(got.UserIdentifiers) != 2 {
		t.Fatalf("user identifiers not carried: %+v", got.UserIdentifiers)
	}
}
