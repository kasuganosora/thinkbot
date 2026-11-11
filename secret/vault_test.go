package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestVault(t *testing.T) *Vault {
	t.Helper()
	v, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestRegisterAndResolve(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "svc.token", "abcdefghijklmnop", "访问示例服务"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got := v.Resolve("", map[string]any{
		"headers": map[string]any{"Authorization": "Bearer {{secret:svc.token}}"},
		"url":     "https://internal.example.com/api",
	})
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("Resolve returned %T", got)
	}
	h := m["headers"].(map[string]any)
	if h["Authorization"] != "Bearer abcdefghijklmnop" {
		t.Fatalf("unexpected header: %v", h["Authorization"])
	}
}

func TestResolveSinglePass(t *testing.T) {
	// 真实值本身含 {{...}}，不得被二次解析。
	v := newTestVault(t)
	if err := v.Register("", "a", "value-with-{{secret:b}}-inside-long", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("", "b", "another-long-secret-value", ""); err != nil {
		t.Fatal(err)
	}
	got := v.Resolve("", "{{secret:a}}").(string)
	if got != "value-with-{{secret:b}}-inside-long" {
		t.Fatalf("single-pass violated: %q", got)
	}
}

func TestResolveUnknownKept(t *testing.T) {
	v := newTestVault(t)
	got := v.Resolve("", "x {{secret:missing}} y").(string)
	if got != "x {{secret:missing}} y" {
		t.Fatalf("unknown placeholder not preserved: %q", got)
	}
}

func TestRedactLongestFirst(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "short", "abcdefghijkl", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("", "long", "abcdefghijklmnopqrst", ""); err != nil {
		t.Fatal(err)
	}
	got := v.Redact("token=abcdefghijklmnopqrst")
	if got != "token={{secret:long}}" {
		t.Fatalf("longest-first violated: %q", got)
	}
}

// 允许登记短值后，占位符文本 {{secret:name}} 本身可能被短值二次命中。
// marker 单遍还原要保证替换结果是干净的占位符。
func TestRedactShortValueDoesNotCorruptPlaceholder(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "s", "secret", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("", "long", "LONG_CREDENTIAL_VALUE_abcdef", ""); err != nil {
		t.Fatal(err)
	}
	if got := v.Redact("use LONG_CREDENTIAL_VALUE_abcdef now"); got != "use {{secret:long}} now" {
		t.Fatalf("placeholder corrupted by short value: %q", got)
	}
}

func TestRedactPreservesTextContainingMarkerPrefix(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "k", "LONG_CREDENTIAL_VALUE_abcdef", ""); err != nil {
		t.Fatal(err)
	}
	in := "\x00S0\x00 and LONG_CREDENTIAL_VALUE_abcdef"
	if got := v.Redact(in); got != "\x00S0\x00 and {{secret:k}}" {
		t.Fatalf("marker collision corrupted the text: %q", got)
	}
}

func TestRedactShortValueBoundaries(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "pin", "123456", ""); err != nil {
		t.Fatal(err)
	}
	if got := v.Redact("pin=123456&x=1"); got != "pin={{secret:pin}}&x=1" {
		t.Fatalf("short credential not redacted in assignment context: %q", got)
	}
	if got := v.Redact("2026-123456-11"); got != "2026-123456-11" {
		t.Fatalf("short credential corrupted a date: %q", got)
	}
	// 低于遮蔽下限的值仍可登记、可解析，但不做出站遮蔽（否则整段文本被打码）。
	if err := v.Register("", "one", "1", ""); err != nil {
		t.Fatal(err)
	}
	if got := v.Resolve("", "{{secret:one}}"); got != "1" {
		t.Fatalf("short value not resolvable: %v", got)
	}
	if got := v.Redact("build 1 of 12"); got != "build 1 of 12" {
		t.Fatalf("one-character value corrupted ordinary text: %q", got)
	}
}

func TestRedactValueRecursive(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "tok", "verylongsecrettoken", ""); err != nil {
		t.Fatal(err)
	}
	got := v.RedactValue(map[string]any{
		"body": "key=verylongsecrettoken",
		"nested": map[string]any{
			"list": []any{"a=verylongsecrettoken", "plain"},
		},
	}).(map[string]any)
	if got["body"] != "key={{secret:tok}}" {
		t.Fatalf("body not redacted: %v", got["body"])
	}
	list := got["nested"].(map[string]any)["list"].([]any)
	if list[0] != "a={{secret:tok}}" {
		t.Fatalf("list not redacted: %v", list[0])
	}
}

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vault.json")
	v, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Register("", "db.password", "supersecretvalue123", "示例库口令"); err != nil {
		t.Fatal(err)
	}
	v2, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if val, ok := v2.Get("", "db.password"); !ok || val != "supersecretvalue123" {
		t.Fatalf("round-trip failed: %q %v", val, ok)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("vault file mode = %o, want 0600", info.Mode().Perm())
	}
}

