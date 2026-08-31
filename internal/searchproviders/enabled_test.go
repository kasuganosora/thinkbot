package searchproviders

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func muteLastResortDDG(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html><body>blocked</body></html>`)
	}))
	restore := OverrideDDGEndpoints(srv.URL, srv.URL+"/lite")
	t.Cleanup(func() {
		restore()
		srv.Close()
	})
}

func testStore(t *testing.T, providers []Provider) *Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "providers.json"))
	if err := store.Save(providers); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreEnabledListFileOrder(t *testing.T) {
	store := testStore(t, []Provider{
		{ID: "a", Type: TypeTavily, Name: "Tavily", Enabled: false, APIKey: "tvly"},
		{ID: "b", Type: TypeBrave, Name: "Brave", Enabled: true, APIKey: "brave-key"},
		{ID: "c", Type: TypeSerper, Name: "Serper", Enabled: true, APIKey: "serper-key"},
	})
	got, err := store.EnabledList()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("EnabledList=%+v", got)
	}
}

func TestSearchEnabledFirstSuccess(t *testing.T) {
	muteLastResortDDG(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{"results": []map[string]any{
				{"title": "Go", "url": "https://go.dev", "description": "lang"},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	store := testStore(t, []Provider{
		{ID: "br-first", Type: TypeBrave, Name: "Brave Live", Enabled: true, APIKey: "k", BaseURL: srv.URL, Timeout: 5},
		{ID: "tv-second", Type: TypeTavily, Name: "Tavily", Enabled: true, APIKey: "k", BaseURL: "http://127.0.0.1:1", Timeout: 1},
	})
	out, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if out.Fallback {
		t.Fatal("first success should not set fallback")
	}
	if len(out.Attempted) != 0 {
		t.Fatalf("attempted=%+v", out.Attempted)
	}
	if out.Provider.ID != "br-first" || len(out.Results) != 1 || out.Results[0].URL != "https://go.dev" {
		t.Fatalf("out=%+v", out)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestSearchEnabledEmptyThenNext(t *testing.T) {
	muteLastResortDDG(t)
	var hitEmpty, hitOK int
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitEmpty++
		_, _ = io.WriteString(w, `{"web":{"results":[]}}`)
	}))
	t.Cleanup(emptySrv.Close)
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOK++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"title": "T", "url": "https://t.example", "content": "c"}},
		})
	}))
	t.Cleanup(okSrv.Close)
	store := testStore(t, []Provider{
		{ID: "br-empty", Type: TypeBrave, Name: "Brave Empty", Enabled: true, APIKey: "k", BaseURL: emptySrv.URL, Timeout: 5},
		{ID: "tv-ok", Type: TypeTavily, Name: "Tavily OK", Enabled: true, APIKey: "k", BaseURL: okSrv.URL, Timeout: 5},
	})
	out, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Fallback || out.Provider.ID != "tv-ok" {
		t.Fatalf("fallback=%v provider=%+v", out.Fallback, out.Provider)
	}
	if len(out.Attempted) != 1 || out.Attempted[0].ID != "br-empty" || !strings.Contains(out.Attempted[0].Error, "no search results") {
		t.Fatalf("attempted=%+v", out.Attempted)
	}
	if hitEmpty != 1 || hitOK != 1 {
		t.Fatalf("hitEmpty=%d hitOK=%d", hitEmpty, hitOK)
	}
}

func TestSearchEnabled401ThenNext(t *testing.T) {
	muteLastResortDDG(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(bad.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{"results": []map[string]any{
				{"title": "Go", "url": "https://go.dev", "description": "d"},
			}},
		})
	}))
	t.Cleanup(ok.Close)
	store := testStore(t, []Provider{
		{ID: "tv-401", Type: TypeTavily, Name: "Tavily Bad", Enabled: true, APIKey: "bad", BaseURL: bad.URL, Timeout: 5},
		{ID: "br-ok", Type: TypeBrave, Name: "Brave", Enabled: true, APIKey: "k", BaseURL: ok.URL, Timeout: 5},
	})
	out, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Fallback || out.Provider.ID != "br-ok" {
		t.Fatalf("out=%+v", out)
	}
	if len(out.Attempted) != 1 || !strings.Contains(out.Attempted[0].Error, "HTTP 401") {
		t.Fatalf("attempted=%+v", out.Attempted)
	}
}

func TestSearchEnabled401CooldownSkips(t *testing.T) {
	muteLastResortDDG(t)
	var hitBad, hitOK int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitBad++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	t.Cleanup(bad.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOK++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"organic": []map[string]any{{"title": "S", "link": "https://s.example", "snippet": "c"}},
		})
	}))
	t.Cleanup(ok.Close)
	store := testStore(t, []Provider{
		{ID: "cool-bad", Type: TypeTavily, Name: "Bad Key", Enabled: true, APIKey: "bad", BaseURL: bad.URL, Timeout: 5},
		{ID: "cool-ok", Type: TypeSerper, Name: "Serper", Enabled: true, APIKey: "k", BaseURL: ok.URL, Timeout: 5},
	})
	if _, err := SearchEnabled(context.Background(), store, "golang", 5); err != nil {
		t.Fatal(err)
	}
	if hitBad != 1 || hitOK != 1 {
		t.Fatalf("after first hitBad=%d hitOK=%d", hitBad, hitOK)
	}
	out, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hitBad != 1 {
		t.Fatalf("401 provider should be skipped, hitBad=%d", hitBad)
	}
	if hitOK != 2 || !out.Fallback || out.Provider.ID != "cool-ok" {
		t.Fatalf("second out=%+v hitOK=%d", out, hitOK)
	}
	if len(out.Attempted) != 1 || !strings.Contains(out.Attempted[0].Error, "skipped") {
		t.Fatalf("attempted=%+v", out.Attempted)
	}
}

func TestSearchEnabledAllFailAggregated(t *testing.T) {
	muteLastResortDDG(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(bad.Close)
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"web":{"results":[]}}`)
	}))
	t.Cleanup(empty.Close)
	store := testStore(t, []Provider{
		{ID: "all-tv", Type: TypeTavily, Name: "Tavily", Enabled: true, APIKey: "bad", BaseURL: bad.URL, Timeout: 5},
		{ID: "all-br", Type: TypeBrave, Name: "Brave", Enabled: true, APIKey: "k", BaseURL: empty.URL, Timeout: 5},
	})
	_, err := SearchEnabled(context.Background(), store, "zzzz", 5)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "all search providers failed") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(msg, "tavily/Tavily") || !strings.Contains(msg, "HTTP 401") {
		t.Fatalf("missing tavily: %v", err)
	}
	if !strings.Contains(msg, "brave/Brave") || !strings.Contains(msg, "no search results") {
		t.Fatalf("missing brave: %v", err)
	}
	if strings.Contains(msg, "duckduckgo.com/?q=") {
		t.Fatalf("fake ddg links: %v", err)
	}
}

