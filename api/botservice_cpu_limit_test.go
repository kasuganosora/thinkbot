package api

import "testing"

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