// 登记只做非空校验：短口令 / PIN / 验证码也是合法凭据。
func TestRegisterAcceptsShortValueRejectsEmptyAndBadName(t *testing.T) {
	v := newTestVault(t)
	for _, short := range []string{"1", "a", "123456", "p@ss"} {
		if err := v.Register("", "x", short, ""); err != nil {
			t.Fatalf("short value %q rejected: %v", short, err)
		}
		if got, ok := v.Get("", "x"); !ok || got != short {
			t.Fatalf("short value %q not stored: %q %v", short, got, ok)
		}
	}
	for _, empty := range []string{"", "   ", "\n\t"} {
		if err := v.Register("", "x", empty, ""); err != ErrEmptyValue {
			t.Fatalf("empty value %q should be rejected with ErrEmptyValue, got %v", empty, err)
		}
	}
	// 非空校验之外不改写值：密码里的首尾空格是有效字符。
	const spaced = "  pass word  "
	if err := v.Register("", "x", spaced, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.Get("", "x"); got != spaced {
		t.Fatalf("value was trimmed: %q, want %q", got, spaced)
	}
	if err := v.Register("", "bad name", "longenoughvalue123", ""); err == nil {
		t.Fatal("invalid name should be rejected")
	}
}

func TestCatalogHasNoValues(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "alpha", "alpha-secret-value-long", "用途说明"); err != nil {
		t.Fatal(err)
	}
	cat := v.Catalog("")
	if len(cat) != 1 || cat[0].Name != "alpha" || cat[0].Description != "用途说明" {
		t.Fatalf("unexpected catalog: %+v", cat)
	}
	// CatalogEntry 结构本身不含 Value 字段，编译期即保证不泄露。
	if len(Placeholder("alpha")) == 0 {
		t.Fatal("empty placeholder")
	}
}

