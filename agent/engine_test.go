package agent

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
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
)

// ============================================================================
// 测试辅助
// ============================================================================

// collectDispatcher 收集所有 dispatch 的 Action（线程安全）。
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

// echoStage 为每条消息生成一个 reply Action，将消息文本作为 payload。
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

// uppercaseStage 模拟 enricher：记录原始文本到 KV。
type uppercaseStage struct{}

func (s *uppercaseStage) Name() string { return "uppercase" }
func (s *uppercaseStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.Set("original_text", env.Message.Text)
	return env, nil
}

// buildTestPipeline 创建测试用 Pipeline。
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

// buildTestIngress 创建测试用 Ingress。
func buildTestIngress(bufSize int) *inbound.Ingress {
	return inbound.NewIngress(
		inbound.IngressConfig{BufferSize: bufSize},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)
}

// ============================================================================
// Engine 集成测试
// ============================================================================

func TestEngine_EndToEnd(t *testing.T) {
	// Pipeline: uppercase(10) → echo(20)
	p := buildTestPipeline(t,
		core.StageInfo{Stage: &uppercaseStage{}, Order: 10, Enabled: true},
		core.StageInfo{Stage: &echoStage{}, Order: 20, Enabled: true},
	)

	ingress := buildTestIngress(16)
	disp := &collectDispatcher{}

	engine := NewEngine(
		ingress, p, disp,
		EngineConfig{Workers: 2, ShutdownTimeout: 5 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// 通过 Ingress 直接注入 3 条消息
	bgCtx := context.Background()
	for i := 0; i < 3; i++ {
		if err := ingress.Receive(bgCtx, core.Message{
			ID:      fmt.Sprintf("msg-%d", i),
			Source:  "test",
			Channel: "test-ch",
			UserID:  "user-1",
			Text:    fmt.Sprintf("hello %d", i),
		}); err != nil {
			t.Fatalf("Receive failed: %v", err)
		}
	}

	// 等待处理完成
	deadline := time.After(3 * time.Second)
	for {
		if len(disp.collected()) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for dispatch, got %d actions", len(disp.collected()))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	actions := disp.collected()
	if len(actions) != 3 {
		t.Fatalf("expected 3 actions, got %d", len(actions))
	}
	for i, a := range actions {
		if a.Type != core.ActionReply {
			t.Errorf("action[%d] type: got %s, want reply", i, a.Type)
		}
		if a.Channel != "test-ch" {
			t.Errorf("action[%d] channel: got %s, want test-ch", i, a.Channel)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("engine returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop in time")
	}
}

func TestEngine_EmptyPipeline(t *testing.T) {
	p := buildTestPipeline(t)
	ingress := buildTestIngress(8)
	disp := &collectDispatcher{}

	engine := NewEngine(
		ingress, p, disp,
		EngineConfig{Workers: 1, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	ingress.Receive(context.Background(), core.Message{ID: "msg-empty", Channel: "ch", Text: "hello"})
	time.Sleep(100 * time.Millisecond)

	if len(disp.collected()) != 0 {
		t.Errorf("expected 0 actions for empty pipeline, got %d", len(disp.collected()))
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("engine returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop in time")
	}
}

func TestEngine_DroppingStage(t *testing.T) {
	dropStage := &core.StageFunc{
		StageName: "drop",
		Fn: func(_ context.Context, _ *core.Envelope) (*core.Envelope, error) {
			return nil, nil
		},
	}

	p := buildTestPipeline(t,
		core.StageInfo{Stage: dropStage, Order: 10, Enabled: true},
		core.StageInfo{Stage: &echoStage{}, Order: 20, Enabled: true},
	)

	ingress := buildTestIngress(8)
	disp := &collectDispatcher{}

	engine := NewEngine(
		ingress, p, disp,
		EngineConfig{Workers: 1, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	ingress.Receive(context.Background(), core.Message{ID: "msg-drop", Channel: "ch", Text: "should be dropped"})
	time.Sleep(100 * time.Millisecond)

	if len(disp.collected()) != 0 {
		t.Errorf("expected 0 actions for dropped message, got %d", len(disp.collected()))
	}

	cancel()
	<-errCh
}

func TestEngine_MemoryChannel(t *testing.T) {
	// 验证通过 MemoryChannel 适配器注入消息
	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	ingress := buildTestIngress(16)
	mem := inbound.NewMemoryChannel("test-channel", ingress)
	disp := &collectDispatcher{}

	engine := NewEngine(
		ingress, p, disp,
		EngineConfig{Workers: 2, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// 通过 MemoryChannel 发送
	bgCtx := context.Background()
	mem.Send(bgCtx, core.Message{ID: "ch-1", Channel: "ch-a", Text: "from channel a"})
	mem.Send(bgCtx, core.Message{ID: "ch-2", Channel: "ch-b", Text: "from channel b"})

	deadline := time.After(3 * time.Second)
	for {
		if len(disp.collected()) >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout, got %d actions", len(disp.collected()))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	actions := disp.collected()
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}

	// 验证 source 被自动填充
	for _, a := range actions {
		// MemoryChannel 填充 source = "test-channel"
		// 这里只验证 action 正确产出
		if a.Type != core.ActionReply {
			t.Errorf("expected reply, got %s", a.Type)
		}
	}

	cancel()
	<-errCh
}

func TestEngine_GracefulShutdown(t *testing.T) {
	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	ingress := buildTestIngress(8)
	disp := &collectDispatcher{}

	engine := NewEngine(
		ingress, p, disp,
		EngineConfig{Workers: 1, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	engine.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("engine returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop in time")
	}

	cancel()
}

func TestEngine_IngressAccessor(t *testing.T) {
	ingress := buildTestIngress(8)
	p := buildTestPipeline(t)
	disp := &collectDispatcher{}

	engine := NewEngine(
		ingress, p, disp,
		DefaultEngineConfig(),
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	if engine.Ingress() != ingress {
		t.Fatal("Ingress() should return the same ingress instance")
	}
}

func TestEngine_DefaultConfig(t *testing.T) {
	cfg := DefaultEngineConfig()
	if cfg.Workers != 4 {
		t.Errorf("default workers: got %d, want 4", cfg.Workers)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("default shutdown timeout: got %v, want 10s", cfg.ShutdownTimeout)
	}
}
