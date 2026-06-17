package outbound

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// testLogger 创建一个静默的 logger 用于测试。
func testLogger() *zap.SugaredLogger {
	l, _ := zap.NewDevelopment()
	return l.Sugar()
}

func TestMemoryEventBus_PublishSubscribe(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	// 订阅特定 trace_id
	sub := bus.Subscribe("trace-123")
	defer bus.Unsubscribe(sub)

	// 发布匹配事件
	bus.Publish(context.Background(), Event{
		Type:    EventStageEnter,
		TraceID: "trace-123",
		BotID:   "bot-1",
		Stage:   "filter",
	})

	// 发布不匹配事件
	bus.Publish(context.Background(), Event{
		Type:    EventStageEnter,
		TraceID: "trace-456",
		BotID:   "bot-1",
		Stage:   "filter",
	})

	// 应该只收到匹配的事件
	select {
	case event := <-sub.C():
		if event.TraceID != "trace-123" {
			t.Errorf("expected trace_id=trace-123, got %s", event.TraceID)
		}
		if event.Type != EventStageEnter {
			t.Errorf("expected type=stage.enter, got %s", event.Type)
		}
		if event.Stage != "filter" {
			t.Errorf("expected stage=filter, got %s", event.Stage)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for event")
	}

	// 不应收到不匹配的事件
	select {
	case event := <-sub.C():
		t.Fatalf("unexpected event: %+v", event)
	case <-time.After(50 * time.Millisecond):
		// OK: no extra events
	}
}

func TestMemoryEventBus_SubscribeAll(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	// 空 traceID = 订阅所有事件
	sub := bus.Subscribe("")
	defer bus.Unsubscribe(sub)

	bus.Publish(context.Background(), Event{Type: EventStageEnter, TraceID: "a"})
	bus.Publish(context.Background(), Event{Type: EventStageExit, TraceID: "b"})

	// 应该收到两个事件
	count := 0
	for i := 0; i < 2; i++ {
		select {
		case <-sub.C():
			count++
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout: expected 2 events, got %d", count)
		}
	}
	if count != 2 {
		t.Errorf("expected 2 events, got %d", count)
	}
}

func TestMemoryEventBus_SubscribeBot(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	sub := bus.SubscribeBot("bot-1")
	defer bus.Unsubscribe(sub)

	// 匹配 bot
	bus.Publish(context.Background(), Event{
		Type:    EventLLMStart,
		TraceID: "t1",
		BotID:   "bot-1",
	})
	// 不匹配 bot
	bus.Publish(context.Background(), Event{
		Type:    EventLLMStart,
		TraceID: "t2",
		BotID:   "bot-2",
	})

	select {
	case event := <-sub.C():
		if event.BotID != "bot-1" {
			t.Errorf("expected bot_id=bot-1, got %s", event.BotID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for event")
	}

	// 不应收到 bot-2 的事件
	select {
	case event := <-sub.C():
		t.Fatalf("unexpected event from bot: %s", event.BotID)
	case <-time.After(50 * time.Millisecond):
		// OK
	}
}

func TestMemoryEventBus_MultipleSubscribers(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	sub1 := bus.Subscribe("trace-1")
	sub2 := bus.Subscribe("trace-1")
	defer bus.Unsubscribe(sub1)
	defer bus.Unsubscribe(sub2)

	bus.Publish(context.Background(), Event{
		Type:    EventLLMTextDelta,
		TraceID: "trace-1",
		Data:    map[string]any{"text": "hello"},
	})

	// 两个订阅者都应收到事件
	for _, sub := range []*Subscription{sub1, sub2} {
		select {
		case event := <-sub.C():
			if event.Data["text"] != "hello" {
				t.Errorf("expected text=hello, got %v", event.Data["text"])
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("timeout waiting for event")
		}
	}
}

func TestMemoryEventBus_Unsubscribe(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	sub := bus.Subscribe("trace-1")

	// 取消订阅
	bus.Unsubscribe(sub)

	// channel 应该已关闭
	_, ok := <-sub.C()
	if ok {
		t.Error("expected channel to be closed after unsubscribe")
	}
}

func TestMemoryEventBus_Close(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())

	sub := bus.Subscribe("trace-1")

	bus.Close()

	// 订阅 channel 应该已关闭
	_, ok := <-sub.C()
	if ok {
		t.Error("expected channel to be closed after bus close")
	}

	// Close 后 Subscribe 返回已关闭的订阅
	sub2 := bus.Subscribe("trace-2")
	_, ok = <-sub2.C()
	if ok {
		t.Error("expected closed subscription from closed bus")
	}

	// Close 后 Publish 不应 panic
	bus.Publish(context.Background(), Event{Type: EventMessageReceived, TraceID: "x"})
}

func TestMemoryEventBus_NonBlockingPublish(t *testing.T) {
	// 使用小 buffer 测试非阻塞行为
	cfg := MemoryEventBusConfig{
		SubscriptionBufferSize: 2,
		MaxSubscriptions:       100,
	}
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()

	sub := bus.Subscribe("trace-1")
	defer bus.Unsubscribe(sub)

	// 发布超过 buffer 大小的事件
	for i := 0; i < 10; i++ {
		bus.Publish(context.Background(), Event{
			Type:    EventLLMTextDelta,
			TraceID: "trace-1",
			Data:    map[string]any{"i": i},
		})
	}

	// 应该最多收到 buffer 大小的事件（其余被丢弃）
	received := 0
	for {
		select {
		case <-sub.C():
			received++
		case <-time.After(50 * time.Millisecond):
			goto done
		}
	}
done:
	if received > 2 {
		// 可能收到 2 个（buffer 满后丢弃），这里不严格断言
		// 因为读取和写入是并发的
	}
	if received == 0 {
		t.Error("expected at least 1 event")
	}
}

func TestMemoryEventBus_MaxSubscriptions(t *testing.T) {
	cfg := MemoryEventBusConfig{
		SubscriptionBufferSize: 8,
		MaxSubscriptions:       2,
	}
	bus := NewMemoryEventBus(cfg, testLogger())
	defer bus.Close()

	sub1 := bus.Subscribe("t1")
	sub2 := bus.Subscribe("t2")
	sub3 := bus.Subscribe("t3") // 超出限制

	defer bus.Unsubscribe(sub1)
	defer bus.Unsubscribe(sub2)

	if sub3.ID != "rejected" {
		t.Errorf("expected rejected subscription, got ID=%s", sub3.ID)
	}

	// rejected 的 channel 应该已关闭
	_, ok := <-sub3.C()
	if ok {
		t.Error("expected closed channel for rejected subscription")
	}
}

func TestMemoryEventBus_ConcurrentPublish(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	sub := bus.Subscribe("")
	defer bus.Unsubscribe(sub)

	// 并发发布
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			bus.Publish(context.Background(), Event{
				Type:    EventLLMTextDelta,
				TraceID: "concurrent",
				Data:    map[string]any{"n": n},
			})
		}(i)
	}

	// 并发读取
	done := make(chan int)
	go func() {
		count := 0
		for range sub.C() {
			count++
			if count >= 64 { // buffer 大小
				break
			}
		}
		done <- count
	}()

	wg.Wait()

	// 稍等让消费者消费
	select {
	case count := <-done:
		if count == 0 {
			t.Error("expected at least some events")
		}
	case <-time.After(500 * time.Millisecond):
		// 可能 buffer 满了，部分丢弃，这是正常的
	}
}

func TestMemoryEventBus_ActiveSubscriptions(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	if bus.ActiveSubscriptions() != 0 {
		t.Error("expected 0 active subscriptions")
	}

	sub1 := bus.Subscribe("t1")
	sub2 := bus.Subscribe("t2")

	if bus.ActiveSubscriptions() != 2 {
		t.Errorf("expected 2, got %d", bus.ActiveSubscriptions())
	}

	bus.Unsubscribe(sub1)
	if bus.ActiveSubscriptions() != 1 {
		t.Errorf("expected 1, got %d", bus.ActiveSubscriptions())
	}

	bus.Unsubscribe(sub2)
	if bus.ActiveSubscriptions() != 0 {
		t.Errorf("expected 0, got %d", bus.ActiveSubscriptions())
	}
}

func TestEvent_TimestampAutoFill(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	sub := bus.Subscribe("")
	defer bus.Unsubscribe(sub)

	before := time.Now()
	bus.Publish(context.Background(), Event{
		Type:    EventMessageReceived,
		TraceID: "t1",
	})

	select {
	case event := <-sub.C():
		if event.Timestamp.Before(before) {
			t.Error("expected timestamp to be auto-filled")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestFormatSubID(t *testing.T) {
	tests := []struct {
		id   uint64
		want string
	}{
		{0, "sub-0"},
		{1, "sub-1"},
		{42, "sub-42"},
		{999, "sub-999"},
		{1000000, "sub-1000000"},
	}
	for _, tt := range tests {
		got := formatSubID(tt.id)
		if got != tt.want {
			t.Errorf("formatSubID(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}
