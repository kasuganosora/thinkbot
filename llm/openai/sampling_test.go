package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestSamplingSentinelOmittedOnChatAndResponses(t *testing.T) {
	omit := float64(llm.OmitSamplingParam)
	keep := 0.7
	explicit := 0.2
	top := 0.9
	cases := []struct {
		name    string
		chat    bool
		stream  bool
		temp    *float64
		top     *float64
		wantT   bool
		wantTop bool
		tVal    string
		topVal  string
	}{
		{name: "chat omit", chat: true, temp: &omit, top: &omit},
		{name: "chat omit stream", chat: true, stream: true, temp: &omit, top: &omit},
		{name: "responses omit", temp: &omit, top: &omit},
		{name: "responses omit stream", stream: true, temp: &omit, top: &omit},
		{name: "chat default value", chat: true, temp: &keep, wantT: true, tVal: "0.7"},
		{name: "chat explicit", chat: true, temp: &explicit, top: &top, wantT: true, wantTop: true, tVal: "0.2", topVal: "0.9"},
		{name: "responses explicit stream", stream: true, temp: &explicit, top: &top, wantT: true, wantTop: true, tVal: "0.2", topVal: "0.9"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			body := captureGenerate(t, tt.chat, tt.stream, llm.GenerateParams{
				Model:       llm.ChatModel("model-sol-main"),
				Messages:    []llm.Message{llm.UserMessage("ping")},
				Temperature: tt.temp,
				TopP:        tt.top,
			})
			assertSampling(t, body, tt.wantT, tt.wantTop, tt.tVal, tt.topVal)
		})
	}
}

func assertSampling(t *testing.T, body string, wantT, wantTop bool, tVal, topVal string) {
	t.Helper()
	if wantT {
		if !strings.Contains(body, `"temperature":`+tVal) {
			t.Fatalf("temperature %s missing: %s", tVal, body)
		}
	} else if strings.Contains(body, `"temperature"`) {
		t.Fatalf("temperature sent: %s", body)
	}
	if wantTop {
		if !strings.Contains(body, `"top_p":`+topVal) {
			t.Fatalf("top_p %s missing: %s", topVal, body)
		}
	} else if strings.Contains(body, `"top_p"`) {
		t.Fatalf("top_p sent: %s", body)
	}
}

func captureGenerate(t *testing.T, chat, stream bool, params llm.GenerateParams) string {
	t.Helper()
	var mu sync.Mutex
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		if chat {
			if strings.Contains(string(b), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			return
		}
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"model-sol-main\"}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"model-sol-main","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer srv.Close()
	opts := []Option{WithAPIKey("dummy-token-not-logged"), WithBaseURL(srv.URL), WithTimeout(2 * time.Second)}
	if chat {
		opts = append(opts, WithChatMode(), WithChatPath("/chat/completions"))
	}
	client := New(opts...)
	ctx := context.Background()
	if stream {
		sr, err := client.DoStream(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		for range sr.Stream {
		}
	} else if _, err := client.DoGenerate(ctx, params); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if body == "" {
		t.Fatal("no request captured")
	}
	return body
}
