package api

import (
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/bot"
)

func TestBrowserMCPRuntimeCheckStates(t *testing.T) {
	next := time.Date(2026, 11, 12, 3, 4, 5, 0, time.UTC)
	failed := browserMCPRuntimeCheck(bot.BrowserMCPStatus{
		State:     bot.BrowserMCPStateFailed,
		LastError: "context deadline exceeded",
		Attempts:  2,
		NextRetry: next,
	})
	if failed.OK {
		t.Fatal("failed browser MCP must surface as not ok")
	}
	if failed.Name != "浏览器 MCP" {
		t.Fatalf("name = %q", failed.Name)
	}
	for _, want := range []string{"state=failed", "attempts=2", "next=2026-11-12T03:04:05Z", "context deadline exceeded"} {
		if !strings.Contains(failed.Extra, want) {
			t.Fatalf("extra %q missing %q", failed.Extra, want)
		}
	}

	pending := browserMCPRuntimeCheck(bot.BrowserMCPStatus{State: bot.BrowserMCPStatePending})
	if !pending.OK || !strings.Contains(pending.Extra, "state=pending") {
		t.Fatalf("pending = %+v", pending)
	}
	connecting := browserMCPRuntimeCheck(bot.BrowserMCPStatus{State: bot.BrowserMCPStateConnecting, Attempts: 1})
	if !connecting.OK || !strings.Contains(connecting.Message, "后台") {
		t.Fatalf("connecting = %+v", connecting)
	}
	connected := browserMCPRuntimeCheck(bot.BrowserMCPStatus{
		State: bot.BrowserMCPStateConnected, Attempts: 2, Tools: []string{"browser__navigate"},
	})
	if !connected.OK || !strings.Contains(connected.Extra, "tools=1") {
		t.Fatalf("connected = %+v", connected)
	}
}