func TestStreamHoldLen(t *testing.T) {
	v := newTestVault(t)
	// 空库：普通文本零扣留。
	if n := v.StreamHoldLen("hello world"); n != 0 {
		t.Fatalf("empty vault should not hold, got %d", n)
	}
	if err := v.Register("", "tok", "abcdefghijklmnop", ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want int
	}{
		{"hello world", 0},        // 无前缀
		{"prefix abc", 3},         // 短真前缀
		{"prefix abcdef", 6},      // 更长真前缀
		{"x abcdefghijklmno", 15}, // 值长 16，最长真前缀 15
		{"abcdefghijklmnop", 0},   // 完整值结尾：交给 Redact，不扣
		{"tail abcdefghijklmnop", 0},
		{"abc and more", 0}, // 前缀不在尾部
		{"", 0},
	}
	for _, c := range cases {
		if got := v.StreamHoldLen(c.in); got != c.want {
			t.Errorf("StreamHoldLen(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// 多值取最长真前缀。
	if err := v.Register("", "long", "abcdefghijklmnopqrst", ""); err != nil {
		t.Fatal(err)
	}
	if got := v.StreamHoldLen("x abcdefghijklmnopqr"); got != 18 {
		t.Fatalf("longest prefix across values: got %d, want 18", got)
	}
}

// TestBotIsolation 守住按 bot 隔离：bot A 的占位符不能被 bot B 还原，目录互不可见；
// 全局条目所有 bot 可见；同名时本 bot 优先；Redact 不分归属（出站防线多遮不漏）。
func TestBotIsolation(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("bot-a", "svc.token", "value-owned-by-bot-a", "A 的令牌"); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("", "shared.key", "value-shared-globally", "全局"); err != nil {
		t.Fatal(err)
	}

	if got := v.Resolve("bot-a", "{{secret:svc.token}}"); got != "value-owned-by-bot-a" {
		t.Fatalf("owner resolve = %v", got)
	}
	if got := v.Resolve("bot-b", "{{secret:svc.token}}"); got != "{{secret:svc.token}}" {
		t.Fatalf("cross-bot resolve leaked: %v", got)
	}
	if got := v.Resolve("", "{{secret:svc.token}}"); got != "{{secret:svc.token}}" {
		t.Fatalf("unowned ctx must not see bot-scoped entry: %v", got)
	}
	if got := v.Resolve("bot-b", "{{secret:shared.key}}"); got != "value-shared-globally" {
		t.Fatalf("global entry not visible to other bot: %v", got)
	}

	names := func(c []CatalogEntry) []string {
		out := make([]string, 0, len(c))
		for _, e := range c {
			out = append(out, e.Name)
		}
		return out
	}
	if got := names(v.Catalog("bot-b")); len(got) != 1 || got[0] != "shared.key" {
		t.Fatalf("bot-b catalog = %v, want only shared.key", got)
	}
	if got := names(v.Catalog("bot-a")); len(got) != 2 {
		t.Fatalf("bot-a catalog = %v, want svc.token + shared.key", got)
	}

	// 同名：本 bot 覆盖全局，且目录只出现一次。
	if err := v.Register("bot-b", "shared.key", "bot-b-overrides-shared", ""); err != nil {
		t.Fatal(err)
	}
	if got := v.Resolve("bot-b", "{{secret:shared.key}}"); got != "bot-b-overrides-shared" {
		t.Fatalf("own entry should shadow global: %v", got)
	}
	if got := v.Resolve("bot-a", "{{secret:shared.key}}"); got != "value-shared-globally" {
		t.Fatalf("bot-b override leaked to bot-a: %v", got)
	}
	if got := names(v.Catalog("bot-b")); len(got) != 1 {
		t.Fatalf("shadowed name listed twice: %v", got)
	}

	// Redact 对所有归属生效。
	if got := v.Redact("x value-owned-by-bot-a y"); got != "x {{secret:svc.token}} y" {
		t.Fatalf("redact must cover every bot: %q", got)
	}
}

// TestLoadLegacyFormat 早期文件是 map[name]Entry，载入后应成为全局条目，
// 再次保存写成新格式且能读回。
func TestLoadLegacyFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vault.json")
	legacy := `{"old.token":{"name":"old.token","value":"legacy-secret-value-1","updated_at":1}}`
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := New(p)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if got, ok := v.Get("any-bot", "old.token"); !ok || got != "legacy-secret-value-1" {
		t.Fatalf("legacy entry should be global: %q %v", got, ok)
	}
	if err := v.Register("bot-a", "new.token", "new-secret-value-abc", ""); err != nil {
		t.Fatal(err)
	}
	v2, err := New(p)
	if err != nil {
		t.Fatalf("reload new format: %v", err)
	}
	if _, ok := v2.Get("bot-a", "old.token"); !ok {
		t.Fatal("legacy entry lost after re-save")
	}
	if _, ok := v2.Get("bot-b", "new.token"); ok {
		t.Fatal("bot-scoped entry became global after reload")
	}
}

// TestRegisterRollsBackOnSaveFailure 持久化失败时内存不能留着新值，
// 否则调用方收到错误、却在重启前一直「能用」，重启后静默消失。
func TestRegisterRollsBackOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	// 先按「文件尚不存在」正常构造，再把父目录位置占成普通文件：save 的 MkdirAll 必然失败。
	v, err := New(filepath.Join(blocker, "vault.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot-a", "svc.token", "value-that-cannot-save", ""); err == nil {
		t.Fatal("expected save error")
	}
	if _, ok := v.Get("bot-a", "svc.token"); ok {
		t.Fatal("entry kept in memory after failed save")
	}
}

// TestRegisterRefusedWhenLoadFailed 加载失败的库拒绝登记，避免覆盖读不出来的旧文件。
func TestRegisterRefusedWhenLoadFailed(t *testing.T) {
	v := &Vault{entries: map[entryKey]Entry{}, loadErr: os.ErrPermission}
	if err := v.Register("bot-a", "svc.token", "some-long-secret-value", ""); err != ErrUnavailable {
		t.Fatalf("Register on failed vault = %v, want ErrUnavailable", err)
	}
}

func TestConcurrentRegisterPersistsEverySuccessfulEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	v, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := v.Register("bot", fmt.Sprintf("key%d", i), fmt.Sprintf("fictional-password-%d", i), "test"); err != nil {
				t.Errorf("register: %v", err)
			}
		}(i)
	}
	wg.Wait()
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		name := fmt.Sprintf("key%d", i)
		want := fmt.Sprintf("fictional-password-%d", i)
		if got, ok := reloaded.Get("bot", name); !ok || got != want {
			t.Errorf("persisted entry %s missing", name)
		}
		if got, ok := v.Get("bot", name); !ok || got != want {
			t.Errorf("memory entry %s missing", name)
		}
	}
}
