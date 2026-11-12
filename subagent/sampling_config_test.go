package subagent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/openai"
)

func TestSubAgentUsesModelSampling(t *testing.T) {
	t.Run("omit", func(t *testing.T) {
		body := delegateOnce(t, llm.OmitSamplingParam, llm.OmitSamplingParam)
		if strings.Contains(body, `"temperature"`) || strings.Contains(body, `"top_p"`) {
			t.Fatalf("omit model sent sampling params")
		}
	})
	t.Run("configured", func(t *testing.T) {
		body := delegateOnce(t, 0.4, 0.8)
		if !strings.Contains(body, `"temperature":0.4`) || !strings.Contains(body, `"top_p":0.8`) {
			t.Fatalf("model sampling missing")
		}
	})
}

func delegateOnce(t *testing.T, temp, top float64) string {
	t.Helper()
	var mu sync.Mutex
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	client := openai.New(
		openai.WithAPIKey("dummy-token-not-logged"),
		openai.WithBaseURL(srv.URL+"/v1"),
		openai.WithChatMode(),
		openai.WithChatPath("/chat/completions"),
		openai.WithTimeout(2*time.Second),
	)
	model := llm.ChatModel("gpt-synth")
	model.Temperature = &temp
	snap := &bot.LLMSnapshot{
		Main:      client,
		MainModel: model,
		MainDef: config.ModelDef{
			Model:       "gpt-synth",
			Temperature: &temp,
			TopP:        &top,
			MaxTokens:   32,
		},
	}
	slot := bot.NewLLMSlot(snap)
	prov := bot.FollowBundle(slot, &bot.LLMBundle{Main: client, MainDef: snap.MainDef}).Main
	mgr := NewSubAgentManager(prov, "gpt-synth", WithMaxTokens(32))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := mgr.DelegateStream(ctx, "system", "ping", WithSkipTools(), WithStuckTimeout(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if body == "" {
		t.Fatal("no request")
	}
	return body
}
