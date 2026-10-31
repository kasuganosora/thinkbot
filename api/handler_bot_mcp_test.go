package api

import (
	"errors"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/mcp"
)

// TestMaskMCPEntrySecrets 验证读取掩码：敏感键打码、非敏感键与空值原样、
// 元字段保留，且绝不改动入参 entry（新 map），以免破坏落盘值与运行时连接。
func TestMaskMCPEntrySecrets(t *testing.T) {
	orig := mcp.BotServerEntry{
		ID:   "mcp-1",
		Name: "svc",
		Env: map[string]string{
			"Authorization": "Bearer real-token-value-123456", // 敏感
			"API_KEY":       "sk-real-key-abcdefghijklmnop",   // 敏感
			"NODE_ENV":      "production",                     // 非敏感
			"EMPTY_TOKEN":   "",                               // 敏感键但空值 → 不打码
		},
		Headers: map[string]string{
			"X-Auth-Token":           "header-secret-99999999", // 敏感
			"Content-Type":           "application/json",       // 非敏感
			"monkey":                 "business-value",         // 误判防护：含 key 子串但非凭据
			"TOKENIZERS_PARALLELISM": "true",                   // 误判防护：含 token 词但切词后不独立命中
		},
	}

	masked := maskMCPEntrySecrets(orig)

	if masked.Env["Authorization"] != mcpSecretMask {
		t.Fatalf("sensitive env should be masked, got %q", masked.Env["Authorization"])
	}
	if masked.Env["API_KEY"] != mcpSecretMask {
		t.Fatalf("sensitive env API_KEY should be masked, got %q", masked.Env["API_KEY"])
	}
	if masked.Headers["X-Auth-Token"] != mcpSecretMask {
		t.Fatalf("sensitive header should be masked, got %q", masked.Headers["X-Auth-Token"])
	}
	if masked.Env["NODE_ENV"] != "production" {
		t.Fatalf("non-sensitive env should be preserved, got %q", masked.Env["NODE_ENV"])
	}
	if masked.Env["EMPTY_TOKEN"] != "" {
		t.Fatalf("empty sensitive value should stay empty, got %q", masked.Env["EMPTY_TOKEN"])
	}
	if masked.Headers["Content-Type"] != "application/json" {
		t.Fatalf("non-sensitive header should be preserved, got %q", masked.Headers["Content-Type"])
	}
	if masked.Headers["monkey"] != "business-value" {
		t.Fatalf("monkey must not be treated as sensitive, got %q", masked.Headers["monkey"])
	}
	if masked.Headers["TOKENIZERS_PARALLELISM"] != "true" {
		t.Fatalf("TOKENIZERS_PARALLELISM must not be treated as sensitive, got %q", masked.Headers["TOKENIZERS_PARALLELISM"])
	}
	if masked.ID != orig.ID || masked.Name != orig.Name {
		t.Fatalf("metadata should be preserved: got id=%q name=%q", masked.ID, masked.Name)
	}

	// 关键不变量：入参 entry 的 map 未被就地改动
	if orig.Env["Authorization"] != "Bearer real-token-value-123456" {
		t.Fatalf("original entry env was mutated: %q", orig.Env["Authorization"])
	}
	if orig.Headers["X-Auth-Token"] != "header-secret-99999999" {
		t.Fatalf("original entry headers was mutated: %q", orig.Headers["X-Auth-Token"])
	}
}

