package stages

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/route"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestOmitTemperatureRequestShape(t *testing.T) {
	const modelID = "gpt-6.1-synth"
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"resp_1","status":"completed","model":"`+modelID+`",
			"output":[{"type":"function_call","call_id":"call_1","name":"pipeline_open","arguments":"{\"intent\":\"chat\",\"model\":\"light\"}"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"remember\":false}"}]}]
		}`)
	}))
	defer srv.Close()

	store := config.NewStore(nil)
	store.SetTemporary("llm.max_retries", "0")
	store.SetTemporary("provider.synth", `{"enabled":true,"clientType":"OpenAI Responses","baseUrl":"`+srv.URL+`/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"`+modelID+`","temperature":-1}]}`)
	bundle, err := bot.CreateLLMBundleFrom(config.NewBuilder(store, nil), "bot-synth", modelID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.MainDef.Temperature == nil || *bundle.MainDef.Temperature != llm.OmitSamplingParam {
		t.Fatalf("resolved temperature %+v", bundle.MainDef.Temperature)
	}
	snap := bot.SnapshotFromBundle(bundle)
	if !snap.MainModel.OmitsTemperature() {
		t.Fatal("snapshot did not keep the omit sentinel")
	}

	logger := zap.NewNop().Sugar()
	router, err := route.New(route.Config{Provider: bundle.Main, Model: snap.MainModel, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	env := core.NewEnvelope(core.Message{ID: "m1", BotID: "bot-synth", Channel: "c", Text: "hello", TraceID: "trace"})
	router.Route(context.Background(), env)

	stage := NewLLMStage("llm", bundle.Main, LLMConfig{
		Model:       snap.MainModel,
		Temperature: bundle.MainDef.Temperature,
		MaxSteps:    0,
	}, nil, logger)
	mainEnv := core.NewEnvelope(core.Message{ID: "m2", BotID: "bot-synth", Text: "hello", Source: "web", Channel: "web"})
	_, _ = stage.Process(bot.WithLLMSnapshot(context.Background(), snap), mainEnv)

	lurkEnv := core.NewEnvelope(core.Message{ID: "m3", BotID: "bot-synth", Text: "post", Source: "web", Channel: "web"})
	lurkEnv.Set(core.KVLurkMode, true)
	_, _ = stage.Process(bot.WithLLMSnapshot(context.Background(), snap), lurkEnv)

	hbEnv := core.NewEnvelope(core.Message{ID: "m4", BotID: "bot-synth", Text: "", InjectContext: "wake", Source: core.SourceHeartbeat, Channel: "web"})
	hbEnv.Set(core.KVHeartbeatMode, true)
	_, _ = stage.Process(bot.WithLLMSnapshot(context.Background(), snap), hbEnv)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 4 {
		t.Fatalf("requests %d, want main+l1+lurk+heartbeat", len(bodies))
	}
	for i, body := range bodies {
		if strings.Contains(body, `"temperature"`) {
			t.Fatalf("request %d sent temperature: %s", i, body)
		}
		if !strings.Contains(body, modelID) {
			t.Fatalf("request %d model: %s", i, body)
		}
	}
}
