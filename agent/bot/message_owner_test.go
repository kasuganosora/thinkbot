package bot

import (
	"context"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"testing"
)

func TestBeforeProcessPinsMessageToRuntimeBot(t *testing.T) {
	b := &Bot{ID: "trusted", emitter: outbound.NewEventEmitter(nil, "trusted")}
	env := core.NewEnvelope(core.Message{BotID: "other-bot", Text: "fictional message"})
	b.OnBeforeProcess(context.Background(), env)
	if env.Message.BotID != "trusted" {
		t.Fatalf("pipeline retained inbound owner %q", env.Message.BotID)
	}
}
