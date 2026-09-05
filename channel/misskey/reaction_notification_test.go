package misskey

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
)

func newReactionTestChannel(t *testing.T) (*MisskeyChannel, *inbound.Ingress) {
	t.Helper()
	ing := inbound.NewIngress(
		inbound.IngressConfig{BufferSize: 8},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)
	t.Cleanup(ing.Close)
	c := &MisskeyChannel{
		name:      "mk",
		botID:     "bot-1",
		botUserID: "bot-user",
		ingress:   ing,
		dedup:     map[string]time.Time{},
	}
	return c, ing
}

func drainOne(t *testing.T, ing *inbound.Ingress) *core.Envelope {
	t.Helper()
	select {
	case env := <-ing.C():
		if env == nil {
			t.Fatal("nil envelope")
		}
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ingress message")
		return nil
	}
}

func TestHandleNotification_ReactionBuildsAwarenessMessage(t *testing.T) {
	c, ing := newReactionTestChannel(t)
	body, _ := json.Marshal(map[string]any{
		"id":        "notif-1",
		"createdAt": "2026-09-05T04:00:00.000Z",
		"type":      "reaction",
		"userId":    "user-alice",
		"user": map[string]any{
			"id": "user-alice", "username": "alice", "name": "Alice", "isBot": false,
		},
		"reaction": "❤️",
		"note": map[string]any{
			"id": "note-bot-1", "userId": "bot-user",
			"user": map[string]any{"id": "bot-user", "username": "thinkbot"},
		},
	})
	c.handleNotification(context.Background(), body)

	env := drainOne(t, ing)
	msg := env.Message
	if msg.ID != "notif-1" {
		t.Fatalf("ID=%q", msg.ID)
	}
	if msg.Text != "" {
		t.Fatalf("Text must be empty, got %q", msg.Text)
	}
	if !strings.Contains(msg.InjectContext, "[Misskey 反应]") {
		t.Fatalf("InjectContext missing marker: %q", msg.InjectContext)
	}
	if !strings.Contains(msg.InjectContext, "@alice") || !strings.Contains(msg.InjectContext, "❤️") {
		t.Fatalf("InjectContext missing reactor/emoji: %q", msg.InjectContext)
	}
	if !strings.Contains(msg.InjectContext, "[note_id: note-bot-1]") {
		t.Fatalf("InjectContext missing note_id: %q", msg.InjectContext)
	}
	if !strings.Contains(msg.InjectContext, "不要回复") {
		t.Fatalf("InjectContext missing soft guidance: %q", msg.InjectContext)
	}
	if msg.Channel != "user-alice" || msg.UserID != "user-alice" {
		t.Fatalf("Channel/UserID = %q/%q", msg.Channel, msg.UserID)
	}
	if msg.ChatType != core.ChatPrivate {
		t.Fatalf("ChatType=%q", msg.ChatType)
	}
	if msg.Mentioned {
		t.Fatal("Mentioned must be false")
	}
	if msg.Metadata["event_type"] != "reaction" {
		t.Fatalf("event_type=%v", msg.Metadata["event_type"])
	}
	if msg.Metadata["ack_only"] != true {
		t.Fatalf("ack_only=%v", msg.Metadata["ack_only"])
	}
	if msg.Metadata["note_id"] != "note-bot-1" {
		t.Fatalf("note_id=%v", msg.Metadata["note_id"])
	}
	if msg.Metadata["reaction"] != "❤️" {
		t.Fatalf("reaction=%v", msg.Metadata["reaction"])
	}
	if msg.Metadata["notification_id"] != "notif-1" {
		t.Fatalf("notification_id=%v", msg.Metadata["notification_id"])
	}
	if _, ok := msg.Metadata["reply_target"]; ok {
		t.Fatal("reply_target must NOT be set")
	}
}

