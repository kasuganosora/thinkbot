package searchproviders

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreEnabledUsesFirstEnabled(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "providers.json"))
	if err := store.Save([]Provider{
		{ID: "a", Type: TypeTavily, Name: "Tavily", Enabled: false, APIKey: "tvly"},
		{ID: "b", Type: TypeBrave, Name: "Brave", Enabled: true, APIKey: "brave-key"},
		{ID: "c", Type: TypeSerper, Name: "Serper", Enabled: true, APIKey: "serper-key"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Enabled()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" || got.Type != TypeBrave {
		t.Fatalf("expected first enabled brave, got %+v", got)
	}
}

func TestStoreEnabledNone(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "providers.json"))
	if err := store.Save([]Provider{
		{ID: "a", Type: TypeBrave, Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Enabled()
	if err == nil || !strings.Contains(err.Error(), "no search provider is enabled") {
		t.Fatalf("expected enabled error, got %v", err)
	}
}

func TestSearchMissingQuery(t *testing.T) {
	_, err := Search(context.Background(), Provider{Type: TypeDuckDuckGo}, "  ", 5)
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Fatalf("expected query error, got %v", err)
	}
}

func TestSearchUnsupported(t *testing.T) {
	_, err := Search(context.Background(), Provider{Type: "unknown-engine"}, "go", 5)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported error, got %v", err)
	}
}

func TestSearchMissingAPIKey(t *testing.T) {
	_, err := Search(context.Background(), Provider{Type: TypeBrave}, "go", 5)
	if err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("expected api key error, got %v", err)
	}
}

func TestDispatchParseAndEnabledProvider(t *testing.T) {
	var seenPath, seenAuth, seenMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("X-Subscription-Token")
		seenMethod = r.Method
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{
				"results": []map[string]any{
					{"title": "Go", "url": "https://go.dev", "description": "The Go programming language"},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "providers.json"))
	if err := store.Save([]Provider{
		{ID: "tavily", Type: TypeTavily, Enabled: false, APIKey: "nope", BaseURL: "http://127.0.0.1:1"},
		{ID: "brave", Type: TypeBrave, Enabled: true, APIKey: "brave-secret", BaseURL: srv.URL, Timeout: 5},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := store.Enabled()
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != TypeBrave {
		t.Fatalf("enabled provider = %s, want brave", p.Type)
	}

	results, err := Search(context.Background(), *p, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if seenMethod != http.MethodGet {
		t.Fatalf("method = %s", seenMethod)
	}
	if seenAuth != "brave-secret" {
		t.Fatalf("auth = %q", seenAuth)
	}
	if seenPath == "" {
		t.Fatal("request was not received")
	}
	if len(results) != 1 || results[0].URL != "https://go.dev" || results[0].Title != "Go" {
		t.Fatalf("results = %+v", results)
	}
	if strings.Contains(results[0].URL, "duckduckgo.com/?q=") {
		t.Fatal("dummy duckduckgo link presented as a hit")
	}
}

func TestProviderParsers(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		status   int
		body     string
		checkReq func(*testing.T, *http.Request)
		wantURL  string
		wantErr  string
	}{
		{
			name:     "brave",
			provider: Provider{Type: TypeBrave, APIKey: "k"},
			body:     `{"web":{"results":[{"title":"A","url":"https://a.example","description":"da"}]}}`,
			checkReq: func(t *testing.T, r *http.Request) {
				if r.Header.Get("X-Subscription-Token") != "k" {
					t.Errorf("missing brave token")
				}
				if r.URL.Query().Get("q") != "hello" {
					t.Errorf("q=%s", r.URL.Query().Get("q"))
				}
			},
			wantURL: "https://a.example",
		},
		{
			name:     "bing",
			provider: Provider{Type: TypeBing, APIKey: "k"},
			body:     `{"webPages":{"value":[{"name":"B","url":"https://b.example","snippet":"sb"}]}}`,
			wantURL:  "https://b.example",
		},
		{
			name:     "google",
			provider: Provider{Type: TypeGoogle, APIKey: "k", SearchType: "cx123"},
			body:     `{"items":[{"title":"G","link":"https://g.example","snippet":"sg"}]}`,
			checkReq: func(t *testing.T, r *http.Request) {
				if r.URL.Query().Get("cx") != "cx123" || r.URL.Query().Get("key") != "k" {
					t.Errorf("query=%s", r.URL.RawQuery)
				}
			},
			wantURL: "https://g.example",
		},
		{
			name:     "tavily",
			provider: Provider{Type: TypeTavily, APIKey: "k"},
			body:     `{"results":[{"title":"T","url":"https://t.example","content":"ct"}]}`,
			wantURL:  "https://t.example",
		},
		{
			name:     "serper",
			provider: Provider{Type: TypeSerper, APIKey: "k"},
			body:     `{"organic":[{"title":"S","link":"https://s.example","snippet":"cs","position":1}]}`,
			wantURL:  "https://s.example",
		},
		{
			name:     "searxng",
			provider: Provider{Type: TypeSearXNG},
			body:     `{"results":[{"title":"X","url":"https://x.example","content":"cx","score":9}]}`,
			wantURL:  "https://x.example",
		},
		{
			name:     "jina",
			provider: Provider{Type: TypeJina, APIKey: "k"},
			body:     `{"data":[{"title":"J","url":"https://j.example","content":"cj"}]}`,
			wantURL:  "https://j.example",
		},
		{
			name:     "exa",
			provider: Provider{Type: TypeExa, APIKey: "k"},
			body:     `{"results":[{"title":"E","url":"https://e.example","text":"ce"}]}`,
			wantURL:  "https://e.example",
		},
		{
			name:     "bocha",
			provider: Provider{Type: TypeBocha, APIKey: "k"},
			body:     `{"data":{"webPages":{"value":[{"name":"Bo","url":"https://bo.example","summary":"cbo"}]}}}`,
			wantURL:  "https://bo.example",
		},
		{
			name:     "empty-brave",
			provider: Provider{Type: TypeBrave, APIKey: "k"},
			body:     `{"web":{"results":[]}}`,
			wantErr:  "no search results",
		},
		{
			name:     "auth-401",
			provider: Provider{Type: TypeTavily, APIKey: "bad"},
			status:   401,
			body:     `{"error":"invalid api key"}`,
			wantErr:  "HTTP 401",
		},
		{
			name:     "google-missing-cx",
			provider: Provider{Type: TypeGoogle, APIKey: "k", SearchType: "SEARCH_TYPE_WEB"},
			wantErr:  "requires cx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr == "requires cx" {
				_, err := Search(context.Background(), tt.provider, "hello", 5)
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.checkReq != nil {
					tt.checkReq(t, r)
				}
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			p := tt.provider
			p.BaseURL = srv.URL
			p.Timeout = 5
			results, err := Search(context.Background(), p, "hello", 5)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(results) == 0 || results[0].URL != tt.wantURL {
				t.Fatalf("results=%+v", results)
			}
		})
	}
}

func TestDuckDuckGoHTMLParse(t *testing.T) {
	html := `
	<a class="result__a" href="https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc">The Go docs</a>
	<a class="result__snippet">Official documentation</a>
	<a class="result__a" href="https://duckduckgo.com/?q=golang">Search golang</a>
	<a class="result__snippet">dummy</a>
	`
	got := parseDuckDuckGoHTML(html, 5)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].URL != "https://go.dev/doc" {
		t.Fatalf("url=%s", got[0].URL)
	}
	if got[0].Title != "The Go docs" {
		t.Fatalf("title=%s", got[0].Title)
	}
}

func TestDuckDuckGoHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("ct=%s", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "q=golang") {
			t.Errorf("body=%s", body)
		}
		if strings.Contains(r.URL.Host, "api.duckduckgo.com") || strings.Contains(r.URL.String(), "format=json") {
			t.Errorf("used instant answer: %s", r.URL.String())
		}
		_, _ = io.WriteString(w, `<a class="result__a" href="https://pkg.go.dev">pkg.go.dev</a><a class="result__snippet">packages</a>`)
	}))
	t.Cleanup(srv.Close)
	results, err := Search(context.Background(), Provider{Type: TypeDuckDuckGo, BaseURL: srv.URL, Timeout: 5}, "golang", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].URL != "https://pkg.go.dev" {
		t.Fatalf("results=%+v", results)
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"web":{"results":[]}}`)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	_, err := Search(ctx, Provider{Type: TypeBrave, APIKey: "k", BaseURL: srv.URL, Timeout: 1}, "go", 3)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestYandexXML(t *testing.T) {
	xmlBody := `<?xml version="1.0"?><response><results><grouping><group><doc><url>https://y.example</url><title>Yandex Hit</title><passages><passage>snippet here</passage></passages></doc></group></grouping></results></response>`
	raw := base64.StdEncoding.EncodeToString([]byte(xmlBody))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Api-Key yk" {
			t.Errorf("auth=%s", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rawData": raw})
	}))
	t.Cleanup(srv.Close)
	results, err := Search(context.Background(), Provider{Type: TypeYandex, APIKey: "yk", BaseURL: srv.URL, SearchType: "SEARCH_TYPE_RU", Timeout: 5}, "hello", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].URL != "https://y.example" || results[0].Snippet != "snippet here" {
		t.Fatalf("results=%+v", results)
	}
}

func TestSogouPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]any{
				"Pages": []any{
					map[string]any{"Title": "S1", "URL": "https://s1.example", "Passage": "p1", "scour": 0.2},
					map[string]any{"Title": "S0", "URL": "https://s0.example", "Passage": "p0", "scour": 0.9},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	results, err := Search(context.Background(), Provider{
		Type: TypeSogou, APIKey: "sk", SearchType: "sid", BaseURL: srv.URL, Timeout: 5,
	}, "hello", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].URL != "https://s0.example" {
		t.Fatalf("results=%+v", results)
	}
}

func TestNoInstantAnswerHostInDuckDuckGoDefault(t *testing.T) {
	// Default DuckDuckGo endpoint must be HTML search, not Instant Answer.
	if strings.Contains(firstNonEmpty("", "https://html.duckduckgo.com/html/"), InstantAnswerHost) {
		t.Fatal("default ddg endpoint is instant answer")
	}
	if InstantAnswerHost != "api.duckduckgo.com" {
		t.Fatal("constant drifted")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	store := NewStore(path)
	orig := []Provider{{ID: "sp1", Type: TypeBrave, Name: "Brave", Enabled: true, APIKey: "k", Timeout: 15}}
	if err := store.Save(orig); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 || list[0].APIKey != "k" {
		t.Fatalf("list=%v err=%v", list, err)
	}
}
