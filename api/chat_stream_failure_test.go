package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
)

func TestChatFinalPersistenceFailure_PreservesCanonicalReplyForSlowSubscriber(t *testing.T) {
	for _, previousSnapshot := range []bool{false, true} {
		name := "without snapshot"
		if previousSnapshot {
			name = "with streaming snapshot"
		}
		t.Run(name, func(t *testing.T) {
			env := newChatStreamTestEnv(t)
			const trace = "final-persistence-failure"
			if previousSnapshot {
				if err := env.history.UpsertAssistantSnapshot("bot", "1", "Incomplete preview", trace, "", "", env.session, true, 0); err != nil {
					t.Fatal(err)
				}
			}
			failHistoryWrite := func(tx *gorm.DB) {
				if tx.Statement.Table == "chat_messages" {
					tx.AddError(errors.New("history unavailable during final write"))
				}
			}
			if err := env.history.db.Callback().Create().Before("gorm:begin_transaction").Register("test:fail_final_create", failHistoryWrite); err != nil {
				t.Fatal(err)
			}
			if err := env.history.db.Callback().Update().Before("gorm:begin_transaction").Register("test:fail_final_update", failHistoryWrite); err != nil {
				t.Fatal(err)
			}
			turn := env.svc.prepareChatTurn(env.channel, "bot", "1", env.session, trace)
			sub := env.bus.SubscribeWithReplay(trace, 0)
			env.reply(trace, "Canonical final response")
			select {
			case <-turn.done:
			case <-time.After(2 * time.Second):
				t.Fatal("collector did not complete")
			}

			// A slow subscriber can first consume message.done after the collector
			// has already exited. Its queued canonical chat.done must still win
			// over the legacy lifecycle fallback, even when no final row exists.
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			(&Server{chatHistory: env.history}).serveChatEvents(ctx, env.bus, sub, "bot", env.session, trace, nil, 0)
			body := recorder.Body.String()
			if !strings.Contains(body, "Canonical final response") || !strings.Contains(body, "persistenceError") {
				t.Fatalf("SSE lost canonical reply or persistence failure: %s", body)
			}
			if strings.Count(body, "event: done\n") != 1 {
				t.Fatalf("expected exactly one final event: %s", body)
			}
		})
	}
}

