package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
)

func newReactionTGChannel(t *testing.T) (*TelegramChannel, *inbound.Ingress) {
	t.Helper()
	ing := inbound.NewIngress(
		inbound.IngressConfig{BufferSize: 8},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)
	t.Cleanup(ing.Close)
	ch := &TelegramChannel{
		name:      "tg",
		botID:     "bot-1",
		botUserID: 999,
		ingress:   ing,
	}
	return ch, ing
}

func drainTG(t *testing.T, ing *inbound.Ingress) *core.Envelope {
	t.Helper()
	select {
	case env := <-ing.C():
		if env == nil {
			t.Fatal("nil envelope")
		}
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ingress")
		return nil
	}
}

func TestHandleMessageReaction_BuildsAwareness(t *testing.T) {
	ch, ing := newReactionTGChannel(t)
	ch.handleUpdate(context.Background(), Update{
		UpdateID: 42,
		MessageReaction: &MessageReactionUpdated{
			Chat:        Chat{ID: 1001, Type: "private"},
			MessageID:   77,
			User:        &User{ID: 55, FirstName: "Luna", Username: "luna"},
			Date:        1700000000,
			OldReaction: nil,
			NewReaction: []ReactionType{{Type: "emoji", Emoji: "❤️"}},
		},
	})
	env := drainTG(t, ing)
	msg := env.Message
	if msg.Text != "" {
		t.Fatalf("Text must be empty, got %q", msg.Text)
	}
	if !strings.Contains(msg.InjectContext, "[Telegram 反应]") {
		t.Fatalf("missing marker: %q", msg.InjectContext)
	}
	if !strings.Contains(msg.InjectContext, "❤️") || !strings.Contains(msg.InjectContext, "@luna") {
		t.Fatalf("missing emoji/user: %q", msg.InjectContext)
	}
	if !strings.Contains(msg.InjectContext, "不要回复") {
		t.Fatalf("missing guidance: %q", msg.InjectContext)
	}
	if msg.Metadata["event_type"] != "reaction" || msg.Metadata["ack_only"] != true {
		t.Fatalf("metadata=%v", msg.Metadata)
	}
	if ids, _ := msg.Metadata["reactor_ids"].([]string); len(ids) != 1 || ids[0] != "55" {
		t.Fatalf("reactor_ids=%v", msg.Metadata["reactor_ids"])
	}
	if _, ok := msg.Metadata["reply_target"]; ok {
		t.Fatal("reply_target must not be set")
	}
	if msg.Channel != "1001" || msg.UserID != "55" {
		t.Fatalf("Channel/UserID=%q/%q", msg.Channel, msg.UserID)
	}
	if msg.Mentioned {
		t.Fatal("Mentioned must be false")
	}
}

func TestHandleMessageReaction_IgnoresRemovalOnly(t *testing.T) {
	ch, ing := newReactionTGChannel(t)
	ch.handleMessageReaction(context.Background(), 1, &MessageReactionUpdated{
		Chat:        Chat{ID: 1, Type: "private"},
		MessageID:   2,
		User:        &User{ID: 3, FirstName: "A"},
		OldReaction: []ReactionType{{Type: "emoji", Emoji: "👍"}},
		NewReaction: nil,
	})
	select {
	case <-ing.C():
		t.Fatal("removal-only must not ingress")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestHandleMessageReaction_IgnoresSelf(t *testing.T) {
	ch, ing := newReactionTGChannel(t)
	ch.handleMessageReaction(context.Background(), 1, &MessageReactionUpdated{
		Chat:        Chat{ID: 1, Type: "private"},
		MessageID:   2,
		User:        &User{ID: ch.botUserID, FirstName: "Bot"},
		NewReaction: []ReactionType{{Type: "emoji", Emoji: "👍"}},
	})
	select {
	case <-ing.C():
		t.Fatal("self reaction must not ingress")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestReactionDiff(t *testing.T) {
	old := []ReactionType{{Type: "emoji", Emoji: "👍"}}
	neu := []ReactionType{{Type: "emoji", Emoji: "👍"}, {Type: "emoji", Emoji: "❤️"}}
	got := reactionDiff(neu, old)
	if len(got) != 1 || got[0].Emoji != "❤️" {
		t.Fatalf("got=%v", got)
	}
}
