package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestCreateProvider_TokenParamStyleReachesChatRequest(t *testing.T) {
	var mu sync.Mutex
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	store := config.NewStore(nil)
	store.SetTemporary("llm.max_retries", "0")
	store.SetTemporary("provider.synth-chat", `{"enabled":true,"clientType":"OpenAI Compatible","baseUrl":"`+srv.URL+`/v1","apiKey":"dummy-token-alpha-aaaa","tokenParamStyle":"max_completion_tokens","models":[{"id":"model-sol-main","maxTokens":128}]}`)
	b := config.NewBuilder(store, nil)
	bundle, err := CreateLLMBundleFrom(b, "bot-synth", "model-sol-main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	n := 64
	_, err = bundle.Main.DoGenerate(context.Background(), llm.GenerateParams{
		Model:     llm.ChatModel("model-sol-main"),
		Messages:  []llm.Message{llm.UserMessage("ping")},
		MaxTokens: &n,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := body
	mu.Unlock()
	if !strings.Contains(got, `"max_completion_tokens":64`) || strings.Contains(got, `"max_tokens"`) {
		t.Fatalf("request body %s", got)
	}
}