func TestChatCollectorRetentionLoss_KeepsCanonicalReplyAndMarksIncompleteHistory(t *testing.T) {
	env := newChatStreamTestEnv(t)
	bus := outbound.NewMemoryEventBus(outbound.MemoryEventBusConfig{
		SubscriptionBufferSize: 1, MaxSubscriptions: 10, StoreCapacity: 1, StoreTTL: time.Minute,
	}, zap.NewNop().Sugar())
	t.Cleanup(bus.Close)
	env.bus, env.svc.eventBus = bus, bus

	blocked, release := make(chan struct{}), make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	if err := env.history.db.Callback().Create().Before("gorm:begin_transaction").Register("test:block_first_snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == "chat_messages" {
			blockOnce.Do(func() {
				close(blocked)
				<-release
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	const trace = "collector-retention-loss"
	turn := env.svc.prepareChatTurn(env.channel, "bot", "1", env.session, trace)
	env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": "a"})
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not start its first snapshot")
	}
	// The paused collector can buffer b, then overflows on c; the tiny ring
	// retains only d. This guarantees a real, unrecoverable gap in its deltas.
	for _, delta := range []string{"b", "c", "d"} {
		env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": delta})
	}
	if n := bus.ActiveSubscriptions(); n != 0 {
		t.Fatalf("expected overflow to evict the old subscription, got %d active", n)
	}
	unblock()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
recovery:
	for bus.ActiveSubscriptions() == 0 {
		select {
		case <-turn.done:
			// The broken implementation exits here, before the actual task.
			break recovery
		case <-tick.C:
		case <-timer.C:
			t.Fatal("collector did not recover its subscription")
		}
	}
	// The actual pipeline still completes and dispatches a canonical reply.
	// Losing progress must not unregister this reply's only durable receiver.
	env.reply(trace, "Complete canonical answer")
	select {
	case <-turn.done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not finalize after the canonical reply")
	}
	saved := loadAssistant(t, env.history, trace)
	if saved.Content != "Complete canonical answer" || saved.Streaming || saved.StreamStatus != "completed" {
		t.Fatalf("canonical reply lost after progress retention gap: %+v", saved)
	}
	if saved.StreamError == "" {
		t.Fatal("history lost progress but did not record an incomplete-history warning")
	}
}

func TestChatCollectorRetentionLoss_LifecycleFallbackPreservesRealOutcome(t *testing.T) {
	cases := []struct {
		name       string
		terminal   outbound.EventType
		canonical  bool
		evict      bool
		wantStatus string
		wantError  string
	}{
		{"evicted done preserves canonical", outbound.EventMessageDone, true, true, "completed", "最终回复已保留"},
		{"queued message error wins", outbound.EventMessageError, true, false, "error", "provider failed"},
		{"queued dispatch error wins", outbound.EventDispatchError, true, false, "error", "provider failed"},
		{"evicted done without canonical fails explicitly", outbound.EventMessageDone, false, true, "error", "完成事件和最终回复未能恢复"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			env := newChatStreamTestEnv(t)
			bus := outbound.NewMemoryEventBus(outbound.MemoryEventBusConfig{
				SubscriptionBufferSize: 1, MaxSubscriptions: 10, StoreCapacity: 1, StoreTTL: time.Minute,
			}, zap.NewNop().Sugar())
			t.Cleanup(bus.Close)
			env.bus, env.svc.eventBus = bus, bus

			blocked, release := make(chan struct{}), make(chan struct{})
			var blockOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			if err := env.history.db.Callback().Create().Before("gorm:begin_transaction").Register("test:pause_collector_before_terminal", func(tx *gorm.DB) {
				if tx.Statement.Table == "chat_messages" {
					blockOnce.Do(func() {
						close(blocked)
						<-release
					})
				}
			}); err != nil {
				t.Fatal(err)
			}
			const trace = "collector-lifecycle-fallback"
			turn := env.svc.prepareChatTurn(env.channel, "bot", "1", env.session, trace)
			env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": "a"})
			select {
			case <-blocked:
			case <-time.After(2 * time.Second):
				t.Fatal("collector did not pause at its first snapshot")
			}
			for _, delta := range []string{"b", "c", "d"} {
				env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": delta})
			}
			if n := bus.ActiveSubscriptions(); n != 0 {
				t.Fatalf("expected collector overflow before dispatch, got %d subscriptions", n)
			}
			if item.canonical {
				if err := env.channel.Send(context.Background(), core.Action{
					Type: core.ActionReply, Payload: "Canonical answer after lost progress",
					Metadata: map[string]any{"trace_id": trace, "chat_session_id": env.session},
				}); err != nil {
					t.Fatal(err)
				}
			}
			// Match the engine ordering: dispatch, publish its actual terminal,
			// then notify completion independently of the bounded replay store.
			env.publish(trace, item.terminal, map[string]any{"error": "provider failed"})
			env.svc.notifyChatTurnDone("bot", trace)
			if item.evict {
				env.publish("unrelated-busy-trace", outbound.EventLLMTextDelta, map[string]any{"text": "unrelated"})
				probe := bus.SubscribeWithReplay(trace, 0)
				select {
				case retained := <-probe.C():
					bus.Unsubscribe(probe)
					t.Fatalf("terminal was not evicted from the replay ring: %+v", retained)
				default:
				}
				bus.Unsubscribe(probe)
			}
			unblock()
			select {
			case <-turn.done:
			case <-time.After(2 * time.Second):
				t.Fatal("collector remained running after actual lifecycle completion")
			}
			saved := loadAssistant(t, env.history, trace)
			if saved.Streaming || saved.StreamStatus != item.wantStatus || !strings.Contains(saved.StreamError, item.wantError) {
				t.Fatalf("lost or misclassified the real terminal outcome: %+v", saved)
			}
			if item.canonical && saved.Content != "Canonical answer after lost progress" {
				t.Fatalf("lifecycle fallback lost canonical response: %+v", saved)
			}
			if !strings.Contains(saved.StreamError, "未完整保存") {
				t.Fatalf("missing incomplete-history warning: %+v", saved)
			}
		})
	}
}
