package route

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestPreemptInjectsIntoOwnerAndDoesNotGrantReply(t *testing.T) {
	g := NewRegistry(Limits{})
	ch := make(chan string, 1)
	g.Add(Run{
		ID:        "l2-a",
		BotID:     "bot",
		Key:       "chat:s1",
		TraceID:   "trace-owner",
		Summary:   "draft the report",
		Interrupt: ch,
	})

	if err := g.Preempt("l2-a", "chat:s1", "trace-new", "add the budget table"); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	select {
	case got := <-ch:
		if got != "add the budget table" {
			t.Fatalf("injected %q", got)
		}
	default:
		t.Fatal("owner interrupt channel was empty")
	}
	if g.OwnsReply("trace-new") {
		t.Fatal("preempted message must not own a reply stream")
	}
	if !g.OwnsReply("trace-owner") {
		t.Fatal("running pipeline keeps the reply stream")
	}
	if err := g.Preempt("l2-a", "chat:other", "trace-x", "nope"); err == nil {
		t.Fatal("preempt across conversations must fail")
	}
}

func TestOpenBudgetStopsRunawayPipelines(t *testing.T) {
	g := NewRegistry(Limits{MaxConcurrent: 2, MaxOpens: 3, OpenWindow: time.Minute})
	key := "chat:s1"
	if !g.AllowOpen(key) || !g.AllowOpen(key) {
		t.Fatal("first opens should be allowed")
	}
	g.Add(Run{ID: "a", Key: key, TraceID: "a"})
	g.Add(Run{ID: "b", Key: key, TraceID: "b"})
	if g.AllowOpen(key) {
		t.Fatal("concurrent cap should refuse another pipeline")
	}
	g.Finish("a")
	g.Finish("b")
	if !g.AllowOpen(key) {
		t.Fatal("a slot should free after the runs finish")
	}
	if g.AllowOpen(key) {
		t.Fatal("window cap should refuse the fourth open")
	}
}

func TestSystemRunsDoNotConsumeUserOpenBudget(t *testing.T) {
	g := NewRegistry(Limits{MaxConcurrent: 1, MaxOpens: 2, OpenWindow: time.Minute})
	g.Add(Run{ID: "hb", Key: "chan:bot:heartbeat:hb", TraceID: "hb", System: true})
	if !g.AllowOpen("chat:s1") {
		t.Fatal("a system run in another conversation must not block a user pipeline")
	}
}

func TestCancelStopsRunWithoutOpeningAnother(t *testing.T) {
	g := NewRegistry(Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	g.Add(Run{ID: "l2-a", BotID: "bot", Key: "chat:s1", TraceID: "trace-owner", Cancel: cancel})
	if err := g.Cancel("l2-a", "chat:s1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("cancel should stop the running pipeline")
	}
	if g.RunningCount("chat:s1") != 0 {
		t.Fatal("cancelled pipeline should leave the running set")
	}
}

func TestConversationKeyPrefersChatSession(t *testing.T) {
	env := core.NewEnvelope(core.Message{
		BotID: "bot", Source: "web", Channel: "web:1",
		Metadata: map[string]any{"chat_session_id": "sess-9"},
	})
	if got := ConversationKey(env); got != "chat:sess-9" {
		t.Fatalf("key = %q", got)
	}
}