// TestMergeMCPSecretMap 验证 update 解析：敏感键掩码串沿用旧值、新真实值覆盖、
// 空串清空、非敏感键及无原值的掩码报错（不能静默丢凭据）、非字符串忽略。
func TestMergeMCPSecretMap(t *testing.T) {
	old := map[string]string{
		"Authorization": "Bearer real-old-token-123456",
		"API_KEY":       "sk-old-key-abcdefghijkl",
		"NODE_ENV":      "production",
	}

	t.Run("masked sensitive key keeps old value", func(t *testing.T) {
		got, err := mergeMCPSecretMap(old, map[string]any{
			"Authorization": mcpSecretMask,
			"NODE_ENV":      "staging",
		})
		if err != nil {
			t.Fatalf("merge must succeed, got %v", err)
		}
		if got["Authorization"] != "Bearer real-old-token-123456" {
			t.Fatalf("masked sensitive value must be preserved, got %q", got["Authorization"])
		}
		if got["NODE_ENV"] != "staging" {
			t.Fatalf("non-sensitive field change must apply, got %q", got["NODE_ENV"])
		}
	})

	t.Run("new real value overwrites", func(t *testing.T) {
		got, err := mergeMCPSecretMap(old, map[string]any{"Authorization": "Bearer fresh-new-token-999"})
		if err != nil {
			t.Fatalf("merge must succeed, got %v", err)
		}
		if got["Authorization"] != "Bearer fresh-new-token-999" {
			t.Fatalf("explicit new value must overwrite, got %q", got["Authorization"])
		}
	})

	t.Run("empty string clears sensitive key", func(t *testing.T) {
		got, err := mergeMCPSecretMap(old, map[string]any{"API_KEY": ""})
		if err != nil {
			t.Fatalf("merge must succeed, got %v", err)
		}
		v, ok := got["API_KEY"]
		if !ok || v != "" {
			t.Fatalf("empty string should explicitly clear the key, got %q exists=%v", v, ok)
		}
	})

	for _, key := range []string{"NODE_ENV", "MY_VALUE"} {
		t.Run("mask cannot become ordinary value "+key, func(t *testing.T) {
			if _, err := mergeMCPSecretMap(old, map[string]any{key: mcpSecretMask}); !errors.Is(err, ErrMCPMaskUnresolvable) {
				t.Fatalf("renamed mask must require a new value, got %v", err)
			}
		})
	}
	for _, oldValue := range []string{"", mcpSecretMask} {
		t.Run("mask needs a real stored value "+oldValue, func(t *testing.T) {
			if _, err := mergeMCPSecretMap(map[string]string{"TOKEN": oldValue}, map[string]any{"TOKEN": mcpSecretMask}); !errors.Is(err, ErrMCPMaskUnresolvable) {
				t.Fatalf("unresolvable mask accepted: %v", err)
			}
		})
	}

	t.Run("masked key with no old value fails instead of dropping the secret", func(t *testing.T) {
		got, err := mergeMCPSecretMap(old, map[string]any{"NEW_TOKEN": mcpSecretMask})
		if err == nil {
			t.Fatal("a masked key with no stored value cannot be resolved; merging must fail")
		}
		if !errors.Is(err, ErrMCPMaskUnresolvable) {
			t.Fatalf("expected ErrMCPMaskUnresolvable, got %v", err)
		}
		if got != nil {
			t.Fatalf("no partial result must be returned on failure, got %v", got)
		}
	})

	t.Run("non-string value ignored", func(t *testing.T) {
		got, err := mergeMCPSecretMap(old, map[string]any{"PORT": 8080})
		if err != nil {
			t.Fatalf("merge must succeed, got %v", err)
		}
		if _, exists := got["PORT"]; exists {
			t.Fatal("non-string value must be ignored")
		}
	})
}

// TestMaskMCPEntries 验证批量掩码返回新切片、不改原切片，并覆盖空切片。
func TestMaskMCPEntries(t *testing.T) {
	entries := []mcp.BotServerEntry{
		{ID: "a", Env: map[string]string{"TOKEN": "real-a-token-123456"}},
		{ID: "b", Headers: map[string]string{"Authorization": "Bearer real-b-token"}},
	}
	out := maskMCPEntries(entries)
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out))
	}
	if out[0].Env["TOKEN"] != mcpSecretMask {
		t.Fatalf("entry a sensitive env should be masked, got %q", out[0].Env["TOKEN"])
	}
	if out[1].Headers["Authorization"] != mcpSecretMask {
		t.Fatalf("entry b sensitive header should be masked, got %q", out[1].Headers["Authorization"])
	}
	if entries[0].Env["TOKEN"] != "real-a-token-123456" {
		t.Fatalf("original slice was mutated: %q", entries[0].Env["TOKEN"])
	}
	if entries[1].Headers["Authorization"] != "Bearer real-b-token" {
		t.Fatalf("original slice was mutated: %q", entries[1].Headers["Authorization"])
	}
	if got := maskMCPEntries(nil); got != nil {
		t.Fatalf("nil input should return nil, got %#v", got)
	}
}

