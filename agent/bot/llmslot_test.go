package bot

import (
	"context"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

type markProvider struct {
	name string
	got  chan string
}

func (p *markProvider) Name() string { return p.name }
func (p *markProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	id := ""
	if params.Model != nil {
		id = params.Model.ID
	}
	if p.got != nil {
		p.got <- id
	}
	return &llm.GenerateResult{Text: p.name}, nil
}
func (p *markProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errString("not used")
}

func TestLLMSlot_PinnedContextKeepsOldSnapshot(t *testing.T) {
	first := &markProvider{name: "first"}
	second := &markProvider{name: "second"}
	slot := NewLLMSlot(&LLMSnapshot{Main: first, MainModel: llm.ChatModel("model-first")})
	follow := newFollow(slot, "main")
	ctx := WithLLMSnapshot(context.Background(), slot.Current())
	slot.Publish(&LLMSnapshot{Main: second, MainModel: llm.ChatModel("model-second")})

	res, err := follow.DoGenerate(ctx, llm.GenerateParams{Model: llm.ChatModel("stale-id")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "first" {
		t.Fatalf("pinned call used %s", res.Text)
	}
	res, err = follow.DoGenerate(context.Background(), llm.GenerateParams{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "second" {
		t.Fatalf("next call used %s", res.Text)
	}
}

func TestFollow_InflightKeepsOldParamsNextCallUsesNew(t *testing.T) {
	var got []callSeen
	capture := &captureParams{got: &got}
	stats := &statsSeen{}
	// Stats sits inside the snapshot, follow sits outside, matching startup.
	recorded := llm.NewStatsRecordingProvider(capture, stats, "bot-synth")
	oldTemp := 0.2
	newTemp := 0.8
	slot := NewLLMSlot(&LLMSnapshot{
		Main:      recorded,
		MainModel: llm.ChatModel("glm-5.2"),
		MainDef:   config.ModelDef{Model: "glm-5.2", MaxTokens: 111, Temperature: &oldTemp},
	})
	follow := newFollow(slot, "main")
	pinned := WithLLMSnapshot(context.Background(), slot.Current())
	staleTemp := 9.0
	staleMax := 9
	staleEffort := "medium"
	call := func(ctx context.Context) {
		t.Helper()
		_, err := follow.DoGenerate(ctx, llm.GenerateParams{
			Model: llm.ChatModel("model-startup"), Temperature: &staleTemp, MaxTokens: &staleMax, ReasoningEffort: &staleEffort,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	call(pinned)
	slot.Publish(&LLMSnapshot{
		Main:      recorded,
		MainModel: llm.ChatModel("glm-5.3"),
		MainDef:   config.ModelDef{Model: "glm-5.3", MaxTokens: 222, Temperature: &newTemp},
	})
	call(pinned)
	call(context.Background())
	if len(got) != 3 || len(stats.models) != 3 {
		t.Fatalf("calls %d stats %v", len(got), stats.models)
	}
	if got[0].model != "glm-5.2" || got[0].temp != 0.2 || got[0].max != 111 || got[0].effort != "medium" {
		t.Fatalf("first inflight %+v", got[0])
	}
	if got[1].model != "glm-5.2" || got[1].temp != 0.2 || got[1].max != 111 || got[1].effort != "medium" {
		t.Fatalf("pinned after publish %+v", got[1])
	}
	if got[2].model != "glm-5.3" || got[2].temp != 0.8 || got[2].max != 222 || got[2].effort != "high" {
		t.Fatalf("next call %+v", got[2])
	}
	if stats.models[0] != "glm-5.2" || stats.models[1] != "glm-5.2" || stats.models[2] != "glm-5.3" {
		t.Fatalf("stats saw %v", stats.models)
	}
}

type callSeen struct {
	model  string
	temp   float64
	max    int
	effort string
}

type statsSeen struct {
	models []string
}

func (s *statsSeen) RecordUsage(_ context.Context, metric llm.UsageMetric) {
	s.models = append(s.models, metric.Model)
}

type captureParams struct {
	got *[]callSeen
}

func (p *captureParams) Name() string { return "capture" }
func (p *captureParams) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	item := callSeen{}
	if params.Model != nil {
		item.model = params.Model.ID
	}
	if params.Temperature != nil {
		item.temp = *params.Temperature
	}
	if params.MaxTokens != nil {
		item.max = *params.MaxTokens
	}
	if params.ReasoningEffort != nil {
		item.effort = *params.ReasoningEffort
	}
	*p.got = append(*p.got, item)
	return &llm.GenerateResult{Text: "ok", Usage: llm.Usage{TotalTokens: 1}}, nil
}
func (p *captureParams) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errString("not used")
}

func TestLLMSlot_ConcurrentPublishStaysWhole(t *testing.T) {
	slot := NewLLMSlot(&LLMSnapshot{MainModel: llm.ChatModel("model-0")})
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "model-sol-main"
			if n%2 == 0 {
				id = "model-terra-light"
			}
			slot.Publish(&LLMSnapshot{MainModel: llm.ChatModel(id), MainDef: slot.Current().MainDef})
		}(i)
	}
	wg.Wait()
	cur := slot.Current()
	if cur == nil || (cur.MainModel.ID != "model-sol-main" && cur.MainModel.ID != "model-terra-light") {
		t.Fatalf("snapshot %v", cur)
	}
	if cur.Generation < 2 {
		t.Fatalf("generation %d", cur.Generation)
	}
}

func TestSnapshotCopiesOmitTemperature(t *testing.T) {
	omit := float64(config.OmitSamplingParam)
	bundle := &LLMBundle{MainDef: config.ModelDef{Model: "model-sol-main", Temperature: &omit}}
	snap := SnapshotFromBundle(bundle)
	if snap.MainModel == nil || !snap.MainModel.OmitsTemperature() {
		t.Fatalf("model %+v", snap.MainModel)
	}
}