func TestHandleNotification_IgnoresNonReactionTypes(t *testing.T) {
	c, ing := newReactionTestChannel(t)
	for _, typ := range []string{"follow", "mention", "renote", "pollEnded"} {
		body, _ := json.Marshal(map[string]any{
			"id": "n-" + typ, "type": typ, "userId": "u1",
			"user": map[string]any{"id": "u1", "username": "x"},
			"note": map[string]any{"id": "note-1", "userId": "bot-user"},
		})
		c.handleNotification(context.Background(), body)
	}
	if ing.Len() != 0 {
		t.Fatalf("non-reaction notifications must be ignored, got %d", ing.Len())
	}
}

func TestHandleNotification_IgnoresSelfReaction(t *testing.T) {
	c, ing := newReactionTestChannel(t)
	body, _ := json.Marshal(map[string]any{
		"id": "notif-self", "type": "reaction", "userId": "bot-user",
		"user":     map[string]any{"id": "bot-user", "username": "thinkbot"},
		"reaction": "👍",
		"note":     map[string]any{"id": "note-1", "userId": "bot-user"},
	})
	c.handleNotification(context.Background(), body)
	if ing.Len() != 0 {
		t.Fatal("self-reaction must be ignored")
	}
}

func TestHandleNotification_GroupedPacksReactors(t *testing.T) {
	c, ing := newReactionTestChannel(t)
	body, _ := json.Marshal(map[string]any{
		"id":   "notif-g1",
		"type": "reaction:grouped",
		"note": map[string]any{"id": "note-g", "userId": "bot-user"},
		"reactions": []map[string]any{
			{"user": map[string]any{"id": "u1", "username": "alice"}, "reaction": "❤️"},
			{"user": map[string]any{"id": "u2", "username": "bob"}, "reaction": "👍"},
			{"user": map[string]any{"id": "bot-user", "username": "thinkbot"}, "reaction": "🎉"}, // self filtered
		},
	})
	c.handleNotification(context.Background(), body)

	env := drainOne(t, ing)
	msg := env.Message
	if msg.Text != "" {
		t.Fatalf("Text must be empty")
	}
	if !strings.Contains(msg.InjectContext, "@alice") || !strings.Contains(msg.InjectContext, "@bob") {
		t.Fatalf("grouped InjectContext should list reactors: %q", msg.InjectContext)
	}
	if strings.Contains(msg.InjectContext, "@thinkbot") {
		t.Fatalf("self reactor must be filtered from grouped: %q", msg.InjectContext)
	}
	if msg.UserID != "u1" || msg.Channel != "u1" {
		t.Fatalf("primary reactor should be first non-self, got UserID=%q Channel=%q", msg.UserID, msg.Channel)
	}
	if _, ok := msg.Metadata["reply_target"]; ok {
		t.Fatal("reply_target must NOT be set for grouped")
	}
	if msg.Metadata["event_type"] != "reaction" {
		t.Fatalf("event_type=%v", msg.Metadata["event_type"])
	}
	if ing.Len() != 0 {
		t.Fatal("grouped must produce exactly one ingress message")
	}
}

func TestHandleStreamMessage_NotificationCase(t *testing.T) {
	c, ing := newReactionTestChannel(t)
	payload := map[string]any{
		"type": "channel",
		"body": map[string]any{
			"id":   mainConnID,
			"type": "notification",
			"body": map[string]any{
				"id": "notif-stream", "type": "reaction", "userId": "u9",
				"user":     map[string]any{"id": "u9", "username": "carol"},
				"reaction": ":blobcat:",
				"note":     map[string]any{"id": "note-s", "userId": "bot-user"},
			},
		},
	}
	raw, _ := json.Marshal(payload)
	if err := c.handleStreamMessage(context.Background(), string(raw)); err != nil {
		t.Fatal(err)
	}
	env := drainOne(t, ing)
	if env.Message.ID != "notif-stream" {
		t.Fatalf("ID=%q", env.Message.ID)
	}
	if !strings.Contains(env.Message.InjectContext, ":blobcat:") {
		t.Fatalf("InjectContext=%q", env.Message.InjectContext)
	}
}
