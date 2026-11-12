package route

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

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/openai"
)

func TestL1OmitsTemperatureWhenModelUsesSentinel(t *testing.T) {
	body := runL1(t, &llm.Model{ID: "model-sol-main", Temperature: floatPtr(llm.OmitSamplingParam)})
	if strings.Contains(body, `"temperature"`) || strings.Contains(body, "0.0") {
		t.Fatalf("sentinel model sent temperature: %s", body)
	}
}

func TestL1StillSendsZeroWhenTemperatureIsConfigured(t *testing.T) {
	body := runL1(t, llm.ChatModel("model-sol-main"))
	if !strings.Contains(body, `"temperature":0,`) && !strings.Contains(body, `"temperature":0}`) {
		t.Fatalf("expected temperature 0, body %s", body)
	}
}

func runL1(t *testing.T, model *llm.Model) string {
	t.Helper()
	var mu sync.Mutex
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if body == "" {
			body = string(b)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"pipeline_open","arguments":"{\"intent\":\"chat\",\"model\":\"light\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()
	client := openai.New(
		openai.WithAPIKey("dummy-token-not-logged"),
		openai.WithBaseURL(srv.URL+"/v1"),
		openai.WithChatMode(),
		openai.WithChatPath("/chat/completions"),
		openai.WithTimeout(2*time.Second),
	)
	stage := &l1Stage{
		reg:      NewRegistry(Limits{}),
		provider: client,
		model:    model,
		logger:   zap.NewNop().Sugar(),
	}
	env := core.NewEnvelope(core.Message{BotID: "bot-synth", Channel: "c", Text: "hello", TraceID: "trace"})
	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if body == "" {
		t.Fatal("no request")
	}
	return body
}

func floatPtr(v float64) *float64 { return &v }