func TestSearchEnabledNoneEnabled(t *testing.T) {
	var ddgHits int
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ddgHits++
	}))
	t.Cleanup(OverrideDDGEndpoints(ddg.URL, ddg.URL+"/lite"))
	t.Cleanup(ddg.Close)
	store := testStore(t, []Provider{
		{ID: "off", Type: TypeBrave, Enabled: false, APIKey: "k"},
	})
	_, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err == nil || !strings.Contains(err.Error(), "no search provider is enabled") {
		t.Fatalf("err=%v", err)
	}
	if ddgHits != 0 {
		t.Fatalf("last-resort DDG must not run when none enabled, hits=%d", ddgHits)
	}
}

func TestSearchEnabledLastResortAfterEnabledFail(t *testing.T) {
	ddgHTML := ddgBodyHTML("https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc", "The Go docs", "Official documentation")
	var ddgHits int
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ddgHits++
		if r.Method != http.MethodPost {
			t.Errorf("ddg method=%s", r.Method)
		}
		if strings.Contains(r.URL.Host, InstantAnswerHost) || strings.Contains(r.URL.String(), "format=json") {
			t.Errorf("instant answer: %s", r.URL.String())
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "q=golang") || !strings.Contains(string(body), "l=wt-wt") {
			t.Errorf("form=%s", body)
		}
		_, _ = io.WriteString(w, ddgHTML)
	}))
	restore := OverrideDDGEndpoints(ddg.URL, ddg.URL+"/no-lite")
	t.Cleanup(func() {
		restore()
		ddg.Close()
	})

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(bad.Close)
	store := testStore(t, []Provider{
		{ID: "only-tv", Type: TypeTavily, Name: "Tavily", Enabled: true, APIKey: "bad", BaseURL: bad.URL, Timeout: 5},
	})
	out, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Fallback || out.Provider.Type != TypeDuckDuckGo || out.Provider.Name != "DuckDuckGo (ddgs html)" {
		t.Fatalf("provider=%+v fallback=%v", out.Provider, out.Fallback)
	}
	if ddgHits == 0 {
		t.Fatal("last-resort DDG was not called")
	}
	if len(out.Results) != 1 || out.Results[0].URL != "https://go.dev/doc" {
		t.Fatalf("results=%+v", out.Results)
	}
	if len(out.Attempted) != 1 || out.Attempted[0].ID != "only-tv" {
		t.Fatalf("attempted=%+v", out.Attempted)
	}
}

