package bot

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
)

// ============================================================================
// 测试辅助
// ============================================================================

// collectDispatcher 收集 dispatch 的 Action（线程安全）。
type collectDispatcher struct {
	mu      sync.Mutex
	actions []core.Action
}

func (d *collectDispatcher) Dispatch(_ context.Context, actions []core.Action) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.actions = append(d.actions, actions...)
	return nil
}

func (d *collectDispatcher) collected() []core.Action {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]core.Action, len(d.actions))
	copy(out, d.actions)
	return out
}

// echoStage 为每条消息生成 reply Action。
type echoStage struct{}

func (s *echoStage) Name() string { return "echo" }
func (s *echoStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: "echo: " + env.Message.Text,
	})
	return env, nil
}

func buildTestPipeline(t *testing.T, stages ...core.StageInfo) *pipeline.Pipeline {
	t.Helper()
	tp := noop_trace.NewTracerProvider()
	mp := noop.NewMeterProvider()
	logger := zap.NewNop().Sugar()
	p, err := pipeline.New(stages, tp, mp, logger)
	if err != nil {
		t.Fatalf("failed to create pipeline: %v", err)
	}
	return p
}

func waitForActions(disp *collectDispatcher, count int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		if len(disp.collected()) >= count {
			return true
		}
		select {
		case <-deadline:
			return false
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// ============================================================================
// Bot 测试
// ============================================================================

func TestBot_New_Validation(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	p := buildTestPipeline(t)
	disp := outbound.NewLogDispatcher(logger, tp)

	tests := []struct {
		name    string
		params  BotParams
		wantErr bool
	}{
		{
			name:    "empty ID",
			params:  BotParams{},
			wantErr: true,
		},
		{
			name:    "missing pipeline",
			params:  BotParams{ID: "test"},
			wantErr: true,
		},
		{
			name:    "missing dispatcher",
			params:  BotParams{ID: "test", Pipeline: p},
			wantErr: true,
		},
		{
			name:    "missing logger",
			params:  BotParams{ID: "test", Pipeline: p, Dispatcher: disp},
			wantErr: true,
		},
		{
			name:    "missing tracer provider",
			params:  BotParams{ID: "test", Pipeline: p, Dispatcher: disp, Logger: logger},
			wantErr: true,
		},
		{
			name:    "valid minimal",
			params:  BotParams{ID: "test", Pipeline: p, Dispatcher: disp, Logger: logger, TP: tp},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.params)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBot_EndToEnd(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	disp := &collectDispatcher{}

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("test-mem", "bot-a")

	bot, err := New(BotParams{
		ID:         "bot-a",
		Name:       "Test Bot A",
		Config:     BotConfig{Workers: 2, IngressBufferSize: 16},
		Pipeline:   p,
		Dispatcher: disp,
		Channels:   []Channel{memCh},
		Logger:     logger,
		TP:         tp,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- bot.Run(ctx)
	}()
	time.Sleep(50 * time.Millisecond)

	// 注入 3 条消息
	for i := 0; i < 3; i++ {
		if err := memCh.Send(context.Background(), core.Message{
			ID:      fmt.Sprintf("msg-%d", i),
			Channel: "ch-1",
			UserID:  "user-1",
			Text:    fmt.Sprintf("hello %d", i),
		}); err != nil {
			t.Fatalf("Send failed: %v", err)
		}
	}

	if !waitForActions(disp, 3, 3*time.Second) {
		t.Fatalf("timeout waiting for 3 actions, got %d", len(disp.collected()))
	}

	actions := disp.collected()
	if len(actions) != 3 {
		t.Fatalf("expected 3 actions, got %d", len(actions))
	}

	for i, a := range actions {
		if a.Type != core.ActionReply {
			t.Errorf("action[%d] type: got %s, want reply", i, a.Type)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("bot returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bot did not stop in time")
	}
}

func TestBot_BotIDInMessage(t *testing.T) {
	// 验证通过 MemoryChannel 注入的消息自动携带 BotID
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	// 自定义 Stage：验证 envelope 中的 BotID
	var capturedBotID string
	var capturedSource string
	captureStage := &core.StageFunc{
		StageName: "capture",
		Fn: func(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
			capturedBotID = env.Message.BotID
			capturedSource = env.Message.Source
			return env, nil
		},
	}

	p := buildTestPipeline(t,
		core.StageInfo{Stage: captureStage, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("misskey-bot-a", "bot-a")

	bot, err := New(BotParams{
		ID:         "bot-a",
		Config:     BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:   p,
		Dispatcher: &collectDispatcher{},
		Channels:   []Channel{memCh},
		Logger:     logger,
		TP:         tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	memCh.Send(context.Background(), core.Message{
		ID:   "msg-1",
		Text: "test bot id",
	})
	time.Sleep(200 * time.Millisecond)

	if capturedBotID != "bot-a" {
		t.Errorf("expected BotID=bot-a, got %q", capturedBotID)
	}
	if capturedSource != "misskey-bot-a" {
		t.Errorf("expected Source=misskey-bot-a, got %q", capturedSource)
	}

	cancel()
}

func TestBot_BotConfigInEnvelope(t *testing.T) {
	// 验证 Bot.processEnvelope 将 BotConfig 注入到 Envelope KV 中
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	var capturedBotID string
	var capturedConfig BotConfig
	captureStage := &core.StageFunc{
		StageName: "capture-config",
		Fn: func(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
			if v, ok := env.Get("bot.id"); ok {
				capturedBotID = v.(string)
			}
			if v, ok := env.Get("bot.config"); ok {
				capturedConfig = v.(BotConfig)
			}
			return env, nil
		},
	}

	p := buildTestPipeline(t,
		core.StageInfo{Stage: captureStage, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("mem", "cfg-bot")
	bot, _ := New(BotParams{
		ID:         "cfg-bot",
		Config:     BotConfig{Workers: 1, IngressBufferSize: 8, SystemPrompt: "You are helpful", Model: "gpt-4o"},
		Pipeline:   p,
		Dispatcher: &collectDispatcher{},
		Channels:   []Channel{memCh},
		Logger:     logger,
		TP:         tp,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	memCh.Send(context.Background(), core.Message{ID: "1", Text: "hi"})
	time.Sleep(200 * time.Millisecond)

	if capturedBotID != "cfg-bot" {
		t.Errorf("expected bot.id=cfg-bot, got %q", capturedBotID)
	}
	if capturedConfig.SystemPrompt != "You are helpful" {
		t.Errorf("expected SystemPrompt, got %q", capturedConfig.SystemPrompt)
	}
	if capturedConfig.Model != "gpt-4o" {
		t.Errorf("expected Model=gpt-4o, got %q", capturedConfig.Model)
	}

	cancel()
}

func TestBot_MultipleChannels(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	disp := &collectDispatcher{}

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	ch1 := NewMemoryChannel("misskey", "multi-bot")
	ch2 := NewMemoryChannel("telegram", "multi-bot")

	bot, _ := New(BotParams{
		ID:         "multi-bot",
		Config:     BotConfig{Workers: 2, IngressBufferSize: 16},
		Pipeline:   p,
		Dispatcher: disp,
		Channels:   []Channel{ch1, ch2},
		Logger:     logger,
		TP:         tp,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// 两个 Channel 各发一条
	ch1.Send(context.Background(), core.Message{ID: "mk-1", Channel: "mk-ch", Text: "from misskey"})
	ch2.Send(context.Background(), core.Message{ID: "tg-1", Channel: "tg-ch", Text: "from telegram"})

	if !waitForActions(disp, 2, 3*time.Second) {
		t.Fatalf("timeout, got %d actions", len(disp.collected()))
	}

	if len(disp.collected()) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(disp.collected()))
	}

	cancel()
}

func TestBot_GracefulShutdown(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	p := buildTestPipeline(t)
	memCh := NewMemoryChannel("mem", "shutdown-bot")

	bot, _ := New(BotParams{
		ID:         "shutdown-bot",
		Config:     BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:   p,
		Dispatcher: &collectDispatcher{},
		Channels:   []Channel{memCh},
		Logger:     logger,
		TP:         tp,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- bot.Run(ctx)
	}()
	time.Sleep(50 * time.Millisecond)

	bot.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("bot returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bot did not stop in time")
	}

	cancel()
}

func TestBot_DefaultNameFallback(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	p := buildTestPipeline(t)

	bot, _ := New(BotParams{
		ID:         "no-name-bot",
		Pipeline:   p,
		Dispatcher: &collectDispatcher{},
		Logger:     logger,
		TP:         tp,
	})

	// Name 应该 fallback 到 ID
	if bot.Name != "no-name-bot" {
		t.Errorf("expected Name to fallback to ID, got %q", bot.Name)
	}
}

// ============================================================================
// BotConfig 测试
// ============================================================================

func TestBotConfig_Default(t *testing.T) {
	cfg := DefaultBotConfig()
	if cfg.Workers != 4 {
		t.Errorf("default workers: got %d, want 4", cfg.Workers)
	}
	if cfg.IngressBufferSize != 256 {
		t.Errorf("default buffer: got %d, want 256", cfg.IngressBufferSize)
	}
}

func TestBotConfig_Merge(t *testing.T) {
	base := DefaultBotConfig()
	override := BotConfig{
		Workers:      8,
		SystemPrompt: "You are a bot",
		Model:        "claude-3.5-sonnet",
		Extra:        map[string]any{"custom": true},
	}

	merged := base.Merge(override)

	if merged.Workers != 8 {
		t.Errorf("merged workers: got %d, want 8", merged.Workers)
	}
	if merged.IngressBufferSize != 256 {
		t.Errorf("merged buffer should keep default: got %d", merged.IngressBufferSize)
	}
	if merged.SystemPrompt != "You are a bot" {
		t.Errorf("merged system prompt: got %q", merged.SystemPrompt)
	}
	if merged.Model != "claude-3.5-sonnet" {
		t.Errorf("merged model: got %q", merged.Model)
	}
	if merged.Extra["custom"] != true {
		t.Error("merged extra should contain custom=true")
	}
}

// ============================================================================
// MemoryChannel 测试
// ============================================================================

func TestMemoryChannel_Interface(t *testing.T) {
	ch := NewMemoryChannel("test-ch", "bot-1")

	if ch.Name() != "test-ch" {
		t.Errorf("Name: got %q, want test-ch", ch.Name())
	}
	if ch.Type() != "memory" {
		t.Errorf("Type: got %q, want memory", ch.Type())
	}
	if ch.BotID() != "bot-1" {
		t.Errorf("BotID: got %q, want bot-1", ch.BotID())
	}
}

func TestMemoryChannel_DefaultName(t *testing.T) {
	ch := NewMemoryChannel("", "bot-1")
	if ch.Name() != "memory" {
		t.Errorf("default Name: got %q, want memory", ch.Name())
	}
}
