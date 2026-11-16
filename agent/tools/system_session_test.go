package tools

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestEnvelopeToSessionContext_IsSystemForCronAndHeartbeat(t *testing.T) {
	cronEnv := core.NewEnvelope(core.Message{
		ID:       "m-cron",
		BotID:    "bot-1",
		Source:   core.SourceCron,
		Channel:  "cron:job-1",
		ChatType: core.ChatPrivate,
		UserID:   "system:cron",
	})
	if sctx := envelopeToSessionContext(cronEnv); !sctx.IsSystem {
		t.Fatal("expected IsSystem=true for SourceCron envelope")
	}

	hbEnv := core.NewEnvelope(core.Message{
		ID:      "m-hb",
		BotID:   "bot-1",
		Source:  core.SourceHeartbeat,
		Channel: "heartbeat",
		UserID:  "system:heartbeat",
	})
	if sctx := envelopeToSessionContext(hbEnv); !sctx.IsSystem {
		t.Fatal("expected IsSystem=true for SourceHeartbeat envelope")
	}

	userEnv := core.NewEnvelope(core.Message{
		ID:       "m-user",
		BotID:    "bot-1",
		Source:   "tg-main",
		Channel:  "12345",
		ChatType: core.ChatPrivate,
		UserID:   "u-1",
	})
	if sctx := envelopeToSessionContext(userEnv); sctx.IsSystem {
		t.Fatal("expected IsSystem=false for normal user envelope")
	}
}
