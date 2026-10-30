package secret

import (
	"os"
	"path/filepath"
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
	if err := v.Register("svc.token", "abcdefghijklmnop", "访问示例服务"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got := v.Resolve(map[string]any{
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
	if err := v.Register("a", "value-with-{{secret:b}}-inside-long", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("b", "another-long-secret-value", ""); err != nil {
		t.Fatal(err)
	}
	got := v.Resolve("{{secret:a}}").(string)
	if got != "value-with-{{secret:b}}-inside-long" {
		t.Fatalf("single-pass violated: %q", got)
	}
}

func TestResolveUnknownKept(t *testing.T) {
	v := newTestVault(t)
	got := v.Resolve("x {{secret:missing}} y").(string)
	if got != "x {{secret:missing}} y" {
		t.Fatalf("unknown placeholder not preserved: %q", got)
	}
}

func TestRedactLongestFirst(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("short", "abcdefghijkl", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("long", "abcdefghijklmnopqrst", ""); err != nil {
		t.Fatal(err)
	}
	got := v.Redact("token=abcdefghijklmnopqrst")
	if got != "token={{secret:long}}" {
		t.Fatalf("longest-first violated: %q", got)
	}
}

func TestRedactValueRecursive(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("tok", "verylongsecrettoken", ""); err != nil {
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
	if err := v.Register("db.password", "supersecretvalue123", "示例库口令"); err != nil {
		t.Fatal(err)
	}
	v2, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if val, ok := v2.Get("db.password"); !ok || val != "supersecretvalue123" {
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

func TestRejectShortAndBadName(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("x", "short", ""); err == nil {
		t.Fatal("short value should be rejected")
	}
	if err := v.Register("x", "longenoughvalue123", ""); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
	if err := v.Register("bad name", "longenoughvalue123", ""); err == nil {
		t.Fatal("invalid name should be rejected")
	}
}

func TestCatalogHasNoValues(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("alpha", "alpha-secret-value-long", "用途说明"); err != nil {
		t.Fatal(err)
	}
	cat := v.Catalog()
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
	if err := v.Register("tok", "abcdefghijklmnop", ""); err != nil {
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
	if err := v.Register("long", "abcdefghijklmnopqrst", ""); err != nil {
		t.Fatal(err)
	}
	if got := v.StreamHoldLen("x abcdefghijklmnopqr"); got != 18 {
		t.Fatalf("longest prefix across values: got %d, want 18", got)
	}
}
