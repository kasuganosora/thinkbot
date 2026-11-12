package sandbox

import (
	"strings"
	"testing"
	"time"
)

func TestParseBrowserHandshakeTimeout(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty is default", raw: "  ", want: DefaultBrowserHandshakeTimeout},
		{name: "minutes", raw: "3m", want: 3 * time.Minute},
		{name: "seconds duration", raw: "90s", want: 90 * time.Second},
		{name: "plain seconds", raw: "180", want: 180 * time.Second},
		{name: "fractional seconds", raw: "1.5", want: 1500 * time.Millisecond},
		{name: "lower bound", raw: "1s", want: time.Second},
		{name: "upper bound", raw: "10m", want: 10 * time.Minute},
		{name: "zero", raw: "0", wantErr: true},
		{name: "negative", raw: "-5s", wantErr: true},
		{name: "too short", raw: "500ms", wantErr: true},
		{name: "too long", raw: "11m", wantErr: true},
		{name: "garbage", raw: "soon", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBrowserHandshakeTimeout(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %s, want error", tc.raw, got)
				}
				if !strings.Contains(err.Error(), "sandbox.browser.handshake_timeout") {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

func TestDefaultConfigBrowserHandshakeTimeout(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.BrowserHandshakeTimeout != 3*time.Minute {
		t.Fatalf("default = %s, want 3m", cfg.BrowserHandshakeTimeout)
	}
	if err := ValidateBrowserHandshakeTimeout(cfg.BrowserHandshakeTimeout); err != nil {
		t.Fatal(err)
	}
}
