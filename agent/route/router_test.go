package route

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	noop_metric "go.opentelemetry.io/otel/metric/noop"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

type scriptProvider struct {
	mu    sync.Mutex
	calls int
	seen  []string
	reply func(n int, params llm.GenerateParams) (*llm.GenerateResult, error)
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.seen = append(p.seen, params.System+"\n"+textOf(params.Messages))
	p.mu.Unlock()
	return p.reply(n, params)
}

func (p *scriptProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("l1 routing is not streamed")
}

func (p *scriptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *scriptProvider) prompt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		return ""
	}
	return p.seen[0]
}

func textOf(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, part := range m.Content {
			if t, ok := part.(llm.TextPart); ok {
				b.WriteString(t.Text)
			}
		}
	}
	return b.String()
}

func testRouter(t *testing.T, prov llm.Provider) *Router {
	t.Helper()
	r, err := New(Config{
		Provider: prov,
		Model:    llm.ChatModel("light-test"),
		Logger:   zap.NewNop().Sugar(),
		TP:       noop_trace.NewTracerProvider(),
		MP:       noop_metric.NewMeterProvider(),
		Limits:   Limits{MaxConcurrent: 2, MaxOpens: 4, OpenWindow: time.Minute},
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return r
}

func userEnv(text, trace string) *core.Envelope {
	env := core.NewEnvelope(core.Message{
		ID: "m-" + trace, BotID: "bot", Source: "web", Channel: "web:1",
		Text: text, TraceID: trace,
		Metadata: map[string]any{"chat_session_id": "sess-1"},
	})
	env.Set(core.KVMemoryRecall, "SENTINEL_MEMORY_BLOCK")
	return env
}

func TestL1PipelineDoesNotLoadMemoryAndOnlyExposesRouteTools(t *testing.T) {
	prov := &scriptProvider{reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{{
				ToolCallID: "c1", ToolName: ToolOpen,
				Input: map[string]any{"intent": "chat", "model": "light"},
			}},
		}, nil
	}}
	r := testRouter(t, prov)
	if names := r.Pipeline().StageNames(); len(names) != 1 || names[0] != "l1-route" {
		t.Fatalf("L1 stages = %v, want only l1-route", names)
	}
	env := userEnv("hello", "trace-1")
	d := r.Route(context.Background(), env)
	if d.Action != ActionOpen || d.Intent != IntentChat || d.Model != ModelLight {
		t.Fatalf("decision = %+v", d)
	}
	if strings.Contains(prov.prompt(), "SENTINEL_MEMORY_BLOCK") {
		t.Fatal("L1 prompt included recalled memory")
	}
	if strings.Contains(prov.prompt(), "memory") && strings.Contains(prov.prompt(), "search") {
		t.Fatal("L1 prompt should not instruct memory search")
	}
	got := IntentOf(env)
	if got != IntentChat || ModelOf(env) != ModelLight {
		t.Fatalf("tier intent=%s model=%s", got, ModelOf(env))
	}
}

func TestPreemptKeepsOwnerStream(t *testing.T) {
	ch := make(chan string, 1)
	var r *Router
	prov := &scriptProvider{reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{{
				ToolCallID: "c1", ToolName: ToolPreempt,
				Input: map[string]any{"pipeline_id": "trace-owner"},
			}},
		}, nil
	}}
	r = testRouter(t, prov)
	r.Registry().Add(Run{
		ID: "trace-owner", BotID: "bot", Key: "chat:sess-1", TraceID: "trace-owner",
		Summary: "writing the report", Interrupt: ch,
	})
	env := userEnv("use last quarter's numbers", "trace-new")
	d := r.Route(context.Background(), env)
	if d.Action != ActionPreempt || d.Target != "trace-owner" {
		t.Fatalf("decision = %+v", d)
	}
	if r.OwnsReply("trace-new") {
		t.Fatal("new trace must not own the reply")
	}
	select {
	case got := <-ch:
		if got != "use last quarter's numbers" {
			t.Fatalf("injected %q", got)
		}
	default:
		t.Fatal("nothing injected into the running pipeline")
	}
	if !strings.Contains(prov.prompt(), "id=trace-owner") {
		t.Fatalf("prompt missing running pipeline:\n%s", prov.prompt())
	}
}

func TestSecondRouteToolDoesNotOpenAnotherPipeline(t *testing.T) {
	prov := &scriptProvider{reply: func(n int, params llm.GenerateParams) (*llm.GenerateResult, error) {
		if n > 1 {
			return nil, errors.New("L1 called the model again after a route was chosen")
		}
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "c1", ToolName: ToolOpen, Input: map[string]any{"intent": "task"}},
				{ToolCallID: "c2", ToolName: ToolOpen, Input: map[string]any{"intent": "task"}},
			},
		}, nil
	}}
	r := testRouter(t, prov)
	d := r.Route(context.Background(), userEnv("do the thing", "trace-1"))
	if d.Action != ActionOpen {
		t.Fatalf("decision = %+v", d)
	}
	if got := r.Registry().Opens("chat:sess-1"); got != 1 {
		t.Fatalf("opens = %d, want 1", got)
	}
	if prov.callCount() != 1 {
		t.Fatalf("model calls = %d, want 1", prov.callCount())
	}
}

func TestRouteFallbackPreemptsWhenBudgetIsSpent(t *testing.T) {
	ch := make(chan string, 1)
	prov := &scriptProvider{reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{{
				ToolCallID: "c1", ToolName: ToolOpen, Input: map[string]any{"intent": "task"},
			}},
		}, nil
	}}
	r := testRouter(t, prov)
	latest := make(chan string, 1)
	r.Registry().Add(Run{ID: "a", BotID: "bot", Key: "chat:sess-1", TraceID: "a", Interrupt: ch})
	r.Registry().Add(Run{ID: "b", BotID: "bot", Key: "chat:sess-1", TraceID: "b", Interrupt: latest})
	d := r.Route(context.Background(), userEnv("another task", "trace-new"))
	if d.Action != ActionPreempt || d.Target != "b" {
		t.Fatalf("over budget should fall back to the latest pipeline, got %+v", d)
	}
	select {
	case <-latest:
	default:
		t.Fatal("fallback preempt did not inject")
	}
}

func TestSkipSystemAndCommands(t *testing.T) {
	r := testRouter(t, &scriptProvider{reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		t.Fatal("router model should not run")
		return nil, nil
	}})
	hb := core.NewEnvelope(core.Message{Source: core.SourceHeartbeat, Text: "wake", TraceID: "h"})
	if r.ShouldRoute(hb) {
		t.Fatal("heartbeat must stay on L2")
	}
	cmd := userEnv("/help", "c")
	if r.ShouldRoute(cmd) {
		t.Fatal("slash commands must stay on L2")
	}
	code := userEnv("TB-ABCD-EFGH", "b")
	if r.ShouldRoute(code) {
		t.Fatal("bind codes must stay on L2")
	}
	env := userEnv("hi", "u")
	SetLevel(env, LevelL2)
	if r.ShouldRoute(env) {
		t.Fatal("an L2 execution must not re-enter L1")
	}
}
