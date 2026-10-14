package outbound

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Both publishers and the replay subscriber must overlap: a sequential
// replay-then-publish test cannot detect an event delivered by both paths.
func TestMemoryEventBus_ConcurrentReplayDoesNotDuplicate(t *testing.T) {
	for round := 0; round < 100; round++ {
		cfg := DefaultMemoryEventBusConfig()
		cfg.SubscriptionBufferSize = 512
		bus := NewMemoryEventBus(cfg, testLogger())
		start := make(chan struct{})
		var publishers sync.WaitGroup
		for worker := 0; worker < 4; worker++ {
			publishers.Add(1)
			go func() {
				defer publishers.Done()
				<-start
				for i := 0; i < 32; i++ {
					bus.PublishTextDelta(context.Background(), "trace", "bot", "x")
					runtime.Gosched()
				}
			}()
		}
		close(start)
		runtime.Gosched()
		sub := bus.SubscribeWithReplay("trace", 0)
		publishers.Wait()
		bus.Unsubscribe(sub)
		seen := make(map[uint64]bool)
		for event := range sub.C() {
			if seen[event.Seq] {
				bus.Close()
				t.Fatalf("round %d: replay and live delivery duplicated seq=%d", round, event.Seq)
			}
			seen[event.Seq] = true
		}
		bus.Close()
		if len(seen) != 128 {
			t.Fatalf("round %d: received %d unique events, want 128", round, len(seen))
		}
	}
}

func TestMemoryEventBus_ReplaysAllRetainedEvents(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.SubscriptionBufferSize = 8
	cfg.StoreCapacity = 6000
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	const total = 5000
	for i := 0; i < total; i++ {
		bus.PublishTextDelta(context.Background(), "long-trace", "bot", "x")
	}
	sub := bus.SubscribeWithReplay("long-trace", 0)
	// Replay must reserve room for live traffic before a consumer starts.
	bus.Publish(context.Background(), Event{Type: EventMessageDone, TraceID: "long-trace"})
	bus.Unsubscribe(sub)
	var count int
	for event := range sub.C() {
		count++
		if event.Seq != uint64(count) {
			t.Fatalf("event %d has seq %d", count, event.Seq)
		}
	}
	if count != total+1 || sub.Overflowed() || sub.ReplayTruncated {
		t.Fatalf("replay count=%d overflow=%t truncated=%t", count, sub.Overflowed(), sub.ReplayTruncated)
	}
}

func TestMemoryEventBus_OverflowClosesStreamForRecovery(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.SubscriptionBufferSize = 2
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	sub := bus.Subscribe("trace")
	for i := 0; i < 3; i++ {
		bus.PublishTextDelta(context.Background(), "trace", "bot", "x")
	}
	if !sub.Overflowed() || bus.ActiveSubscriptions() != 0 {
		t.Fatalf("overflow must signal recovery and release subscription")
	}
	var delivered int
	for range sub.C() {
		delivered++
	}
	if delivered != 2 {
		t.Fatalf("delivered=%d", delivered)
	}
	// The event that overflowed the live channel must still exist in replay.
	recovered := bus.SubscribeWithReplay("trace", 2)
	defer bus.Unsubscribe(recovered)
	if e := <-recovered.C(); e.Seq != 3 {
		t.Fatalf("recovered seq=%d", e.Seq)
	}
}

func TestMemoryEventBus_ReplayReportsRetentionFloor(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.StoreCapacity = 2
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	for i := 0; i < 3; i++ {
		bus.PublishTextDelta(context.Background(), "trace", "bot", "x")
	}
	sub := bus.SubscribeWithReplay("trace", 0)
	defer bus.Unsubscribe(sub)
	if !sub.ReplayTruncated || sub.ReplayAfterSeq != 1 {
		t.Fatalf("truncated=%t floor=%d", sub.ReplayTruncated, sub.ReplayAfterSeq)
	}
	covered := bus.SubscribeWithReplay("trace", 1)
	defer bus.Unsubscribe(covered)
	if covered.ReplayTruncated {
		t.Fatal("cursor at retention floor has complete replay")
	}
}

