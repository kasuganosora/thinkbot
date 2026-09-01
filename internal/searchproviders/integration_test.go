package searchproviders

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/config"
)

// ============================================================================
// 真实 Brave Search API 集成测试
//
// 验证 searchproviders 对官方 Web Search 端点的调用链路：
//
//	Provider{type:brave} → callBrave → api.search.brave.com/res/v1/web/search
//	→ web.results{title,url,description}
//
// 运行方式（未配置凭据时自动 Skip，不会让 go test ./... 因缺外部服务失败）：
//
//	cp internal/searchproviders/.env.test.example internal/searchproviders/.env.test
//	# 填入自己的 Brave API Key
//	go test -v -run TestIntegration ./internal/searchproviders/ -timeout 60s
//
// 凭据只从环境变量或 .env.test 读取，**禁止硬编码进本文件**。
// .env.test 已被 .gitignore 排除，提交时不要把真 Key 写进源码或 example。
// ============================================================================

const (
	envKeyBraveAPIKey = "THINKBOT_TEST_BRAVE_API_KEY"
)

var integBraveAPIKey = integEnv(envKeyBraveAPIKey, "")

// integEnvFiles 合并加载本包 .env.test 与仓库根 .env（只读一次）。
var integEnvFiles = sync.OnceValue(func() map[string]string {
	merged := make(map[string]string)
	for _, path := range []string{".env.test", filepath.Join("..", "..", ".env")} {
		values, err := config.LoadEnvFile(path)
		if err != nil {
			continue
		}
		for k, v := range values {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
	}
	return merged
})

func integEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := integEnvFiles()[key]; v != "" {
		return v
	}
	return def
}

func skipIfNoBrave(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if strings.TrimSpace(integBraveAPIKey) == "" {
		t.Skipf("skipping Brave integration test: %s not set "+
			"(put it in internal/searchproviders/.env.test — see .env.test.example)", envKeyBraveAPIKey)
	}
}

// TestIntegration_Brave_WebSearch 对真实 Brave Web Search API 发一次查询。
func TestIntegration_Brave_WebSearch(t *testing.T) {
	skipIfNoBrave(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := Provider{
		ID:      "integ-brave",
		Type:    TypeBrave,
		Name:    "Brave Integration",
		Enabled: true,
		APIKey:  integBraveAPIKey,
		Timeout: 25,
	}

	results, err := Search(ctx, p, "golang programming language", 5)
	if err != nil {
		t.Fatalf("Brave web search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result from Brave")
	}
	if len(results) > 5 {
		t.Fatalf("got %d results, want <= 5", len(results))
	}

	for i, r := range results {
		if strings.TrimSpace(r.Title) == "" {
			t.Errorf("result[%d]: empty title", i)
		}
		if strings.TrimSpace(r.URL) == "" {
			t.Errorf("result[%d]: empty url", i)
		}
		if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
			t.Errorf("result[%d]: url not http(s): %q", i, r.URL)
		}
		if strings.Contains(r.URL, "api.duckduckgo.com") || strings.Contains(r.URL, "duckduckgo.com/?q=") {
			t.Errorf("result[%d]: leaked DDG placeholder: %q", i, r.URL)
		}
		t.Logf("result[%d]: %s | %s", i, r.Title, r.URL)
	}
}

// TestIntegration_Brave_ViaSearchEnabled 走启用列表 + fallback 入口打 Brave。
func TestIntegration_Brave_ViaSearchEnabled(t *testing.T) {
	skipIfNoBrave(t)

	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "providers.json"))
	if err := store.Save([]Provider{{
		ID:      "integ-brave-enabled",
		Type:    TypeBrave,
		Name:    "Brave Enabled",
		Enabled: true,
		APIKey:  integBraveAPIKey,
		Timeout: 25,
	}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := SearchEnabled(ctx, store, "Brave Search API documentation", 3)
	if err != nil {
		t.Fatalf("SearchEnabled via Brave failed: %v", err)
	}
	if out.Fallback {
		t.Fatalf("expected primary Brave hit without fallback, attempted=%v", out.Attempted)
	}
	if out.Provider.Type != TypeBrave {
		t.Fatalf("engine=%q, want brave", out.Provider.Type)
	}
	if len(out.Results) == 0 {
		t.Fatal("expected results")
	}
	t.Logf("engine=%s provider=%s count=%d", out.Provider.Type, out.Provider.Name, len(out.Results))
}

// TestIntegration_Brave_AuthFailure 错误 Key 应返回可识别的鉴权错误（不触发假结果）。
func TestIntegration_Brave_AuthFailure(t *testing.T) {
	skipIfNoBrave(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := Search(ctx, Provider{
		Type:    TypeBrave,
		APIKey:  "BSA_invalid_key_for_integration_test",
		Timeout: 15,
	}, "test query", 3)
	if err == nil {
		t.Fatal("expected auth error for invalid Brave key")
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "duckduckgo.com/?q=") {
		t.Fatalf("fake DDG link on auth failure: %v", err)
	}
	// Brave 通常 401/403；把状态码或 unauthorized 类信息带出来即可。
	if !strings.Contains(msg, "401") && !strings.Contains(msg, "403") &&
		!strings.Contains(msg, "unauthor") && !strings.Contains(msg, "forbidden") &&
		!strings.Contains(msg, "invalid") && !strings.Contains(msg, "subscription") {
		t.Logf("auth failure message (informational): %v", err)
	}
}

