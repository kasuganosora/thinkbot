package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestCreateProvider_GrokClientTypeUsesSharedSafeErrorParser(t *testing.T) {
	const secret = "sk-test-LEAK-9999-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"temperature","message":"DO_NOT_LEAK `+secret+`"}}`)
	}))
	defer srv.Close()

	store := config.NewStore(nil)
	store.SetTemporary("provider.synth-grok", `{"enabled":true,"clientType":"grok","baseUrl":"`+srv.URL+`","apiKey":"dummy-grok-key","models":[{"id":"grok-4.3"}]}`)
	builder := config.NewBuilder(store, zap.NewNop().Sugar())

	def, err := builder.ResolveLLMModel("grok-4.3")
	if err != nil {
		t.Fatalf("ResolveLLMModel(grok-4.3): %v", err)
	}
	if def.Provider != "grok" {
		t.Fatalf("Provider = %q, want grok", def.Provider)
	}

	llmCfg := config.DefaultLLMClientConfig()
	llmCfg.MaxRetries = 0
	llmCfg.ClientTimeoutSeconds = 2
	prov, err := CreateProvider(def, llmCfg)
	if err != nil {
		t.Fatalf("CreateProvider(grok): %v", err)
	}

	_, callErr := prov.DoGenerate(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel(def.Model),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	le, ok := llm.AsLLMError(callErr)
	if !ok {
		t.Fatalf("expected *llm.LLMError, got %T (%v)", callErr, callErr)
	}
	if le.ProviderName != "grok" || le.Reason != llm.ErrorReasonInvalidRequest {
		t.Fatalf("unexpected LLMError: %+v", le)
	}
	if le.SafeDetail != "code=unsupported_parameter param=temperature type=invalid_request_error" {
		t.Fatalf("SafeDetail = %q", le.SafeDetail)
	}
	for _, s := range []string{le.Message, le.SafeDetail, le.Error(), llm.SafeErrorText(le), llm.UserFailureText(le)} {
		if strings.Contains(s, "DO_NOT_LEAK") || strings.Contains(s, secret) || strings.Contains(s, "LEAK") {
			t.Fatalf("grok factory path leaked raw error content: %q", s)
		}
	}
}
