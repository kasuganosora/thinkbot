package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/internal/searchproviders"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestMain(m *testing.M) {
	// 默认把 last-resort DDG 指到空页，避免 web_search 测试打公网。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html><body>blocked</body></html>`)
	}))
	restore := searchproviders.OverrideDDGEndpoints(srv.URL, srv.URL+"/lite")
	code := m.Run()
	restore()
	srv.Close()
	os.Exit(code)
}

func TestWebSearchUsesEnabledProvider(t *testing.T) {
	var hitTavily, hitBrave int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tavily") {
			hitTavily++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"title": "nope", "url": "https://nope.example", "content": "x"}},
			})
			return
		}
		hitBrave++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{
				"results": []map[string]any{
					{"title": "Go", "url": "https://go.dev", "description": "The Go language"},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	store := searchproviders.NewStore(filepath.Join(t.TempDir(), "providers.json"))
	if err := store.Save([]searchproviders.Provider{
		{ID: "tv", Type: searchproviders.TypeTavily, Name: "Tavily", Enabled: false, APIKey: "tavily-key", BaseURL: srv.URL + "/tavily", Timeout: 5},
		{ID: "br", Type: searchproviders.TypeBrave, Name: "Brave Live", Enabled: true, APIKey: "brave-key", BaseURL: srv.URL, Timeout: 5},
	}); err != nil {
		t.Fatal(err)
	}

	def := searchToolDef(SearchConfig{Store: store, MaxResults: 5})
	out := execTool(t, def.Tool, map[string]any{"query": "golang"})
	m, _ := out.(map[string]any)
	if m["engine"] != searchproviders.TypeBrave {
		t.Fatalf("engine=%v", m["engine"])
	}
	if m["provider"] != "Brave Live" {
		t.Fatalf("provider=%v", m["provider"])
	}
	if hitBrave != 1 || hitTavily != 0 {
		t.Fatalf("hitBrave=%d hitTavily=%d", hitBrave, hitTavily)
	}
	if m["fallback"] == true {
		t.Fatal("first enabled success should not fallback")
	}
	if _, ok := m["attempted"]; ok {
		t.Fatalf("attempted should be omitted: %v", m["attempted"])
	}
	raw, _ := json.Marshal(m["results"])
	if strings.Contains(string(raw), "duckduckgo.com/?q=") {
		t.Fatalf("dummy ddg link in results: %s", raw)
	}
	if !strings.Contains(string(raw), "https://go.dev") {
		t.Fatalf("missing real hit: %s", raw)
	}
}

func TestWebSearchNoEnabledProvider(t *testing.T) {
	store := searchproviders.NewStore(filepath.Join(t.TempDir(), "providers.json"))
	_ = store.Save([]searchproviders.Provider{
		{ID: "br", Type: searchproviders.TypeBrave, Enabled: false, APIKey: "k"},
	})
	def := searchToolDef(SearchConfig{Store: store})
	_, err := def.Tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"query": "golang"})
	if err == nil || !strings.Contains(err.Error(), "no search provider is enabled") {
		t.Fatalf("err=%v", err)
	}
}

func TestWebSearchEmptyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"web":{"results":[]}}`)
	}))
	t.Cleanup(srv.Close)
	store := searchproviders.NewStore(filepath.Join(t.TempDir(), "providers.json"))
	_ = store.Save([]searchproviders.Provider{
		{ID: "br", Type: searchproviders.TypeBrave, Enabled: true, APIKey: "k", BaseURL: srv.URL, Timeout: 5},
	})
	def := searchToolDef(SearchConfig{Store: store})
	_, err := def.Tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"query": "zzzz-no-results"})
	if err == nil || !strings.Contains(err.Error(), "no search results") {
		t.Fatalf("err=%v", err)
	}
}

func TestWebSearchAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(srv.Close)
	store := searchproviders.NewStore(filepath.Join(t.TempDir(), "providers.json"))
	_ = store.Save([]searchproviders.Provider{
		{ID: "tv", Type: searchproviders.TypeTavily, Enabled: true, APIKey: "bad", BaseURL: srv.URL, Timeout: 5},
	})
	def := searchToolDef(SearchConfig{Store: store})
	_, err := def.Tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"query": "golang"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err=%v", err)
	}
}
func TestWebSearchFallbackSecondProvider(t *testing.T) {
	var hitTavily, hitBrave int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tavily") {
			hitTavily++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
			return
		}
		hitBrave++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{
				"results": []map[string]any{
					{"title": "Go", "url": "https://go.dev", "description": "The Go language"},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	store := searchproviders.NewStore(filepath.Join(t.TempDir(), "providers.json"))
	if err := store.Save([]searchproviders.Provider{
		{ID: "tv-fb", Type: searchproviders.TypeTavily, Name: "Tavily", Enabled: true, APIKey: "bad", BaseURL: srv.URL + "/tavily", Timeout: 5},
		{ID: "br-fb", Type: searchproviders.TypeBrave, Name: "Brave Live", Enabled: true, APIKey: "brave-key", BaseURL: srv.URL, Timeout: 5},
	}); err != nil {
		t.Fatal(err)
	}

	def := searchToolDef(SearchConfig{Store: store, MaxResults: 5})
	out := execTool(t, def.Tool, map[string]any{"query": "golang"})
	m, _ := out.(map[string]any)
	if m["fallback"] != true {
		t.Fatalf("fallback=%v", m["fallback"])
	}
	if m["engine"] != searchproviders.TypeBrave || m["provider"] != "Brave Live" {
		t.Fatalf("engine=%v provider=%v", m["engine"], m["provider"])
	}
	if hitTavily != 1 || hitBrave != 1 {
		t.Fatalf("hitTavily=%d hitBrave=%d", hitTavily, hitBrave)
	}
	attempted, ok := m["attempted"].([]searchproviders.Attempt)
	if !ok || len(attempted) != 1 || attempted[0].ID != "tv-fb" {
		t.Fatalf("attempted=%#v", m["attempted"])
	}
	raw, _ := json.Marshal(m["results"])
	if strings.Contains(string(raw), "duckduckgo.com/?q=") {
		t.Fatalf("dummy ddg link: %s", raw)
	}
}

func TestWebSearchLastResortDDG(t *testing.T) {
	html := `<div class="links_main result__body"><h2 class="result__title"><a class="result__a" href="https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev">Go</a></h2><a class="result__snippet">lang</a></div>`
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		_, _ = io.WriteString(w, html)
	}))
	t.Cleanup(ddg.Close)
	restore := searchproviders.OverrideDDGEndpoints(ddg.URL, ddg.URL+"/lite")
	t.Cleanup(restore)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(bad.Close)
	store := searchproviders.NewStore(filepath.Join(t.TempDir(), "providers.json"))
	_ = store.Save([]searchproviders.Provider{
		{ID: "tv-lr", Type: searchproviders.TypeTavily, Name: "Tavily", Enabled: true, APIKey: "bad", BaseURL: bad.URL, Timeout: 5},
	})
	def := searchToolDef(SearchConfig{Store: store})
	out := execTool(t, def.Tool, map[string]any{"query": "golang"})
	m, _ := out.(map[string]any)
	if m["fallback"] != true || m["engine"] != searchproviders.TypeDuckDuckGo {
		t.Fatalf("got=%v", m)
	}
	if m["provider"] != "DuckDuckGo (ddgs html)" {
		t.Fatalf("provider=%v", m["provider"])
	}
}
