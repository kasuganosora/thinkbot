package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestModelTemperaturePrecedence_WireRequestOmitAndZeroAndNoBotOverride(t *testing.T) {
	var (
		mu       sync.Mutex
		wireJSON map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		mu.Lock()
		wireJSON = parsed
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" || r.URL.Path == "/responses" {
			_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","model":"synth","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"synth","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	takeWire := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return wireJSON
	}

	store := config.NewStore(nil)
	store.SetTemporary("provider.synth-chat", `{
		"enabled": true,
		"clientType": "OpenAI Compatible",
		"baseUrl": "`+srv.URL+`/v1",
		"apiKey": "dummy-key",
		"models": [
			{"id": "model-omit", "temperature": -1, "topP": -1},
			{"id": "glm-5.3", "temperature": 0},
			{"id": "model-unset-custom", "temperature": 0},
			{"id": "model-explicit-low", "temperature": 0.25}
		]
	}`)
	store.SetTemporary("provider.synth-resp", `{
		"enabled": true,
		"clientType": "OpenAI Responses",
		"baseUrl": "`+srv.URL+`/v1",
		"apiKey": "dummy-key",
		"models": [
			{"id": "model-resp-omit", "temperature": -1, "topP": -1}
		]
	}`)

	builder := config.NewBuilder(store, zap.NewNop().Sugar())
	botLegacyTemp := 0.7

	runBoundCall := func(t *testing.T, modelID string) map[string]any {
		t.Helper()
		bundle, err := CreateLLMBundleFrom(builder, "bot-synth", modelID, "", "")
		if err != nil {
			t.Fatalf("CreateLLMBundleFrom(%q): %v", modelID, err)
		}
		slot := NewLLMSlot(SnapshotFromBundle(bundle))
		follow := newFollow(slot, "main")

		// Even if a caller/bot passes 0.7 on GenerateParams, followProvider binds ModelDef.Temperature.
		_, err = follow.DoGenerate(context.Background(), llm.GenerateParams{
			Model:       llm.ChatModel(modelID),
			Temperature: &botLegacyTemp,
			Messages:    []llm.Message{llm.UserMessage("ping")},
		})
		if err != nil {
			t.Fatalf("DoGenerate(%q): %v", modelID, err)
		}
		return takeWire()
	}

	// 1. Model temperature = -1 (Chat Completions): omitted from wire request; bot 0.7 cannot override.
	bodyOmitChat := runBoundCall(t, "model-omit")
	if _, exists := bodyOmitChat["temperature"]; exists {
		t.Fatalf("Chat Completions sent temperature=%v when model temperature=-1 (must omit)", bodyOmitChat["temperature"])
	}
	if _, exists := bodyOmitChat["top_p"]; exists {
		t.Fatalf("Chat Completions sent top_p=%v when model topP=-1 (must omit)", bodyOmitChat["top_p"])
	}

	// 2. Model temperature = -1 (Responses API): omitted from wire request; bot 0.7 cannot override.
	bodyOmitResp := runBoundCall(t, "model-resp-omit")
	if _, exists := bodyOmitResp["temperature"]; exists {
		t.Fatalf("Responses API sent temperature=%v when model temperature=-1 (must omit)", bodyOmitResp["temperature"])
	}
	if _, exists := bodyOmitResp["top_p"]; exists {
		t.Fatalf("Responses API sent top_p=%v when model topP=-1 (must omit)", bodyOmitResp["top_p"])
	}

	// 3. Model temperature = 0 ("unset / default later") on preset model glm-5.3:
	// resolves to preset 1.0 (NOT bot 0.7, and NOT omitted).
	bodyPresetZero := runBoundCall(t, "glm-5.3")
	if got, ok := bodyPresetZero["temperature"].(float64); !ok || got != 1.0 {
		t.Fatalf("glm-5.3 with model temperature=0 sent temperature=%v, want 1.0 (preset, not bot 0.7)", bodyPresetZero["temperature"])
	}

	// 4. Model temperature = 0 ("unset / default later") on custom model without preset:
	// resolves to model default 0.7 on wire (NOT omitted), even if caller passes a different legacy bot temp.
	otherBotTemp := 0.35
	bundleCustom, err := CreateLLMBundleFrom(builder, "bot-synth", "model-unset-custom", "", "")
	if err != nil {
		t.Fatal(err)
	}
	followCustom := newFollow(NewLLMSlot(SnapshotFromBundle(bundleCustom)), "main")
	if _, err := followCustom.DoGenerate(context.Background(), llm.GenerateParams{
		Model:       llm.ChatModel("model-unset-custom"),
		Temperature: &otherBotTemp,
		Messages:    []llm.Message{llm.UserMessage("ping")},
	}); err != nil {
		t.Fatal(err)
	}
	bodyCustomZero := takeWire()
	if got, ok := bodyCustomZero["temperature"].(float64); !ok || got != 0.7 {
		t.Fatalf("custom model temperature=0 sent temperature=%v, want 0.7 (not bot 0.35 and not omitted)", bodyCustomZero["temperature"])
	}

	// 5. Explicit 0.0 on ModelDef.Temperature is retained on wire as 0 (not omitted, not overridden by bot 0.7).
	explicitZero := 0.0
	rawClient, err := CreateProvider(bundleCustom.MainDef, config.DefaultLLMClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	zeroDef := bundleCustom.MainDef
	zeroDef.Temperature = &explicitZero
	followExplicitZero := newFollow(NewLLMSlot(&LLMSnapshot{
		Main:      rawClient,
		MainModel: modelWithTemperature(zeroDef),
		MainDef:   zeroDef,
	}), "main")
	if _, err := followExplicitZero.DoGenerate(context.Background(), llm.GenerateParams{
		Model:       llm.ChatModel(zeroDef.Model),
		Temperature: &botLegacyTemp,
		Messages:    []llm.Message{llm.UserMessage("ping")},
	}); err != nil {
		t.Fatal(err)
	}
	bodyExplicitZero := takeWire()
	if got, ok := bodyExplicitZero["temperature"].(float64); !ok || got != 0.0 {
		t.Fatalf("explicit ModelDef Temperature=0.0 sent temperature=%v, want 0 (retained, not omitted or overridden)", bodyExplicitZero["temperature"])
	}

	// 6. Explicit model temperature 0.25 wins over default bot 0.7.
	bodyLow := runBoundCall(t, "model-explicit-low")
	if got, ok := bodyLow["temperature"].(float64); !ok || got != 0.25 {
		t.Fatalf("model temperature=0.25 sent temperature=%v, want 0.25 (bot 0.7 must not override)", bodyLow["temperature"])
	}
}
