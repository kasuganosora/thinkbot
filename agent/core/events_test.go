package core

import (
	"context"
	"testing"
)

func TestNoopSinkIsNoop(t *testing.T) {
	// 不 panic、不分配 seq（Noop 实现直接丢弃）
	NoopSink.Emit(context.Background(), Event{Kind: EventStageStart, Source: "x"})
}

func TestMemorySinkAssignsSeq(t *testing.T) {
	s := NewMemorySink(4)
	for i := 0; i < 3; i++ {
		s.Emit(context.Background(), Event{Kind: EventToolCall, Source: "tool:x"})
	}
	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("want 3 events, got %d", len(snap))
	}
	// seq 单调递增
	for i := 1; i < len(snap); i++ {
		if snap[i].Seq <= snap[i-1].Seq {
			t.Errorf("seq not increasing: %d then %d", snap[i-1].Seq, snap[i].Seq)
		}
	}
	if snap[0].Time.IsZero() {
		t.Error("Time should be set by sink when zero")
	}
}

func TestMemorySinkRingCap(t *testing.T) {
	s := NewMemorySink(3)
	for i := 0; i < 10; i++ {
		s.Emit(context.Background(), Event{Kind: EventStageEnd, Source: "s"})
	}
	if s.Len() > 3 {
		t.Fatalf("cap violated: len=%d", s.Len())
	}
	// 仍保留最近 3 条
	if len(s.Snapshot()) != 3 {
		t.Fatalf("snapshot len=%d, want 3", len(s.Snapshot()))
	}
}

func TestEventSinkContextRoundTrip(t *testing.T) {
	s := NewMemorySink(8)
	ctx := WithEventSink(context.Background(), s)
	if EventSinkFromContext(ctx) != s {
		t.Fatal("sink not retrievable from context")
	}
	// nil context / 未设置 → Noop
	if EventSinkFromContext(nil) != NoopSink {
		t.Fatal("nil ctx should yield NoopSink")
	}
	// WithEventSink(nil) 不应 panic，应回退 Noop
	if EventSinkFromContext(WithEventSink(context.Background(), nil)) != NoopSink {
		t.Fatal("nil sink should normalize to NoopSink")
	}
}
