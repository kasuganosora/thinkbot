package misskey

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

func TestHandleNote_TrailingBotMentionReachesModel(t *testing.T) {
	ing := inbound.NewIngress(inbound.IngressConfig{BufferSize: 8}, zap.NewNop().Sugar(), noop.NewTracerProvider())
	defer ing.Close()
	c := newExplicitTestChannel()
	c.ingress = ing
	note := Note{
		ID: "n-trailing", ReplyID: "p1", Visibility: "public",
		User:  User{ID: "carol-id", Username: "carol"},
		Reply: &Note{ID: "p1", User: User{ID: "alice-id", Username: "alice"}, Text: "lunch ideas?"},
		Text:  "@alice ramen sounds good, any tips? @helperbot",
	}
	c.handleNote(context.Background(), note, "mention", true)
	select {
	case env := <-ing.C():
		if v, _ := env.Message.Metadata[core.MetaExplicitMention].(bool); !v {
			t.Fatal("explicit_mention should be true")
		}
		txt := env.Message.Text
		if !strings.Contains(txt, "@helperbot") || !strings.Contains(txt, explicitMentionMarker) {
			t.Fatalf("model-facing text must keep the mention and marker: %q", txt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no message")
	}
}

func TestHandleNote_InheritedMentionNoMarker(t *testing.T) {
	ing := inbound.NewIngress(inbound.IngressConfig{BufferSize: 8}, zap.NewNop().Sugar(), noop.NewTracerProvider())
	defer ing.Close()
	c := newExplicitTestChannel()
	c.ingress = ing
	note := Note{
		ID: "n-inh", ReplyID: "p2", Visibility: "public",
		User:  User{ID: "carol-id", Username: "carol"},
		Reply: &Note{ID: "p2", User: User{ID: "alice-id", Username: "alice"}, Mentions: []string{"bot-id"}},
		Text:  "@alice @helperbot agreed",
	}
	c.handleNote(context.Background(), note, "mention", true)
	select {
	case env := <-ing.C():
		if v, _ := env.Message.Metadata[core.MetaExplicitMention].(bool); v {
			t.Fatal("inherited-only mention must not be explicit")
		}
		if strings.Contains(env.Message.Text, "@helperbot") || strings.Contains(env.Message.Text, explicitMentionMarker) {
			t.Fatalf("auto-prefix @bot should be stripped, no marker: %q", env.Message.Text)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no message")
	}
}
