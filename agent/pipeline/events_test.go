package pipeline

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func noopTracerProvider() trace.TracerProvider {
	return nooptrace.NewTracerProvider()
}

func noopMeterProvider() metric.MeterProvider {
	return noopmetric.NewMeterProvider()
}

func TestPipelineEmitsStageEvents(t *testing.T) {
	sink := core.NewMemorySink(64)
	p, err := New([]core.StageInfo{
		{Stage: noopStage("a"), Order: 10, Enabled: true},
		{Stage: noopStage("b"), Order: 20, Enabled: true},
	}, noopTracerProvider(), noopMeterProvider(), zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	p.SetSink(sink)

	env := &core.Envelope{}
	if _, err := p.Execute(context.Background(), env); err != nil {
		t.Fatal(err)
	}

	evs := sink.Snapshot()
	// 期望：stage/start a, stage/end a, stage/start b, stage/end b
	if len(evs) != 4 {
		t.Fatalf("want 4 events, got %d: %+v", len(evs), evs)
	}
	wantKinds := []core.EventKind{
		core.EventStageStart, core.EventStageEnd,
		core.EventStageStart, core.EventStageEnd,
	}
	for i, k := range wantKinds {
		if evs[i].Kind != k {
			t.Errorf("ev %d kind=%s want %s", i, evs[i].Kind, k)
		}
	}
	if evs[0].Source != "a" || evs[2].Source != "b" {
		t.Errorf("sources: %q %q", evs[0].Source, evs[2].Source)
	}
	if evs[1].Seq <= evs[0].Seq {
		t.Error("seq not increasing")
	}
}

func TestPipelineSkipsDisabledStageEvents(t *testing.T) {
	sink := core.NewMemorySink(64)
	p, err := New([]core.StageInfo{
		{Stage: noopStage("on"), Order: 10, Enabled: true},
		{Stage: noopStage("off"), Order: 20, Enabled: false},
	}, noopTracerProvider(), noopMeterProvider(), zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	p.SetSink(sink)
	if _, err := p.Execute(context.Background(), &core.Envelope{}); err != nil {
		t.Fatal(err)
	}
	evs := sink.Snapshot()
	// 仅 enabled 的 "on" 应产生 start/end；disabled 的 "off" 不发事件。
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	for _, e := range evs {
		if e.Source != "on" {
			t.Errorf("unexpected event source %q", e.Source)
		}
	}
}

// TestPipelineContextCarriesSink 验证 Execute 把 sink 注入 ctx 后，
// 下游 Stage / 工具循环能通过 EventSinkFromContext(ctx) 取到并追加事件（C1 深层集成）。
func TestPipelineContextCarriesSink(t *testing.T) {
	sink := core.NewMemorySink(64)
	p, err := New([]core.StageInfo{
		{Stage: &core.StageFunc{
			StageName: "probe",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				// 模拟工具循环：从 ctx 取 sink 发事件（run_code 等工具正是此路径）。
				core.EventSinkFromContext(ctx).Emit(ctx, core.Event{
					Kind:    core.EventContextInject,
					Source:  "probe",
					Surface: true,
				})
				return env, nil
			},
		}, Order: 10, Enabled: true},
	}, noopTracerProvider(), noopMeterProvider(), zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	p.SetSink(sink)
	if _, err := p.Execute(context.Background(), &core.Envelope{}); err != nil {
		t.Fatal(err)
	}
	evs := sink.Snapshot()
	// 期望：stage/start probe, probe 经 ctx 发 context/inject, stage/end probe = 3 条。
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(evs), evs)
	}
	if evs[1].Kind != core.EventContextInject {
		t.Errorf("middle event kind = %s, want context/inject", evs[1].Kind)
	}
	if !evs[1].Surface {
		t.Error("context/inject event should be marked surface")
	}
}
