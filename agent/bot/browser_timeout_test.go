package bot

import (
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/sandbox"
)

func TestBrowserHandshakeTimeoutFallsBackToThreeMinutes(t *testing.T) {
	if sandbox.DefaultBrowserHandshakeTimeout != 3*time.Minute {
		t.Fatalf("default = %s, want 3m", sandbox.DefaultBrowserHandshakeTimeout)
	}
	if got := browserHandshakeTimeout(sandbox.Config{}); got != 3*time.Minute {
		t.Fatalf("zero config = %s, want 3m (not the generic 8s)", got)
	}
	cfg := sandbox.Config{BrowserHandshakeTimeout: 90 * time.Second}
	if got := browserHandshakeTimeout(cfg); got != 90*time.Second {
		t.Fatalf("valid override = %s", got)
	}
	for _, bad := range []time.Duration{time.Millisecond, 50 * time.Millisecond, 11 * time.Minute} {
		cfg.BrowserHandshakeTimeout = bad
		if got := browserHandshakeTimeout(cfg); got != 3*time.Minute {
			t.Fatalf("invalid %s fell through as %s", bad, got)
		}
	}
}
