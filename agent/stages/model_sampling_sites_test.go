package stages

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/openai"
)

func TestConfiguredTemperatureSurvivesLurkAndHeartbeat(t *testing.T) {
	const modelID = "gpt-synth"
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
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"remember\":false}"}]}]
		}`)
	}))
	defer srv.Close()

	store := config.NewStore(nil)
	store.SetTemporary("llm.max_retries", "0")
	store.SetTemporary("provider.synth", `{"enabled":true,"clientType":"OpenAI Responses","baseUrl":"`+srv.URL+`/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"`+modelID+`","temperature":0.4,"topP":0.8,"maxTokens":64}]}`)
	bundle, err := bot.CreateLLMBundleFrom(config.NewBuilder(store, nil), "bot-synth", modelID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	snap := bot.SnapshotFromBundle(bundle)
	logger := zap.NewNop().Sugar()
	stage := NewLLMStage("llm", bundle.Main, LLMConfig{
		Model:       snap.MainModel,
		Temperature: bundle.MainDef.Temperature,
		TopP:        bundle.MainDef.TopP,
		MaxSteps:    0,
	}, nil, logger)

	lurkEnv := core.NewEnvelope(core.Message{ID: "m3", BotID: "bot-synth", Text: "post", Source: "web", Channel: "web"})
	lurkEnv.Set(core.KVLurkMode, true)
	_, _ = stage.Process(bot.WithLLMSnapshot(context.Background(), snap), lurkEnv)

	hbEnv := core.NewEnvelope(core.Message{ID: "m4", BotID: "bot-synth", Text: "", InjectContext: "wake", Source: core.SourceHeartbeat, Channel: "web"})
	hbEnv.Set(core.KVHeartbeatMode, true)
	_, _ = stage.Process(bot.WithLLMSnapshot(context.Background(), snap), hbEnv)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("requests %d, want lurk+heartbeat", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, `"temperature":0.4`) {
			t.Fatalf("request %d lost the model temperature", i)
		}
		if strings.Contains(body, `"temperature":0.1`) || strings.Contains(body, `"temperature":0,`) || strings.Contains(body, `"temperature":0}`) {
			t.Fatalf("request %d replaced the model temperature", i)
		}
	}
}

func TestCompactionAndMemoryUseModelSampling(t *testing.T) {
	t.Run("omit", func(t *testing.T) {
		for _, body := range runSummaryAndMemory(t, llm.OmitSamplingParam) {
			if strings.Contains(body, `"temperature"`) || strings.Contains(body, `"top_p"`) {
				t.Fatalf("omit model sent sampling params")
			}
		}
	})
	t.Run("configured", func(t *testing.T) {
		bodies := runSummaryAndMemory(t, 0.4)
		for _, body := range bodies {
			if !strings.Contains(body, `"temperature":0.4`) {
				t.Fatalf("model temperature missing")
			}
		}
	})
}

func runSummaryAndMemory(t *testing.T, temp float64) []string {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"[]"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	client := openai.New(
		openai.WithAPIKey("dummy-token-not-logged"),
		openai.WithBaseURL(srv.URL+"/v1"),
		openai.WithChatMode(),
		openai.WithChatPath("/chat/completions"),
		openai.WithTimeout(2*time.Second),
	)
	top := float64(llm.OmitSamplingParam)
	if temp != llm.OmitSamplingParam {
		top = 0.8
	}
	model := llm.ChatModel("gpt-synth")
	model.Temperature = &temp
	snap := &bot.LLMSnapshot{
		Main:      client,
		MainModel: model,
		MainDef: config.ModelDef{
			Model:       "gpt-synth",
			Temperature: &temp,
			TopP:        &top,
			MaxTokens:   64,
		},
	}
	slot := bot.NewLLMSlot(snap)
	prov := bot.FollowBundle(slot, &bot.LLMBundle{Main: client, MainDef: snap.MainDef}).Main
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := llm.NewCompactor(llm.DefaultCompactionConfig())
	if _, err := c.SummarizeHead(ctx, prov, "gpt-synth", []llm.Message{llm.UserMessage("hi"), llm.AssistantMessage("yo")}, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.ClusterMerge(ctx, prov, model, "merge", []memory.ClusterInput{
		{ID: "1", Content: "alpha"},
		{ID: "2", Content: "beta"},
	}, 64, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("calls %d, want summary+memory", len(bodies))
	}
	return append([]string(nil), bodies...)
}
