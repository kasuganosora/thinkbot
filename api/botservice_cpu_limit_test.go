package api

import (
	"context"
	"fmt"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/sandbox"
	"testing"
)

// TestParseSandboxCPULimit 覆盖 sandbox.cpu_limit 的三种形态：
// 未配置 / 显式不限制 / 具体核数。重点是不能只靠返回值是否为空来判断——
// 空串既是"未配置"的返回，也是"不限制"的返回，必须看第二个返回值。
func TestParseSandboxCPULimit(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantValue  string
		wantConfig bool
	}{
		{"unset: empty means follow builtin default", "", "", false},
		{"unset: blank string", "   ", "", false},
		{"explicit unlimited: 0", "0", "", true},
		{"explicit unlimited: dash", "-", "", true},
		{"explicit unlimited: keyword", "unlimited", "", true},
		{"explicit unlimited: keyword any case", "  Unlimited ", "", true},
		{"concrete cores", "16", "16", true},
		{"fractional cores", "1.0", "1.0", true},
		{"fractional cores trimmed", " 0.5 ", "0.5", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, configured := parseSandboxCPULimit(c.raw)
			if got != c.wantValue || configured != c.wantConfig {
				t.Fatalf("parseSandboxCPULimit(%q) = (%q, %v), want (%q, %v)",
					c.raw, got, configured, c.wantValue, c.wantConfig)
			}
		})
	}
}

// TestSandboxMemoryLimit 覆盖 per-bot memory_limit_mb 的三种取值。
// 与 CPU 同理：mb==0（不限制）和 mb<0（未指定）都返回空串，靠第二个返回值区分。
func TestSandboxMemoryLimit(t *testing.T) {
	cases := []struct {
		name       string
		mb         int64
		wantValue  string
		wantConfig bool
	}{
		{"unspecified: negative keeps builtin 2G default", -1, "", false},
		{"unlimited: 0", 0, "", true},
		{"concrete: 2048", 2048, "2048m", true},
		{"concrete: 24576", 24576, "24576m", true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/mb=%d", c.name, c.mb), func(t *testing.T) {
			got, configured := sandboxMemoryLimit(c.mb)
			if got != c.wantValue || configured != c.wantConfig {
				t.Fatalf("sandboxMemoryLimit(%d) = (%q, %v), want (%q, %v)",
					c.mb, got, configured, c.wantValue, c.wantConfig)
			}
		})
	}
}

func TestEffectiveSandboxCPULimitRestoresDefault(t *testing.T) {
	store := config.NewStore(nil)
	svc := &BotService{store: store}
	for _, tc := range []struct{ raw, want string }{{"16", "16"}, {"", sandbox.DefaultConfig().CPULimit}, {"unlimited", ""}} {
		if err := store.Set(context.Background(), config.KeySandboxCPULimit, tc.raw); err != nil {
			t.Fatal(err)
		}
		if got := svc.effectiveSandboxCPULimit(); got != tc.want {
			t.Fatalf("setting %q: got %q, want %q", tc.raw, got, tc.want)
		}
	}
}
