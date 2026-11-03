package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	noop_metric "go.opentelemetry.io/otel/metric/noop"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/route"
	"github.com/kasuganosora/thinkbot/llm"
)

type routeScript struct {
	mu    sync.Mutex
	calls int
	reply func(n int) (*llm.GenerateResult, error)
}

func (p *routeScript) Name() string { return "route-script" }

func (p *routeScript) DoGenerate(context.Context, llm.GenerateParams) (*llm.GenerateResult, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	return p.reply(n)
}

func (p *routeScript) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("l1 is not streamed")
}

func (p *routeScript) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type countStage struct {
	n int
}

func (s *countStage) Name() string { return "count" }
func (s *countStage) Process(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
	s.n++
	env.AddAction(core.Action{Type: core.ActionReply, Payload: "ran"})
	return env, nil
}

func newRouteEngine(t *testing.T, prov llm.Provider, stage core.Stage) (*Engine, *collectDispatcher, *route.Router) {
	t.Helper()
	p := buildTestPipeline(t, core.StageInfo{Stage: stage, Order: 1, Enabled: true})
	r, err := route.New(route.Config{
		Provider: prov,
		Model:    llm.ChatModel("light-test"),
		Logger:   zap.NewNop().Sugar(),
		TP:       noop_trace.NewTracerProvider(),
		MP:       noop_metric.NewMeterProvider(),
	})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	eng := NewEngine(
		buildTestIngress(8), p, &collectDispatcher{},
		EngineConfig{Workers: 1},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
		WithRouter(r),
	)
	return eng, eng.dispatcher.(*collectDispatcher), r
}

// startHook records when the engine registers the turn (OnBeforeProcess).
// Channel placeholders treat "not registered" as "turn already over" after a
// short grace, so routing must not run before this hook.
type startHook struct {
	NoopEngineHook
	mu      sync.Mutex
	calls   int
	started bool
}

func (h *startHook) OnBeforeProcess(ctx context.Context, env *core.Envelope) context.Context {
	h.mu.Lock()
	h.calls++
	h.started = true
	h.mu.Unlock()
	return h.NoopEngineHook.OnBeforeProcess(ctx, env)
}

func (h *startHook) active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}

func TestEngine_L1RegistersTurnBeforeRouting(t *testing.T) {
	hook := &startHook{}
	prov := &routeScript{reply: func(int) (*llm.GenerateResult, error) {
		if !hook.active() {
			t.Errorf("L1 model ran before the turn was registered")
		}
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{{
				ToolCallID: "c1", ToolName: route.ToolOpen,
				Input: map[string]any{"intent": "task", "model": "default"},
			}},
		}, nil
	}}
	stage := &countStage{}
	eng, disp, _ := newRouteEngine(t, prov, stage)
	eng.hook = hook
	env := core.NewEnvelope(core.Message{
		ID: "m-reg", BotID: "bot", Source: "telegram", Channel: "42",
		Text: "ping", TraceID: "t-reg",
	})
	if _, _, err := eng.ProcessSync(context.Background(), env); err != nil {
		t.Fatalf("process: %v", err)
	}
	if !hook.active() || hook.calls != 1 {
		t.Fatalf("OnBeforeProcess calls = %d started=%v", hook.calls, hook.active())
	}
	if stage.n != 1 {
		t.Fatalf("execution runs = %d, want 1", stage.n)
	}
	if len(disp.collected()) != 1 {
		t.Fatalf("dispatched = %d", len(disp.collected()))
	}
}

func TestEngine_L1OpenRunsExecutionOnce(t *testing.T) {
	prov := &routeScript{reply: func(int) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{{
				ToolCallID: "c1", ToolName: route.ToolOpen,
				Input: map[string]any{"intent": "chat", "model": "light"},
			}},
		}, nil
	}}
	stage := &countStage{}
	eng, disp, _ := newRouteEngine(t, prov, stage)
	env := core.NewEnvelope(core.Message{
		ID: "m1", BotID: "bot", Source: "web", Channel: "web:1",
		Text: "hi", TraceID: "t-new",
		Metadata: map[string]any{"chat_session_id": "s1"},
	})
	if _, _, err := eng.ProcessSync(context.Background(), env); err != nil {
		t.Fatalf("process: %v", err)
	}
	if stage.n != 1 {
		t.Fatalf("execution runs = %d, want 1", stage.n)
	}
	if prov.callCount() != 1 {
		t.Fatalf("l1 calls = %d, want 1", prov.callCount())
	}
	if route.ModelOf(env) != route.ModelLight {
		t.Fatalf("model tier = %s", route.ModelOf(env))
	}
	if len(disp.collected()) != 1 {
		t.Fatalf("dispatched = %d", len(disp.collected()))
	}
}

func TestEngine_L1PreemptDoesNotWriteSecondReply(t *testing.T) {
	prov := &routeScript{reply: func(int) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{{
				ToolCallID: "c1", ToolName: route.ToolPreempt,
				Input: map[string]any{"pipeline_id": "t-owner"},
			}},
		}, nil
	}}
	stage := &countStage{}
	eng, disp, r := newRouteEngine(t, prov, stage)
	ch := make(chan string, 1)
	r.Registry().Add(route.Run{
		ID: "t-owner", BotID: "bot", Key: "chat:s1", TraceID: "t-owner",
		Summary: "draft", Interrupt: ch,
	})
	env := core.NewEnvelope(core.Message{
		ID: "m2", BotID: "bot", Source: "web", Channel: "web:1",
		Text: "add a table", TraceID: "t-new",
		Metadata: map[string]any{"chat_session_id": "s1"},
	})
	if _, actions, err := eng.ProcessSync(context.Background(), env); err != nil {
		t.Fatalf("process: %v", err)
	} else if actions != nil {
		t.Fatalf("preempt must not dispatch, got %#v", actions)
	}
	if stage.n != 0 {
		t.Fatalf("execution ran on the preempted message: %d", stage.n)
	}
	select {
	case got := <-ch:
		if got != "add a table" {
			t.Fatalf("injected %q", got)
		}
	default:
		t.Fatal("owner pipeline received no context")
	}
	if len(disp.collected()) != 0 {
		t.Fatalf("second reply was dispatched")
	}
	if r.OwnsReply("t-new") {
		t.Fatal("preempted trace still owns a reply stream")
	}
}

func TestEngine_AdoptedTraceCannotDispatch(t *testing.T) {
	prov := &routeScript{reply: func(int) (*llm.GenerateResult, error) {
		t.Fatal("l2 execution must not enter l1")
		return nil, nil
	}}
	stage := &countStage{}
	eng, disp, r := newRouteEngine(t, prov, stage)
	ch := make(chan string, 1)
	r.Registry().Add(route.Run{
		ID: "t-owner", BotID: "bot", Key: "chat:s1", TraceID: "t-owner", Interrupt: ch,
	})
	if err := r.Registry().Preempt("t-owner", "chat:s1", "t-new", "context"); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	env := core.NewEnvelope(core.Message{
		ID: "m3", BotID: "bot", Source: "web", Channel: "web:1",
		Text: "should not send", TraceID: "t-new",
		Metadata: map[string]any{"chat_session_id": "s1"},
	})
	route.SetLevel(env, route.LevelL2)
	if _, _, err := eng.ProcessSync(context.Background(), env); err != nil {
		t.Fatalf("process: %v", err)
	}
	if stage.n != 1 {
		t.Fatalf("execution runs = %d, want 1", stage.n)
	}
	if len(disp.collected()) != 0 {
		t.Fatal("adopted trace wrote a reply")
	}
}
