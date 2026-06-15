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

// uppercaseStage 将文本转大写（模拟 enricher）。
type uppercaseStage struct{}

func (s *uppercaseStage) Name() string { return "uppercase" }
func (s *uppercaseStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.Set("original_text", env.Message.Text)
	return env, nil
}

// buildTestPipeline 创建用于测试的 Pipeline。
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

// ============================================================================
// Engine 集成测试
// ============================================================================

func TestEngine_EndToEnd(t *testing.T) {
	// 构建 Pipeline：uppercase(10) → echo(20)
	p := buildTestPipeline(t,
		core.StageInfo{Stage: &uppercaseStage{}, Order: 10, Enabled: true},
		core.StageInfo{Stage: &echoStage{}, Order: 20, Enabled: true},
	)

	// 创建 MemorySource
	src := inbound.NewMemorySource("test", 16)

	// 收集 Dispatcher
	disp := &collectDispatcher{}

	// 创建 Engine
	engine := NewEngine(
		[]inbound.Source{src},
		p,
		disp,
		EngineConfig{Workers: 2, InboundBuffer: 16, ShutdownTimeout: 5 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	// 启动 Engine
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	// 等一下让 Engine 启动
	time.Sleep(50 * time.Millisecond)

	// 发送 3 条消息
	for i := 0; i < 3; i++ {
		src.Send(core.Message{
			ID:      fmt.Sprintf("msg-%d", i),
			Channel: "test-ch",
			UserID:  "user-1",
			Text:    fmt.Sprintf("hello %d", i),
		})
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

	// 验证
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

	// 优雅关闭
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
	// 空 Pipeline：消息通过但无 Action
	p := buildTestPipeline(t)

	src := inbound.NewMemorySource("test", 8)
	disp := &collectDispatcher{}

	engine := NewEngine(
		[]inbound.Source{src},
		p,
		disp,
		EngineConfig{Workers: 1, InboundBuffer: 8, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	src.Send(core.Message{ID: "msg-empty", Channel: "ch", Text: "hello"})

	// 给一点时间处理
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
	// Stage 返回 nil → 消息被丢弃
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

	src := inbound.NewMemorySource("test", 8)
	disp := &collectDispatcher{}

	engine := NewEngine(
		[]inbound.Source{src},
		p,
		disp,
		EngineConfig{Workers: 1, InboundBuffer: 8, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	src.Send(core.Message{ID: "msg-drop", Channel: "ch", Text: "should be dropped"})
	time.Sleep(100 * time.Millisecond)

	if len(disp.collected()) != 0 {
		t.Errorf("expected 0 actions for dropped message, got %d", len(disp.collected()))
	}

	cancel()
	<-errCh
}

func TestEngine_MultipleSources(t *testing.T) {
	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	src1 := inbound.NewMemorySource("source-a", 8)
	src2 := inbound.NewMemorySource("source-b", 8)
	disp := &collectDispatcher{}

	engine := NewEngine(
		[]inbound.Source{src1, src2},
		p,
		disp,
		EngineConfig{Workers: 2, InboundBuffer: 16, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	src1.Send(core.Message{ID: "a-1", Channel: "ch-a", Text: "from a"})
	src2.Send(core.Message{ID: "b-1", Channel: "ch-b", Text: "from b"})

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

	// 验证两个 source 都有消息处理
	channels := map[string]bool{}
	for _, a := range actions {
		channels[a.Channel] = true
	}
	if !channels["ch-a"] || !channels["ch-b"] {
		t.Errorf("expected actions from both channels, got %v", channels)
	}

	cancel()
	<-errCh
}

func TestEngine_GracefulShutdown(t *testing.T) {
	p := buildTestPipeline(t,
		core.StageInfo{Stage: &echoStage{}, Order: 10, Enabled: true},
	)

	src := inbound.NewMemorySource("test", 8)
	disp := &collectDispatcher{}

	engine := NewEngine(
		[]inbound.Source{src},
		p,
		disp,
		EngineConfig{Workers: 1, InboundBuffer: 8, ShutdownTimeout: 3 * time.Second},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// 用 Stop 方法停止
	engine.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("engine returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop in time")
	}

	// 二次调用 cancel 不应 panic
	cancel()
}

func TestEngine_DefaultConfig(t *testing.T) {
	cfg := DefaultEngineConfig()
	if cfg.Workers != 4 {
		t.Errorf("default workers: got %d, want 4", cfg.Workers)
	}
	if cfg.InboundBuffer != 128 {
		t.Errorf("default buffer: got %d, want 128", cfg.InboundBuffer)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("default shutdown timeout: got %v, want 10s", cfg.ShutdownTimeout)
	}
}
