package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestProtocolSelectionHitsDistinctEndpoints(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path.Store(r.URL.Path + " " + string(body))
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "responses") {
			_, _ = io.WriteString(w, `{"id":"resp","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"cmpl","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	store := config.NewStore(nil)
	store.SetTemporary("llm.max_retries", "0")
	b := config.NewBuilder(store, nil)
	params := llm.GenerateParams{Messages: []llm.Message{llm.UserMessage("ping")}}

	store.SetTemporary("provider.synth-chat", `{"enabled":true,"clientType":"OpenAI Compatible","baseUrl":"`+srv.URL+`/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"}]}`)
	chat, err := CreateLLMBundleFrom(b, "bot-synth", "model-sol-main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	params.Model = llm.ChatModel("model-sol-main")
	if _, err := chat.Main.DoGenerate(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	got, _ := path.Load().(string)
	if !strings.Contains(got, "/v1/chat/completions") || !strings.Contains(got, `"messages"`) {
		t.Fatalf("compatible request = %s", got)
	}

	store.SetTemporary("provider.synth-resp", `{"enabled":true,"clientType":"OpenAI Responses","baseUrl":"`+srv.URL+`/v1","apiKey":"dummy-token-beta-bbbb","models":[{"id":"model-terra-light"}]}`)
	// The chat model id is only on the compatible provider. Use a distinct id.
	respBundle, err := CreateLLMBundleFrom(b, "bot-synth", "model-terra-light", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if respBundle.MainDef.APIStyle != "responses" {
		t.Fatalf("style %s", respBundle.MainDef.APIStyle)
	}
	params.Model = llm.ChatModel("model-terra-light")
	if _, err := respBundle.Main.DoGenerate(context.Background(), params); err != nil {
		t.Fatalf("responses generate: %v", err)
	}
	got, _ = path.Load().(string)
	if !strings.Contains(got, "/responses") || strings.Contains(got, "/v1/v1/") || !strings.Contains(got, `"input"`) {
		t.Fatalf("responses request = %s", got)
	}
}