// TestMCPSecretMaskRoundTrip_PreservesRealValue 是最关键的闭环：模拟
// 「读取接口打码 → 前端原样回传做 update → merge 后真实凭据不丢且普通字段改动生效」。
func TestMCPSecretMaskRoundTrip_PreservesRealValue(t *testing.T) {
	orig := mcp.BotServerEntry{
		ID:   "mcp-rt",
		Name: "svc",
		Env: map[string]string{
			"Authorization": "Bearer real-roundtrip-token-123",
			"NODE_ENV":      "production",
		},
		Headers: map[string]string{
			"X-Api-Key":    "real-roundtrip-key-abcdefgh",
			"Content-Type": "application/json",
		},
	}

	// 读取接口返回给前端的是打码后的拷贝
	masked := maskMCPEntrySecrets(orig)

	// 前端原样回传：敏感字段是掩码串，普通字段照旧
	reqEnv := map[string]any{}
	for k, v := range masked.Env {
		reqEnv[k] = v
	}
	reqHeaders := map[string]any{}
	for k, v := range masked.Headers {
		reqHeaders[k] = v
	}
	// 用户顺手改了一个普通字段
	reqEnv["NODE_ENV"] = "staging"

	// 后端 update 解析后：敏感值必须仍是最初的真实值，普通字段改动生效
	gotEnv, err := mergeMCPSecretMap(orig.Env, reqEnv)
	if err != nil {
		t.Fatalf("round-trip env merge must succeed, got %v", err)
	}
	gotHeaders, err := mergeMCPSecretMap(orig.Headers, reqHeaders)
	if err != nil {
		t.Fatalf("round-trip headers merge must succeed, got %v", err)
	}

	if gotEnv["Authorization"] != "Bearer real-roundtrip-token-123" {
		t.Fatalf("round-trip must preserve env secret, got %q", gotEnv["Authorization"])
	}
	if gotHeaders["X-Api-Key"] != "real-roundtrip-key-abcdefgh" {
		t.Fatalf("round-trip must preserve header secret, got %q", gotHeaders["X-Api-Key"])
	}
	if gotEnv["NODE_ENV"] != "staging" {
		t.Fatalf("non-sensitive edit must apply through round-trip, got %q", gotEnv["NODE_ENV"])
	}
}

func TestMCPHandlersRejectUnresolvableMasksWithoutSaving(t *testing.T) {
	t.Chdir(t.TempDir())
	const bot = "mask-test"
	original := []mcp.BotServerEntry{{ID: "existing", Name: "existing", Env: map[string]string{"TOKEN": "fictional-original-token"}}}
	if err := mcp.SaveBotServers(bot, original); err != nil {
		t.Fatal(err)
	}
	server := &Server{logger: zap.NewNop().Sugar()}
	for _, tc := range []struct {
		name, method, body string
		handler            gin.HandlerFunc
	}{
		{"create env", "POST", `{"name":"new","env":{"TOKEN":"********"}}`, server.handleCreateBotMcp},
		{"create renamed key", "POST", `{"name":"new","env":{"MY_VALUE":"********"}}`, server.handleCreateBotMcp},
		{"create header", "POST", `{"name":"new","headers":{"Authorization":"********"}}`, server.handleCreateBotMcp},
		{"update renamed key", "PUT", `{"name":"changed","env":{"MY_VALUE":"********"}}`, server.handleUpdateBotMcp},
		{"import env", "POST", `{"mcpServers":{"valid":{"env":{"PORT":"3000"}},"invalid":{"env":{"TOKEN":"********"}}}}`, server.handleImportBotMcp},
		{"import header", "POST", `{"mcpServers":{"invalid":{"headers":{"MY_VALUE":"********"}}}}`, server.handleImportBotMcp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "id", Value: bot}, {Key: "mid", Value: "existing"}}
			ctx.Request = httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			tc.handler(ctx)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			got, err := mcp.LoadBotServers(bot)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, original) {
				t.Fatalf("rejected request modified stored config: %+v", got)
			}
			if strings.Contains(recorder.Body.String(), "fictional-original-token") {
				t.Fatal("error leaked stored credential")
			}
		})
	}
}
