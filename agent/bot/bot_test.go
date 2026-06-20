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
	"github.com/kasuganosora/thinkbot/agent/memory"
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
		if err := memCh.Inject(context.Background(), core.Message{
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
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	_ = memCh.Inject(context.Background(), core.Message{
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
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	_ = memCh.Inject(context.Background(), core.Message{ID: "1", Text: "hi"})
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
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	// 两个 Channel 各发一条
	_ = ch1.Inject(context.Background(), core.Message{ID: "mk-1", Channel: "mk-ch", Text: "from misskey"})
	_ = ch2.Inject(context.Background(), core.Message{ID: "tg-1", Channel: "tg-ch", Text: "from telegram"})

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

func TestMemoryChannel_SenderInterface(t *testing.T) {
	ch := NewMemoryChannel("test-sender", "bot-1")

	// MemoryChannel 应该同时实现 Channel 和 Sender
	var _ Channel = ch
	var _ Sender = ch

	// Send 应该记录 action
	err := ch.Send(context.Background(), core.Action{
		Type:    core.ActionReply,
		Channel: "chat-1",
		Payload: "hello",
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	actions := ch.SentActions()
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Payload != "hello" {
		t.Errorf("payload: got %v, want hello", actions[0].Payload)
	}

	// LastSentAction
	last := ch.LastSentAction()
	if last == nil {
		t.Fatal("LastSentAction returned nil")
	}
	if last.Channel != "chat-1" {
		t.Errorf("last action channel: got %q, want chat-1", last.Channel)
	}

	// ClearSentActions
	ch.ClearSentActions()
	if len(ch.SentActions()) != 0 {
		t.Error("expected 0 actions after clear")
	}
	if ch.LastSentAction() != nil {
		t.Error("expected nil after clear")
	}
}

// ============================================================================
// Outbound 全链路测试：Pipeline → Dispatcher → ChannelReplyHandler → Sender
// ============================================================================

// replyWithSourceStage 生成带 source_channel 的 reply Action。
// 这是 Outbound 全链路所需的：Pipeline Stage 必须在 Action.Metadata 中设置 source_channel，
// ChannelReplyHandler 才能路由到正确的 Channel Sender。
type replyWithSourceStage struct{}

func (s *replyWithSourceStage) Name() string { return "reply-with-source" }
func (s *replyWithSourceStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: "echo: " + env.Message.Text,
		Metadata: map[string]any{
			"source_channel": env.Message.Source, // Pipeline Stage 应该从 Message.Source 获取来源 Channel
		},
	})
	return env, nil
}

func TestBot_OutboundFullPipeline(t *testing.T) {
	// 测试完整的双向链路：
	// MemoryChannel.Inject → Ingress → Pipeline(replyWithSource) → MultiDispatcher
	//   → ChannelReplyHandler → MemoryChannel.Send

	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	// 使用 MultiDispatcher（Bot.New 会自动注册 ChannelReplyHandler）
	multiDisp := outbound.NewMultiDispatcher(logger, tp)

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &replyWithSourceStage{}, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("test-outbound", "outbound-bot")

	bot, err := New(BotParams{
		ID:         "outbound-bot",
		Config:     BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:   p,
		Dispatcher: multiDisp,
		Channels:   []Channel{memCh},
		Logger:     logger,
		TP:         tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- bot.Run(ctx)
	}()
	time.Sleep(100 * time.Millisecond)

	// 注入消息
	err = memCh.Inject(context.Background(), core.Message{
		ID:      "msg-1",
		Channel: "chat-42",
		UserID:  "user-1",
		Text:    "hello world",
	})
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// 等待 Sender 收到 action
	deadline := time.After(3 * time.Second)
	for len(memCh.SentActions()) < 1 {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for sent action, got %d", len(memCh.SentActions()))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 验证 Sender 收到的 action
	actions := memCh.SentActions()
	if len(actions) != 1 {
		t.Fatalf("expected 1 sent action, got %d", len(actions))
	}

	a := actions[0]
	if a.Type != core.ActionReply {
		t.Errorf("action type: got %s, want reply", a.Type)
	}
	if a.Channel != "chat-42" {
		t.Errorf("action channel: got %q, want chat-42", a.Channel)
	}
	if a.Payload != "echo: hello world" {
		t.Errorf("action payload: got %v, want echo: hello world", a.Payload)
	}
	if a.Metadata["source_channel"] != "test-outbound" {
		t.Errorf("source_channel: got %v, want test-outbound", a.Metadata["source_channel"])
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("bot error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bot did not stop in time")
	}
}

func TestBot_OutboundMultipleChannels(t *testing.T) {
	// 测试多个 Channel 的消息各自路由到正确的 Sender

	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	multiDisp := outbound.NewMultiDispatcher(logger, tp)

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &replyWithSourceStage{}, Order: 10, Enabled: true},
	)

	ch1 := NewMemoryChannel("ch-misskey", "multi-out-bot")
	ch2 := NewMemoryChannel("ch-telegram", "multi-out-bot")

	bot, err := New(BotParams{
		ID:         "multi-out-bot",
		Config:     BotConfig{Workers: 2, IngressBufferSize: 16},
		Pipeline:   p,
		Dispatcher: multiDisp,
		Channels:   []Channel{ch1, ch2},
		Logger:     logger,
		TP:         tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// ch1 注入一条消息
	_ = ch1.Inject(context.Background(), core.Message{
		ID: "mk-1", Channel: "note-1", Text: "from misskey",
	})
	// ch2 注入一条消息
	_ = ch2.Inject(context.Background(), core.Message{
		ID: "tg-1", Channel: "chat-2", Text: "from telegram",
	})

	// 等待两个 sender 各收到一条
	deadline := time.After(3 * time.Second)
	for len(ch1.SentActions()) < 1 || len(ch2.SentActions()) < 1 {
		select {
		case <-deadline:
			t.Fatalf("timeout: ch1=%d, ch2=%d", len(ch1.SentActions()), len(ch2.SentActions()))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// ch1 的 sender 应该收到 misskey 的回复
	a1 := ch1.SentActions()[0]
	if a1.Channel != "note-1" {
		t.Errorf("ch1 action channel: got %q, want note-1", a1.Channel)
	}
	if a1.Payload != "echo: from misskey" {
		t.Errorf("ch1 action payload: got %v", a1.Payload)
	}

	// ch2 的 sender 应该收到 telegram 的回复
	a2 := ch2.SentActions()[0]
	if a2.Channel != "chat-2" {
		t.Errorf("ch2 action channel: got %q, want chat-2", a2.Channel)
	}
	if a2.Payload != "echo: from telegram" {
		t.Errorf("ch2 action payload: got %v", a2.Payload)
	}

	cancel()
}

// ============================================================================
// Outbound NoteHandler 测试：Pipeline → Dispatcher → NoteHandler → NoteStore
// ============================================================================

// noteOnlyStage 只产出 ActionNote，不回复用户。
type noteOnlyStage struct{}

func (s *noteOnlyStage) Name() string { return "note-only" }
func (s *noteOnlyStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.AddAction(core.Action{
		Type:    core.ActionNote,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: "observed: " + env.Message.Text,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"category":       "observation",
		},
	})
	return env, nil
}

// replyAndNoteStage 同时产出 ActionReply 和 ActionNote。
type replyAndNoteStage struct{}

func (s *replyAndNoteStage) Name() string { return "reply-and-note" }
func (s *replyAndNoteStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	// Reply 回写到 Channel
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: "reply: " + env.Message.Text,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
		},
	})
	// Note 记录备忘
	env.AddAction(core.Action{
		Type:    core.ActionNote,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: "memo: user said " + env.Message.Text,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
			"bot_id":         env.Message.BotID,
			"message_id":     env.Message.ID,
			"category":       "insight",
		},
	})
	return env, nil
}

func TestBot_NoteOnlyPipeline(t *testing.T) {
	// 测试"只备注不回复"的完整链路：
	// Inject → Pipeline(noteOnly) → MultiDispatcher → NoteHandler → memory.Store
	// Channel Sender 不应收到任何 action（不回复）

	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	memRepo := memory.NewMemoryRepository()
	multiDisp := outbound.NewMultiDispatcher(logger, tp)

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &noteOnlyStage{}, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("test-note", "note-bot")

	bot, err := New(BotParams{
		ID:          "note-bot",
		Config:      BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:    p,
		Dispatcher:  multiDisp,
		Channels:    []Channel{memCh},
		MemoryStore: memRepo,
		Logger:      logger,
		TP:          tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// 注入消息
	_ = memCh.Inject(context.Background(), core.Message{
		ID:      "msg-1",
		Channel: "user-1",
		UserID:  "u1",
		Text:    "interesting observation",
	})

	// 等待记忆仓储收到备注
	deadline := time.After(3 * time.Second)
	for {
		count, _ := memRepo.Count(context.Background(), memory.ChannelScope("user-1"))
		if count >= 1 {
			break
		}
		select {
		case <-deadline:
			c, _ := memRepo.Count(context.Background(), memory.ChannelScope("user-1"))
			t.Fatalf("timeout waiting for note, got %d", c)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 验证备注内容（现在是 memory.Entry）
	entries, _ := memRepo.Recent(context.Background(), memory.ChannelScope("user-1"), 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Content != "observed: interesting observation" {
		t.Errorf("entry content = %q", e.Content)
	}
	if e.Source != "note" {
		t.Errorf("entry source = %q, want note", e.Source)
	}
	if e.Scope.ID != "user-1" {
		t.Errorf("entry scope.ID = %q, want user-1", e.Scope.ID)
	}
	if e.Category != "observation" {
		t.Errorf("entry category = %q, want observation", e.Category)
	}

	// Channel Sender 不应收到任何消息（只备注不回复）
	if len(memCh.SentActions()) != 0 {
		t.Errorf("Sender should not receive actions in note-only mode, got %d", len(memCh.SentActions()))
	}

	cancel()
}

func TestBot_ReplyAndNotePipeline(t *testing.T) {
	// 测试"回复 + 备注"的完整链路：
	// Inject → Pipeline(replyAndNote) → MultiDispatcher
	//   → ChannelReplyHandler → Sender（回复）
	//   → NoteHandler → memory.Store（备注纳入记忆）

	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	memRepo := memory.NewMemoryRepository()
	multiDisp := outbound.NewMultiDispatcher(logger, tp)

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &replyAndNoteStage{}, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("test-both", "both-bot")

	bot, err := New(BotParams{
		ID:          "both-bot",
		Config:      BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:    p,
		Dispatcher:  multiDisp,
		Channels:    []Channel{memCh},
		MemoryStore: memRepo,
		Logger:      logger,
		TP:          tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// 注入消息
	_ = memCh.Inject(context.Background(), core.Message{
		ID:      "msg-1",
		Channel: "chat-99",
		UserID:  "u1",
		Text:    "deploy failed again",
	})

	// 等待 Sender 和 MemoryStore 都收到数据
	deadline := time.After(3 * time.Second)
	for {
		count, _ := memRepo.Count(context.Background(), memory.ChannelScope("chat-99"))
		if len(memCh.SentActions()) >= 1 && count >= 1 {
			break
		}
		select {
		case <-deadline:
			c, _ := memRepo.Count(context.Background(), memory.ChannelScope("chat-99"))
			t.Fatalf("timeout: sent=%d, entries=%d", len(memCh.SentActions()), c)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 验证回复
	sentActions := memCh.SentActions()
	if len(sentActions) != 1 {
		t.Fatalf("expected 1 sent action, got %d", len(sentActions))
	}
	if sentActions[0].Payload != "reply: deploy failed again" {
		t.Errorf("sent payload = %v", sentActions[0].Payload)
	}
	if sentActions[0].Channel != "chat-99" {
		t.Errorf("sent channel = %q, want chat-99", sentActions[0].Channel)
	}

	// 验证备注（现在是 memory.Entry）
	entries, _ := memRepo.Recent(context.Background(), memory.ChannelScope("chat-99"), 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Content != "memo: user said deploy failed again" {
		t.Errorf("entry content = %q", entries[0].Content)
	}
	if entries[0].Category != "insight" {
		t.Errorf("entry category = %q, want insight", entries[0].Category)
	}
	if entries[0].Source != "note" {
		t.Errorf("entry source = %q, want note", entries[0].Source)
	}

	cancel()
}

// ============================================================================
// Outbound CallbackHandler + SilentHandler 测试
// ============================================================================

// callbackStage 产出 ActionCallback。
type callbackStage struct{}

func (s *callbackStage) Name() string { return "callback" }
func (s *callbackStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.AddAction(core.Action{
		Type:    core.ActionCallback,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: "task result: " + env.Message.Text,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
			"callback_id":    "parent-task-1",
			"status":         "success",
		},
	})
	return env, nil
}

// silentStage 产出 ActionSilent。
type silentStage struct{}

func (s *silentStage) Name() string { return "silent" }
func (s *silentStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.AddAction(core.Action{
		Type:    core.ActionSilent,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
			"reason":         "irrelevant chatter",
		},
	})
	return env, nil
}

func TestBot_CallbackPipeline(t *testing.T) {
	// 测试回调全链路：
	// Inject → Pipeline(callback) → MultiDispatcher → CallbackHandler → CallbackRegistry.Invoke

	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	callbackRegistry := outbound.NewMemoryCallbackRegistry()
	multiDisp := outbound.NewMultiDispatcher(logger, tp)

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &callbackStage{}, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("test-callback", "cb-bot")

	_, err := New(BotParams{
		ID:               "cb-bot",
		Config:           BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:         p,
		Dispatcher:       multiDisp,
		Channels:         []Channel{memCh},
		CallbackRegistry: callbackRegistry,
		Logger:           logger,
		TP:               tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// 注册回调
	var callbackResult outbound.CallbackResult
	var callbackCalled bool
	callbackRegistry.Register("parent-task-1", func(ctx context.Context, result outbound.CallbackResult) error {
		callbackResult = result
		callbackCalled = true
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Re-create bot (to pick up the callback registered above)
	bot, err := New(BotParams{
		ID:               "cb-bot-2",
		Config:           BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:         p,
		Dispatcher:       multiDisp,
		Channels:         []Channel{memCh},
		CallbackRegistry: callbackRegistry,
		Logger:           logger,
		TP:               tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// 注入消息
	_ = memCh.Inject(context.Background(), core.Message{
		ID:      "msg-cb",
		Channel: "conv-1",
		UserID:  "u1",
		Text:    "analysis complete",
	})

	// 等待回调被调用
	deadline := time.After(3 * time.Second)
	for !callbackCalled {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for callback")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 验证回调结果
	if callbackResult.CallbackID != "parent-task-1" {
		t.Errorf("callback_id = %q", callbackResult.CallbackID)
	}
	if callbackResult.Payload != "task result: analysis complete" {
		t.Errorf("payload = %v", callbackResult.Payload)
	}
	if callbackResult.Status != "success" {
		t.Errorf("status = %q", callbackResult.Status)
	}

	// Channel Sender 不应收到任何消息（回调不走 Channel 输出）
	if len(memCh.SentActions()) != 0 {
		t.Errorf("Sender should not receive actions in callback mode, got %d", len(memCh.SentActions()))
	}

	cancel()
}

func TestBot_SilentPipeline(t *testing.T) {
	// 测试静默全链路：
	// Inject → Pipeline(silent) → MultiDispatcher → SilentHandler → 仅 trace/log

	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	multiDisp := outbound.NewMultiDispatcher(logger, tp)

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &silentStage{}, Order: 10, Enabled: true},
	)

	memCh := NewMemoryChannel("test-silent", "silent-bot")

	bot, err := New(BotParams{
		ID:         "silent-bot",
		Config:     BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:   p,
		Dispatcher: multiDisp,
		Channels:   []Channel{memCh},
		Logger:     logger,
		TP:         tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bot.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// 注入消息
	_ = memCh.Inject(context.Background(), core.Message{
		ID:      "msg-silent",
		Channel: "chat-group",
		UserID:  "u2",
		Text:    "random chatter",
	})

	// 等待一小段时间，确认没有任何输出
	time.Sleep(500 * time.Millisecond)

	// Channel Sender 不应收到任何消息
	if len(memCh.SentActions()) != 0 {
		t.Errorf("Sender should not receive actions in silent mode, got %d", len(memCh.SentActions()))
	}

	cancel()
}

func TestBot_CallbackRegistry_Access(t *testing.T) {
	// 测试 Bot.CallbackRegistry() 返回正确的 registry
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	reg := outbound.NewMemoryCallbackRegistry()
	multiDisp := outbound.NewMultiDispatcher(logger, tp)

	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	bot, err := New(BotParams{
		ID:               "access-bot",
		Config:           BotConfig{Workers: 1, IngressBufferSize: 8},
		Pipeline:         p,
		Dispatcher:       multiDisp,
		Channels:         []Channel{},
		CallbackRegistry: reg,
		Logger:           logger,
		TP:               tp,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// 通过 Bot.CallbackRegistry() 注册回调
	botReg := bot.CallbackRegistry()
	botReg.Register("test-cb", func(ctx context.Context, result outbound.CallbackResult) error { return nil })

	// 验证 registry 是同一个实例
	if !reg.Has("test-cb") {
		t.Error("expected callback registered via Bot.CallbackRegistry() to be in the original registry")
	}
}