func TestSearchEnabledNoLastResortIfDDGEnabled(t *testing.T) {
	var lastResortHits int
	ddgHook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastResortHits++
		_, _ = io.WriteString(w, ddgBodyHTML("https://example.com/from-hook", "hook", "x"))
	}))
	restore := OverrideDDGEndpoints(ddgHook.URL, "")
	t.Cleanup(func() {
		restore()
		ddgHook.Close()
	})

	var enabledDDG int
	enabled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enabledDDG++
		_, _ = io.WriteString(w, `<html><body>blocked</body></html>`)
	}))
	t.Cleanup(enabled.Close)
	store := testStore(t, []Provider{
		{ID: "ddg-on", Type: TypeDuckDuckGo, Name: "DuckDuckGo", Enabled: true, BaseURL: enabled.URL, Timeout: 5},
	})
	_, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err == nil {
		t.Fatal("expected failure")
	}
	if enabledDDG == 0 {
		t.Fatal("enabled DDG not called")
	}
	if lastResortHits != 0 {
		t.Fatalf("must not retry DDG last-resort, hits=%d", lastResortHits)
	}
	if !strings.Contains(err.Error(), "duckduckgo") {
		t.Fatalf("err=%v", err)
	}
}

func TestSearchEnabledSaveClearsCircuit(t *testing.T) {
	muteLastResortDDG(t)
	var hitBad int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitBad++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(bad.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{"results": []map[string]any{
				{"title": "Go", "url": "https://go.dev", "description": "d"},
			}},
		})
	}))
	t.Cleanup(ok.Close)
	providers := []Provider{
		{ID: "save-bad", Type: TypeTavily, Name: "Bad", Enabled: true, APIKey: "bad", BaseURL: bad.URL, Timeout: 5},
		{ID: "save-ok", Type: TypeBrave, Name: "OK", Enabled: true, APIKey: "k", BaseURL: ok.URL, Timeout: 5},
	}
	store := testStore(t, providers)
	if _, err := SearchEnabled(context.Background(), store, "golang", 5); err != nil {
		t.Fatal(err)
	}
	if hitBad != 1 {
		t.Fatalf("hitBad=%d", hitBad)
	}
	if err := store.Save(providers); err != nil {
		t.Fatal(err)
	}
	if _, err := SearchEnabled(context.Background(), store, "golang", 5); err != nil {
		t.Fatal(err)
	}
	if hitBad != 2 {
		t.Fatalf("Save should clear circuit, hitBad=%d", hitBad)
	}
}

func ddgBodyHTML(href, title, snippet string) string {
	return `<div class="links_main result__body">` +
		`<h2 class="result__title"><a class="result__a" href="` + href + `">` + title + `</a></h2>` +
		`<a class="result__snippet" href="` + href + `">` + snippet + `</a>` +
		`</div>`
}

func TestParseDuckDuckGoBodyDivAndAds(t *testing.T) {
	html := ddgBodyHTML("https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc", "The Go docs", "Official documentation") +
		ddgBodyHTML("https://duckduckgo.com/y.js?ad_domain=spam.com", "Buy this", "ad") +
		ddgBodyHTML("https://duckduckgo.com/?q=golang", "Search golang", "dummy")
	got := parseDuckDuckGoHTML(html, 5)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].URL != "https://go.dev/doc" || got[0].Title != "The Go docs" {
		t.Fatalf("got=%+v", got)
	}
}