func TestMemoryEventBus_ReplayFloorIgnoresOtherTraces(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.StoreCapacity = 2
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	// A waiting choice has persisted through sequence 1. Other bots keep
	// publishing while the user decides, eventually evicting its event.
	bus.PublishTextDelta(context.Background(), "quiet", "bot-a", "choose")
	for i := 0; i < 10; i++ {
		bus.PublishTextDelta(context.Background(), "busy", "bot-b", "x")
	}
	sub := bus.SubscribeWithReplay("quiet", 1)
	defer bus.Unsubscribe(sub)
	if sub.ReplayTruncated || sub.ReplayAfterSeq != 1 {
		t.Fatalf("another trace invalidated complete quiet snapshot: truncated=%t floor=%d", sub.ReplayTruncated, sub.ReplayAfterSeq)
	}
	// An expired event belonging to another bot must not raise this trace's floor.
	bus.Publish(context.Background(), Event{TraceID: "expired", Timestamp: time.Now().Add(-time.Hour)})
	fresh := bus.SubscribeWithReplay("quiet", 1)
	defer bus.Unsubscribe(fresh)
	if fresh.ReplayTruncated || fresh.ReplayAfterSeq != 1 {
		t.Fatalf("unrelated TTL expiry raised quiet floor: %d", fresh.ReplayAfterSeq)
	}
}

func TestMemoryEventBus_TraceMetadataIsBoundedWithoutLosingQuietTaskFloor(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.StoreCapacity = 4
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	quiet := bus.SubscribeNewTrace("quiet-task")
	bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: "quiet-task"})
	quietCursor := bus.LatestSeq()
	for i := 0; i < 2000; i++ {
		bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: fmt.Sprintf("trace-%d", i)})
		if len(bus.store.traces) > cfg.StoreCapacity+bus.store.pinnedTraces || bus.store.traceLRU.Len() != len(bus.store.traces) {
			t.Fatalf("metadata grew past bound: traces=%d list=%d pinned=%d", len(bus.store.traces), bus.store.traceLRU.Len(), bus.store.pinnedTraces)
		}
	}
	if bus.store.unknownFloor <= quietCursor {
		t.Fatal("test never evicted enough metadata to exercise unknown floor")
	}
	resumedQuiet := bus.SubscribeWithReplay("quiet-task", quietCursor)
	if resumedQuiet.ReplayTruncated || resumedQuiet.ReplayAfterSeq != quietCursor {
		t.Fatalf("quiet pinned trace falsely needs recovery: floor=%d", resumedQuiet.ReplayAfterSeq)
	}
	forgotten := bus.SubscribeWithReplay("trace-0", 0)
	if !forgotten.ReplayTruncated || forgotten.ReplayAfterSeq == 0 {
		t.Fatal("forgotten trace incorrectly claimed complete replay")
	}
	bus.Unsubscribe(resumedQuiet)
	bus.Unsubscribe(forgotten)
	bus.Unsubscribe(quiet)
	bus.Unsubscribe(quiet) // repeated cancellation must not unpin another trace.
	if bus.store.pinnedTraces != 0 || len(bus.store.traces) > cfg.StoreCapacity {
		t.Fatalf("unsubscribe leaked pinned metadata: pinned=%d traces=%d", bus.store.pinnedTraces, len(bus.store.traces))
	}
}

func TestMemoryEventBus_NewTraceStartsCleanAfterMetadataEviction(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.StoreCapacity = 2
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	for i := 0; i < 20; i++ {
		bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: fmt.Sprintf("old-%d", i)})
	}
	if bus.store.unknownFloor == 0 {
		t.Fatal("test needs evicted trace metadata")
	}
	// The persistence subscriber starts before Inject publishes message.received;
	// a second HTTP subscriber must still recognize this empty trace as fresh.
	fresh := bus.SubscribeNewTrace("fresh")
	defer bus.Unsubscribe(fresh)
	httpSub := bus.SubscribeWithReplay("fresh", 0)
	defer bus.Unsubscribe(httpSub)
	if httpSub.ReplayTruncated || httpSub.ReplayAfterSeq != 0 {
		t.Fatalf("new trace inherited another trace's gap: %+v", httpSub)
	}
	bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: "fresh"})
	bus.PublishTextDelta(context.Background(), "fresh", "bot", "hello")
	for i := 0; i < 2; i++ {
		select {
		case event := <-httpSub.C():
			if event.TraceID != "fresh" {
				t.Fatalf("wrong replay: %+v", event)
			}
		default:
			t.Fatal("fresh trace event missing")
		}
	}
	// Legacy producers can prove a complete beginning through message.received.
	bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: "legacy-new"})
	knownStart := bus.SubscribeWithReplay("legacy-new", 0)
	defer bus.Unsubscribe(knownStart)
	if knownStart.ReplayTruncated {
		t.Fatal("retained message.received did not establish a complete trace beginning")
	}
	bus.Close()
	if bus.store.pinnedTraces != 0 || len(bus.store.traces) > cfg.StoreCapacity {
		t.Fatal("Close leaked trace pins")
	}
}

