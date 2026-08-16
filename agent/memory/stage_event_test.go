package memory

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestMemoryStage_EmitsContextInjectEvent(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	scope := ChannelScope("chat-inject")
	if err := repo.Append(ctx, Entry{Scope: scope, Content: "User name is Luna, a Go developer"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	stage := NewMemoryStage("memory", repo, MemoryStageConfig{}, noop.NewTracerProvider(), zap.NewNop().Sugar())

	sink := core.NewMemorySink(64)
	ectx := core.WithEventSink(ctx, sink)

	env := core.NewEnvelope(core.Message{
		ID:      "m-1",
		TraceID: "trace-1",
		Channel: "chat-inject",
		UserID:  "user-1",
		Text:    "hi",
	})

	if _, err := stage.Process(ectx, env); err != nil {
		t.Fatalf("Process: %v", err)
	}

	events := sink.Snapshot()
	var inject *core.Event
	for i := range events {
		if events[i].Kind == core.EventContextInject {
			inject = &events[i]
			break
		}
	}
	if inject == nil {
		t.Fatalf("expected an EventContextInject event, got %d events: %+v", len(events), events)
	}
	if inject.Source != "memory-recall" {
		t.Errorf("inject source = %q, want memory-recall", inject.Source)
	}
	if !inject.Surface {
		t.Errorf("inject Surface should be true (context enters model surface)")
	}
	payload, ok := inject.Payload.(map[string]any)
	if !ok {
		t.Fatalf("inject payload not a map: %+v", inject.Payload)
	}
	if payload["context_len"] == nil || payload["entries_used"] == nil {
		t.Errorf("inject payload missing fields: %+v", payload)
	}
}

func TestMemoryStage_NoContextNoInjectEvent(t *testing.T) {
	repo := NewMemoryRepository() // 空仓库 → 无上下文
	stage := NewMemoryStage("memory", repo, MemoryStageConfig{}, noop.NewTracerProvider(), zap.NewNop().Sugar())

	sink := core.NewMemorySink(64)
	ectx := core.WithEventSink(context.Background(), sink)

	env := core.NewEnvelope(core.Message{
		ID:      "m-2",
		TraceID: "trace-2",
		Channel: "empty-ch",
		UserID:  "user-2",
		Text:    "hi",
	})
	if _, err := stage.Process(ectx, env); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for _, e := range sink.Snapshot() {
		if e.Kind == core.EventContextInject {
			t.Errorf("should not emit context/inject when there is no context")
		}
	}
}
