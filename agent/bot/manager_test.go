package bot

import (
	"context"
	"fmt"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// BotManager 测试
// ============================================================================

func createTestBot(t *testing.T, id string, channels ...Channel) *Bot {
	t.Helper()
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	bot, err := New(BotParams{
		ID:         id,
		Config:     BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:   p,
		Dispatcher: &collectDispatcher{},
		Channels:   channels,
		Logger:     logger,
		TP:         tp,
	})
	if err != nil {
		t.Fatalf("createTestBot(%q) failed: %v", id, err)
	}
	return bot
}

func TestBotManager_Register(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	mgr := NewBotManager(logger, tp)

	bot := createTestBot(t, "bot-1")

	if err := mgr.Register(bot); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if mgr.Count() != 1 {
		t.Errorf("Count: got %d, want 1", mgr.Count())
	}

	// 重复注册应失败
	if err := mgr.Register(bot); err == nil {
		t.Error("expected error on duplicate register")
	}
}

func TestBotManager_Get(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	mgr := NewBotManager(logger, tp)

	bot := createTestBot(t, "bot-1")
	mgr.Register(bot)

	got, ok := mgr.Get("bot-1")
	if !ok {
		t.Fatal("Get(bot-1) should return true")
	}
	if got.ID != "bot-1" {
		t.Errorf("Get returned bot with ID %q", got.ID)
	}

	_, ok = mgr.Get("nonexistent")
	if ok {
		t.Error("Get(nonexistent) should return false")
	}
}

func TestBotManager_Unregister(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	mgr := NewBotManager(logger, tp)

	bot := createTestBot(t, "bot-1")
	mgr.Register(bot)

	if !mgr.Unregister("bot-1") {
		t.Error("Unregister should return true for existing bot")
	}
	if mgr.Count() != 0 {
		t.Errorf("Count after unregister: got %d, want 0", mgr.Count())
	}

	if mgr.Unregister("nonexistent") {
		t.Error("Unregister should return false for nonexistent bot")
	}
}

func TestBotManager_List(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	mgr := NewBotManager(logger, tp)

	for i := 0; i < 3; i++ {
		bot := createTestBot(t, fmt.Sprintf("bot-%d", i))
		mgr.Register(bot)
	}

	list := mgr.List()
	if len(list) != 3 {
		t.Fatalf("List: got %d bots, want 3", len(list))
	}
}

func TestBotManager_Info(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	mgr := NewBotManager(logger, tp)

	ch := NewMemoryChannel("misskey", "bot-1")
	bot := createTestBot(t, "bot-1", ch)
	mgr.Register(bot)

	infos := mgr.Info()
	if len(infos) != 1 {
		t.Fatalf("Info: got %d, want 1", len(infos))
	}
	if infos[0].ID != "bot-1" {
		t.Errorf("Info[0].ID: got %q", infos[0].ID)
	}
	if len(infos[0].Channels) != 1 || infos[0].Channels[0] != "misskey" {
		t.Errorf("Info[0].Channels: got %v", infos[0].Channels)
	}
}

func TestBotManager_RunAll_StopAll(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	mgr := NewBotManager(logger, tp)

	// 创建 2 个 bot，各有 1 个 MemoryChannel
	ch1 := NewMemoryChannel("ch-1", "bot-1")
	ch2 := NewMemoryChannel("ch-2", "bot-2")
	bot1 := createTestBot(t, "bot-1", ch1)
	bot2 := createTestBot(t, "bot-2", ch2)
	mgr.Register(bot1)
	mgr.Register(bot2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := mgr.RunAll(ctx); err != nil {
		t.Fatalf("RunAll failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// 验证可以通过 MemoryChannel 发送消息
	if err := ch1.Send(context.Background(), core.Message{ID: "1", Text: "hello bot-1"}); err != nil {
		t.Fatalf("Send to bot-1 failed: %v", err)
	}
	if err := ch2.Send(context.Background(), core.Message{ID: "2", Text: "hello bot-2"}); err != nil {
		t.Fatalf("Send to bot-2 failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	mgr.StopAll()
	cancel()

	// 等待一下让 bot goroutine 退出
	time.Sleep(200 * time.Millisecond)
}
