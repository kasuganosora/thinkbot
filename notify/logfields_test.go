package notify

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The notification level must not be logged under "level": the JSON encoder
// already uses that key for the log severity, so `"level":"critical"` made
// log parsers read an INFO line as CRITICAL (2026-09-28).
func TestDeliveredLogDoesNotReuseLevelKey(t *testing.T) {
	h := newHarness(t)
	core, logs := observer.New(zapcore.DebugLevel)
	h.svc.Logger = zap.New(core).Sugar()
	res := h.svc.Notify(context.Background(), "bot-a", caller, smartReq())
	if !res.Delivered {
		t.Fatalf("%+v", res)
	}
	entries := logs.FilterMessage("notify: delivered").All()
	if len(entries) != 1 {
		t.Fatalf("delivered log lines = %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if _, bad := fields["level"]; bad {
		t.Fatalf("delivered log must not carry a \"level\" field: %v", fields)
	}
	if fields["notify_level"] != "critical" {
		t.Fatalf("notify_level = %v", fields["notify_level"])
	}
	for _, e := range logs.All() {
		for _, reserved := range []string{"level", "ts", "msg", "caller"} {
			if _, bad := e.ContextMap()[reserved]; bad {
				t.Errorf("log %q uses reserved key %q", e.Message, reserved)
			}
		}
	}
}
