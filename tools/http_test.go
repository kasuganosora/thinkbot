package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestNormalizeFetchURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// 2026-10-06 事故：原始中文查询串，Bing 回 400 空 body。
		{"https://www.bing.com/search?q=Re0+老吴+梗+rezero", "https://www.bing.com/search?q=Re0+%E8%80%81%E5%90%B4+%E6%A2%97+rezero"},
		// 已编码：原样保留，不二次编码。
		{"https://www.bing.com/search?q=Re0+%E8%80%81%E5%90%B4+%E6%A2%97", "https://www.bing.com/search?q=Re0+%E8%80%81%E5%90%B4+%E6%A2%97"},
		{"https://example.com/a%20b/%e4%b8%ad?x=%2F%3D", "https://example.com/a%20b/%e4%b8%ad?x=%2F%3D"},
		// 半编码半原始：只转义原始部分。
		{"https://example.com/search?q=%E8%80%81吴", "https://example.com/search?q=%E8%80%81%E5%90%B4"},
		// 中文路径、空格、落单的 %。
		{"https://zh.wikipedia.org/wiki/从零开始的异世界生活", "https://zh.wikipedia.org/wiki/%E4%BB%8E%E9%9B%B6%E5%BC%80%E5%A7%8B%E7%9A%84%E5%BC%82%E4%B8%96%E7%95%8C%E7%94%9F%E6%B4%BB"},
		{"https://example.com/search?q=hello world", "https://example.com/search?q=hello%20world"},
		{"https://example.com/p?discount=100%", "https://example.com/p?discount=100%25"},
		// 保留字符与片段不动。
		{"https://example.com/p/q?x=1&y=a+b;c=d#frag", "https://example.com/p/q?x=1&y=a+b;c=d#frag"},
		{"  https://example.com/  ", "https://example.com/"},
		// 中文域名 → punycode；端口保留。
		{"https://例子.测试:8443/路径", "https://xn--fsqu00a.xn--0zwm56d:8443/%E8%B7%AF%E5%BE%84"},
	}
	for _, c := range cases {
		got, err := normalizeFetchURL(c.in)
		if err != nil {
			t.Errorf("normalizeFetchURL(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeFetchURL(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		// 幂等：再规范一次不变（即不会双重编码）。
		if again, _ := normalizeFetchURL(got); again != got {
			t.Errorf("not idempotent: %q -> %q", got, again)
		}
	}
}

// fetchServer 记录收到的原始请求行 URI，按 handler 回应。
func fetchServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, func() (string, string)) {
	var mu sync.Mutex
	var uri, q string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uri, q = r.RequestURI, r.URL.Query().Get("q")
		mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() (string, string) { mu.Lock(); defer mu.Unlock(); return uri, q }
}

func runWebFetch(t *testing.T, rawURL string) (any, error) {
	t.Helper()
	def := webFetchToolDef(Config{}.defaults())
	return def.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"url": rawURL})
}

func TestWebFetch_EncodesChineseQuery(t *testing.T) {
	srv, seen := fetchServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	out, err := runWebFetch(t, srv.URL+"/search?q=Re0+老吴+梗+rezero")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	uri, q := seen()
	if uri != "/search?q=Re0+%E8%80%81%E5%90%B4+%E6%A2%97+rezero" {
		t.Fatalf("request URI = %q", uri)
	}
	if q != "Re0 老吴 梗 rezero" {
		t.Fatalf("server decoded q = %q", q)
	}
	if m := out.(map[string]any); m["statusCode"] != 200 || m["body"] != "ok" {
		t.Fatalf("result = %v", m)
	}
}

func TestWebFetch_AlreadyEncodedNotDoubleEncoded(t *testing.T) {
	srv, seen := fetchServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	const path = "/search?q=Re0+%E8%80%81%E5%90%B4&x=a%2Fb"
	if _, err := runWebFetch(t, srv.URL+path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uri, q := seen(); uri != path || q != "Re0 老吴" {
		t.Fatalf("request URI = %q, q = %q", uri, q)
	}
}

func TestWebFetch_Non2xxIsError(t *testing.T) {
	t.Run("400 empty body", func(t *testing.T) {
		srv, _ := fetchServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		})
		out, err := runWebFetch(t, srv.URL+"/search?q=x")
		if err == nil {
			t.Fatalf("non-2xx must be a tool error, got result %v", out)
		}
		for _, want := range []string{"HTTP 400", "<empty body>", "/search?q=x"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err, want)
			}
		}
	})
	t.Run("404 with body snippet", func(t *testing.T) {
		srv, _ := fetchServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "页面不存在 "+strings.Repeat("长", 1000))
		})
		_, err := runWebFetch(t, srv.URL+"/missing")
		if err == nil {
			t.Fatal("expected error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "HTTP 404") || !strings.Contains(msg, "text/plain") || !strings.Contains(msg, "页面不存在") {
			t.Fatalf("error = %q", msg)
		}
		if len(msg) > 800 || !strings.HasSuffix(msg, "…") {
			t.Fatalf("body snippet should be truncated, len=%d", len(msg))
		}
	})
}