func TestMemoryEventBus_RepeatedFreshDeclarationPreservesKnownGap(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.StoreCapacity = 2
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	first := bus.SubscribeNewTrace("trace")
	defer bus.Unsubscribe(first)
	bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: "trace"})
	bus.PublishTextDelta(context.Background(), "trace", "bot", "a")
	bus.PublishTextDelta(context.Background(), "trace", "bot", "b")
	duplicate := bus.SubscribeNewTrace("trace")
	defer bus.Unsubscribe(duplicate)
	resumed := bus.SubscribeWithReplay("trace", 0)
	defer bus.Unsubscribe(resumed)
	if !resumed.ReplayTruncated || resumed.ReplayAfterSeq != 1 {
		t.Fatalf("fresh declaration erased known loss: floor=%d", resumed.ReplayAfterSeq)
	}
}

func TestMemoryEventBus_OverflowPinGapCannotClaimCompleteReplay(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.StoreCapacity = 2
	cfg.SubscriptionBufferSize = 1
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	old := bus.SubscribeNewTrace("overflowed")
	bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: "overflowed"})
	bus.PublishTextDelta(context.Background(), "overflowed", "bot", "lost live delta")
	if !old.Overflowed() {
		t.Fatal("test must release original pin by overflowing it")
	}
	// Simulate other bots evicting both events and metadata before the detached
	// collector gets scheduled to resubscribe. Unknown must remain a gap.
	for i := 0; i < 20; i++ {
		bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: fmt.Sprintf("during-gap-%d", i)})
	}
	recovered := bus.SubscribeWithReplay("overflowed", 1)
	defer bus.Unsubscribe(recovered)
	if !recovered.ReplayTruncated || recovered.ReplayAfterSeq < 2 {
		t.Fatalf("unpin/resubscribe window falsely claimed complete replay: floor=%d", recovered.ReplayAfterSeq)
	}
	bus.Unsubscribe(old) // The earlier closed subscription cannot release this new pin.
	if state := bus.store.traces["overflowed"]; state == nil || state.pins != 1 {
		t.Fatal("unsubscribing overflowed handle released replacement pin")
	}
}

func TestMemoryEventBus_ReplayReportsExpiredAndDisabledStore(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		cfg := DefaultMemoryEventBusConfig()
		cfg.StoreTTL = time.Minute
		if disabled {
			cfg.StoreCapacity = 0
		}
		bus := NewMemoryEventBus(cfg, testLogger())
		bus.Publish(context.Background(), Event{TraceID: "trace", Timestamp: time.Now().Add(-2 * time.Minute)})
		sub := bus.SubscribeWithReplay("trace", 0)
		if !sub.ReplayTruncated || sub.ReplayAfterSeq != 1 {
			t.Errorf("disabled=%t truncated=%t floor=%d", disabled, sub.ReplayTruncated, sub.ReplayAfterSeq)
		}
		bus.Close()
	}
}

func TestMemoryEventBus_ConcurrentPublishKeepsSequenceOrder(t *testing.T) {
	cfg := DefaultMemoryEventBusConfig()
	cfg.SubscriptionBufferSize = 1024
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()
	live := bus.Subscribe("trace")
	var publishers sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			<-start
			for i := 0; i < 64; i++ {
				bus.PublishTextDelta(context.Background(), "trace", "bot", "x")
			}
		}()
	}
	close(start)
	publishers.Wait()
	replay := bus.SubscribeWithReplay("trace", 0)
	for name, sub := range map[string]*Subscription{"live": live, "replay": replay} {
		bus.Unsubscribe(sub)
		var previous uint64
		for event := range sub.C() {
			if event.Seq <= previous {
				t.Errorf("%s event sequence went backward: %d followed %d", name, event.Seq, previous)
				break
			}
			previous = event.Seq
		}
	}
}