func TestDuckDuckGoLiteFallback(t *testing.T) {
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html><body>blocked</body></html>`)
	}))
	t.Cleanup(htmlSrv.Close)
	liteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("lite method=%s", r.Method)
		}
		_, _ = io.WriteString(w, `<table><tr><td><a class="result-link" href="https://pkg.go.dev">pkg.go.dev</a></td></tr><tr><td class="result-snippet">packages</td></tr></table>`)
	}))
	t.Cleanup(liteSrv.Close)
	restore := OverrideDDGEndpoints(htmlSrv.URL, liteSrv.URL)
	t.Cleanup(restore)
	results, err := Search(context.Background(), Provider{Type: TypeDuckDuckGo, Timeout: 5}, "golang", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].URL != "https://pkg.go.dev" {
		t.Fatalf("results=%+v", results)
	}
}

func TestDuckDuckGoHTTP202Retry(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = io.WriteString(w, ddgBodyHTML("https://pkg.go.dev", "pkg", "go packages"))
	}))
	t.Cleanup(srv.Close)
	restore := OverrideDDGEndpoints(srv.URL, srv.URL+"/no-lite")
	t.Cleanup(restore)
	results, err := Search(context.Background(), Provider{Type: TypeDuckDuckGo, Timeout: 5}, "golang", 3)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 3 {
		t.Fatalf("hits=%d want 3 (two 202 retries)", hits)
	}
	if len(results) != 1 || results[0].URL != "https://pkg.go.dev" {
		t.Fatalf("results=%+v", results)
	}
}

func TestDuckDuckGoNeverInstantAnswer(t *testing.T) {
	if strings.Contains(ddgHTMLEndpoint, InstantAnswerHost) || strings.Contains(defaultDDGHTML, InstantAnswerHost) {
		t.Fatal("html endpoint is instant answer")
	}
	if strings.Contains(ddgLiteEndpoint, InstantAnswerHost) || strings.Contains(defaultDDGLite, InstantAnswerHost) {
		t.Fatal("lite endpoint is instant answer")
	}
	if InstantAnswerHost != "api.duckduckgo.com" {
		t.Fatal("constant drifted")
	}
}

func TestParseDuckDuckGoIgnoresWrapperBodyClass(t *testing.T) {
	html := `
	<div class="serp__body">
		<div class="links_main result__body">
			<h2 class="result__title"><a class="result__a" href="https://a.example">A</a></h2>
			<a class="result__snippet">sa</a>
		</div>
		<div class="links_main result__body">
			<h2 class="result__title"><a class="result__a" href="https://b.example">B</a></h2>
			<a class="result__snippet">sb</a>
		</div>
	</div>`
	got := parseDuckDuckGoHTML(html, 5)
	if len(got) != 2 {
		t.Fatalf("wrapper class containing body must not collapse results, got %+v", got)
	}
	if got[0].URL != "https://a.example" || got[1].URL != "https://b.example" {
		t.Fatalf("got=%+v", got)
	}
}

func TestParseDuckDuckGoDropsYjsEvenWithUddg(t *testing.T) {
	html := ddgBodyHTML("https://duckduckgo.com/y.js?ad_domain=spam.com&uddg=https%3A%2F%2Fspam.example%2F", "Buy this", "ad") +
		ddgBodyHTML("https://good.example/doc", "Good", "ok")
	got := parseDuckDuckGoHTML(html, 5)
	if len(got) != 1 || got[0].URL != "https://good.example/doc" {
		t.Fatalf("y.js ads with uddg must be dropped, got %+v", got)
	}
}

func TestDuckDuckGoRefusesYjsRedirect(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "y.js") {
			followed = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/y.js?ad=1", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	restore := OverrideDDGEndpoints(srv.URL, srv.URL+"/no-lite")
	t.Cleanup(restore)
	_, err := Search(context.Background(), Provider{Type: TypeDuckDuckGo, Timeout: 5}, "golang", 3)
	if followed {
		t.Fatal("followed redirect into y.js")
	}
	if err == nil {
		t.Fatal("expected error when DDG redirects to y.js")
	}
}

func TestDuckDuckGoRefusesInstantAnswerRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+InstantAnswerHost+"/?q=golang&format=json", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	restore := OverrideDDGEndpoints(srv.URL, srv.URL+"/no-lite")
	t.Cleanup(restore)
	_, err := Search(context.Background(), Provider{Type: TypeDuckDuckGo, Timeout: 5}, "golang", 3)
	if err == nil {
		t.Fatal("expected error when DDG redirects to Instant Answer")
	}
	if !strings.Contains(err.Error(), InstantAnswerHost) && !strings.Contains(err.Error(), "Instant Answer") {
		t.Fatalf("err=%v", err)
	}
}

func TestSearchEnabled429RetryAfter(t *testing.T) {
	muteLastResortDDG(t)
	var hitRate, hitOK int
	rateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitRate++
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	t.Cleanup(rateSrv.Close)
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOK++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{"results": []map[string]any{
				{"title": "Go", "url": "https://go.dev", "description": "d"},
			}},
		})
	}))
	t.Cleanup(okSrv.Close)
	store := testStore(t, []Provider{
		{ID: "rate-tv", Type: TypeTavily, Name: "Tavily", Enabled: true, APIKey: "k", BaseURL: rateSrv.URL, Timeout: 5},
		{ID: "rate-ok", Type: TypeBrave, Name: "Brave", Enabled: true, APIKey: "k", BaseURL: okSrv.URL, Timeout: 5},
	})
	out, err := SearchEnabled(context.Background(), store, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hitRate != 1 || !out.Fallback || out.Provider.ID != "rate-ok" {
		t.Fatalf("first out=%+v hitRate=%d", out, hitRate)
	}
	out, err = SearchEnabled(context.Background(), store, "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hitRate != 1 {
		t.Fatalf("429 provider should be skipped, hitRate=%d", hitRate)
	}
	if hitOK != 2 || !out.Fallback || out.Provider.ID != "rate-ok" {
		t.Fatalf("second out=%+v hitOK=%d", out, hitOK)
	}
	if len(out.Attempted) != 1 || !strings.Contains(out.Attempted[0].Error, "skipped") {
		t.Fatalf("attempted=%+v", out.Attempted)
	}
}

func TestSearchEnabled500UnauthorizedBodyDoesNotAuthCircuit(t *testing.T) {
	muteLastResortDDG(t)
	var hit500, hitOK int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit500++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"unauthorized upstream"}`)
	}))
	t.Cleanup(bad.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOK++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{"results": []map[string]any{
				{"title": "Go", "url": "https://go.dev", "description": "d"},
			}},
		})
	}))
	t.Cleanup(ok.Close)
	store := testStore(t, []Provider{
		{ID: "five-bad", Type: TypeTavily, Name: "Tavily", Enabled: true, APIKey: "k", BaseURL: bad.URL, Timeout: 5},
		{ID: "five-ok", Type: TypeBrave, Name: "Brave", Enabled: true, APIKey: "k", BaseURL: ok.URL, Timeout: 5},
	})
	if _, err := SearchEnabled(context.Background(), store, "golang", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := SearchEnabled(context.Background(), store, "golang", 5); err != nil {
		t.Fatal(err)
	}
	if hit500 != 2 {
		t.Fatalf("HTTP 500 must not trip 401/403 circuit, hit500=%d", hit500)
	}
	if hitOK != 2 {
		t.Fatalf("hitOK=%d", hitOK)
	}
}

func TestClassifySearchErrorByStatusNotBody(t *testing.T) {
	err500 := newHTTPSearchError(http.StatusInternalServerError, []byte(`{"error":"unauthorized"}`), 0)
	auth, _, isRate := classifySearchError(err500)
	if auth || isRate {
		t.Fatalf("500 with unauthorized body classified as auth=%v rate=%v", auth, isRate)
	}
	err429 := newHTTPSearchError(http.StatusTooManyRequests, []byte(`{"error":"slow down"}`), 90*time.Second)
	auth, rate, isRate := classifySearchError(err429)
	if auth || !isRate || rate != 90*time.Second {
		t.Fatalf("429 classified auth=%v isRate=%v rate=%v", auth, isRate, rate)
	}
	err401 := newHTTPSearchError(http.StatusUnauthorized, []byte(`{"error":"nope"}`), 0)
	auth, _, isRate = classifySearchError(err401)
	if !auth || isRate {
		t.Fatalf("401 classified auth=%v isRate=%v", auth, isRate)
	}
}
