package bot

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/route"
)

// keepChan records KeepReplyStream. A preempted inbound trace must not tear
// down the chat placeholder; the pipeline that kept the reply still edits it.
type keepChan struct {
	trace string
}

func (k *keepChan) Name() string                                  { return "keep" }
func (k *keepChan) Type() string                                  { return "test" }
func (k *keepChan) BotID() string                                 { return "bot" }
func (k *keepChan) Start(context.Context, *inbound.Ingress) error { return nil }
func (k *keepChan) Stop(context.Context) error                    { return nil }
func (k *keepChan) KeepReplyStream(traceID string)                { k.trace = traceID }

func TestPreemptKeepsReplyStreamBeforeTraceClears(t *testing.T) {
	kc := &keepChan{}
	var cleared bool
	checkOrder := true
	b := &Bot{
		ID:       "bot",
		emitter:  outbound.NewEventEmitter(nil, "bot"),
		channels: []Channel{kc},
		onMessageDone: func(string, string) {
			cleared = true
			if checkOrder && kc.trace == "" {
				t.Error("reply stream must be kept before the inbound trace is cleared")
			}
		},
	}
	env := core.NewEnvelope(core.Message{BotID: "bot", TraceID: "t-new", Text: "follow up"})
	env.Set(route.KVDropReason, route.DropReasonPreempt)
	b.OnMessageDropped(context.Background(), env)
	checkOrder = false
	if !cleared {
		t.Fatal("preempt drop did not finish the inbound trace")
	}
	if kc.trace != "t-new" {
		t.Fatalf("kept trace = %q, want t-new", kc.trace)
	}

	kc.trace = ""
	other := core.NewEnvelope(core.Message{BotID: "bot", TraceID: "t-silent", Text: "nope"})
	b.OnMessageDropped(context.Background(), other)
	if kc.trace != "" {
		t.Fatalf("non-preempt drop kept a reply stream: %q", kc.trace)
	}
}
