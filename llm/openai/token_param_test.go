package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

func officialHostname(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(DefaultBaseURL)
	if err != nil || u.Hostname() == "" {
		t.Fatalf("default base URL %q", DefaultBaseURL)
	}
	return u.Hostname()
}

func TestChatTokenField_HostAndStyle(t *testing.T) {
	host := officialHostname(t)
	official := DefaultBaseURL + "/v1"
	lookalike := "https://" + host + ".example.test/v1"
	cases := []struct {
		style string
		base  string
		want  string
	}{
		{style: "", base: official, want: "max_completion_tokens"},
		{style: "auto", base: official, want: "max_completion_tokens"},
		{style: "AUTO", base: "https://" + strings.ToUpper(host) + ":443/v1", want: "max_completion_tokens"},
		{style: "auto", base: "https://user:secret@" + host + "/v1", want: "max_completion_tokens"},
		{style: "", base: lookalike, want: "max_tokens"},
		{style: "auto", base: "https://evil.example/a/" + host, want: "max_tokens"},
		{style: "auto", base: "https://evil.example/v1?u=" + host, want: "max_tokens"},
		{style: "auto", base: "https://not" + host + "/v1", want: "max_tokens"},
		{style: "", base: "", want: "max_tokens"},
		{style: "max_tokens", base: official, want: "max_tokens"},
		{style: "max_completion_tokens", base: "http://127.0.0.1:9/v1", want: "max_completion_tokens"},
		{style: "nope", base: official, want: "max_completion_tokens"},
	}
	for _, tt := range cases {
		t.Run(tt.style+"|"+tt.base, func(t *testing.T) {
			if got := chatTokenField(TokenParamStyle(tt.style), tt.base); got != tt.want {
				t.Fatalf("field %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChatCompletionsTokenField_RequestJSON(t *testing.T) {
	host := officialHostname(t)
	official := DefaultBaseURL + "/v1"
	n := 32
	zero := 0
	cases := []struct {
		name   string
		base   string
		style  string
		max    *int
		want   string
		stream bool
	}{
		{name: "official auto", base: official, max: &n, want: "max_completion_tokens"},
		{name: "official auto stream", base: official, max: &n, want: "max_completion_tokens", stream: true},
		{name: "lookalike host stays legacy", base: "https://" + host + ".example.test/v1", max: &n, want: "max_tokens"},
		{name: "compatible auto", base: "", max: &n, want: "max_tokens"},
		{name: "compatible auto stream", base: "", max: &n, want: "max_tokens", stream: true},
		{name: "official forced legacy", base: official, style: "max_tokens", max: &n, want: "max_tokens"},
		{name: "compatible forced completion", base: "", style: "max_completion_tokens", max: &n, want: "max_completion_tokens"},
		{name: "compatible forced completion stream", base: "", style: "max_completion_tokens", max: &n, want: "max_completion_tokens", stream: true},
		{name: "unset", base: "", want: ""},
		{name: "unset stream", base: "", want: "", stream: true},
		{name: "zero", base: official, max: &zero, want: ""},
		{name: "official unset", base: official, want: ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotBody []byte
			var gotHost string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				gotBody = append([]byte(nil), b...)
				mu.Unlock()
				if strings.Contains(string(b), `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"model-sol-main\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
					_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"model-sol-main\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"c1","model":"model-sol-main","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			base := tt.base
			if base == "" {
				base = srv.URL + "/v1"
			}
			target, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				mu.Lock()
				gotHost = req.URL.Hostname()
				mu.Unlock()
				clone := req.Clone(req.Context())
				clone.URL.Scheme = target.Scheme
				clone.URL.Host = target.Host
				clone.Host = target.Host
				return (&http.Transport{Proxy: nil}).RoundTrip(clone)
			})
			opts := []Option{
				WithAPIKey("dummy-token-not-logged"),
				WithBaseURL(base),
				WithChatMode(),
				WithChatPath("/chat/completions"),
				WithTimeout(2 * time.Second),
				WithHTTPClient(&http.Client{Timeout: 2 * time.Second, Transport: transport}),
			}
			if tt.style != "" {
				opts = append(opts, WithTokenParamStyle(tt.style))
			}
			client := New(opts...)
			params := llm.GenerateParams{
				Model:     llm.ChatModel("model-sol-main"),
				Messages:  []llm.Message{llm.UserMessage("ping")},
				MaxTokens: tt.max,
			}
			if tt.stream {
				sr, err := client.DoStream(context.Background(), params)
				if err != nil {
					t.Fatal(err)
				}
				for range sr.Stream {
				}
			} else if _, err := client.DoGenerate(context.Background(), params); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			body := string(gotBody)
			seenHost := gotHost
			mu.Unlock()
			if body == "" {
				t.Fatal("request body was not captured")
			}
			assertTokenField(t, body, tt.want, 32)
			wantHost := host
			if tt.base == "" {
				wantHost = target.Hostname()
			} else if strings.Contains(tt.base, host+".example.test") {
				wantHost = host + ".example.test"
			}
			if !strings.EqualFold(seenHost, wantHost) {
				t.Fatalf("request host %q, want %q", seenHost, wantHost)
			}
		})
	}
}

func TestResponsesUsesOutputTokenField(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"model-sol-main\"}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"model-sol-main","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer srv.Close()

	client := New(
		WithAPIKey("dummy-token-not-logged"),
		WithBaseURL(srv.URL),
		WithTokenParamStyle("max_completion_tokens"),
		WithTimeout(2*time.Second),
	)
	n := 48
	params := llm.GenerateParams{
		Model:     llm.ChatModel("model-sol-main"),
		Messages:  []llm.Message{llm.UserMessage("ping")},
		MaxTokens: &n,
	}
	if _, err := client.DoGenerate(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	assertResponsesTokenField(t, body, true)

	zero := 0
	params.MaxTokens = &zero
	if _, err := client.DoGenerate(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	body = gotBody
	mu.Unlock()
	assertResponsesTokenField(t, body, false)

	params.MaxTokens = &n
	sr, err := client.DoStream(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	for range sr.Stream {
	}
	mu.Lock()
	body = gotBody
	mu.Unlock()
	assertResponsesTokenField(t, body, true)
	if !strings.Contains(body, `"stream":true`) {
		t.Fatalf("stream flag missing: %s", body)
	}
}

func assertTokenField(t *testing.T, body, field string, value float64) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("request json: %v\n%s", err, body)
	}
	_, hasLegacy := m["max_tokens"]
	_, hasCompletion := m["max_completion_tokens"]
	switch field {
	case "":
		if hasLegacy || hasCompletion {
			t.Fatalf("token field present: %s", body)
		}
	case "max_tokens":
		if hasCompletion || m["max_tokens"] != value {
			t.Fatalf("max_tokens=%v completion=%v body=%s", m["max_tokens"], hasCompletion, body)
		}
	case "max_completion_tokens":
		if hasLegacy || m["max_completion_tokens"] != value {
			t.Fatalf("max_completion_tokens=%v legacy=%v body=%s", m["max_completion_tokens"], hasLegacy, body)
		}
	default:
		t.Fatalf("unknown field %s", field)
	}
}

func assertResponsesTokenField(t *testing.T, body string, present bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("request json: %v\n%s", err, body)
	}
	if _, ok := m["max_tokens"]; ok {
		t.Fatalf("responses request included max_tokens: %s", body)
	}
	if _, ok := m["max_completion_tokens"]; ok {
		t.Fatalf("responses request included max_completion_tokens: %s", body)
	}
	got, ok := m["max_output_tokens"]
	if present {
		if !ok || got != float64(48) {
			t.Fatalf("max_output_tokens=%v body=%s", got, body)
		}
		return
	}
	if ok {
		t.Fatalf("max_output_tokens present: %s", body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
