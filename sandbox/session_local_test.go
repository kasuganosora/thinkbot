//go:build unix

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testSessionConfig() SessionConfig {
	c := DefaultSessionConfig()
	c.StableWindow = 20 * time.Millisecond
	c.HealthWindow = 500 * time.Millisecond
	c.RetainAfterExit = 200 * time.Millisecond
	return c
}

func TestLocalSession_EndToEndSpawnPollSendClose(t *testing.T) {
	cfg := testSessionConfig()
	sp := newLocalSessionSpawner(cfg, zap.NewNop().Sugar(), t.TempDir())
	h, err := sp.Spawn(context.Background(), "bot-test", SpawnRequest{
		Command: "printf 'hello session\\n'; cat",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer h.Close()

	v, err := h.Poll(0, "screen", 40, 3*time.Second)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v.State != SessionRunning {
		t.Fatalf("state = %q, want running", v.State)
	}
	if !strings.Contains(v.Screen+v.Delta, "hello session") {
		t.Fatalf("first screen missed the greeting: screen=%q delta=%q", v.Screen, v.Delta)
	}
	if v.SettledSeq > v.Seq {
		t.Errorf("settledSeq %d must never exceed seq %d", v.SettledSeq, v.Seq)
	}
	if v.Health.Verdict == "" {
		t.Error("poll must return health")
	}

	if err := h.Send([]byte("ping\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Poll with the raw seq on purpose: the implementation must clamp it to
	// settledSeq rather than silently skipping unsettled content.
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		v2, err := h.Poll(v.Seq, "screen", 40, 300*time.Millisecond)
		if err != nil {
			t.Fatalf("second poll: %v", err)
		}
		if strings.Contains(v2.Delta+v2.Screen, "ping") {
			found = true
			break
		}
	}
	if !found {
		t.Error("input echo never appeared; clamping sinceSeq to seq may have dropped content")
	}

	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	final := h.View()
	if final.State != SessionExited {
		t.Errorf("state after close = %q, want exited", final.State)
	}
	if final.Reason != SessionReasonClose {
		t.Errorf("reason after close = %q, want %q", final.Reason, SessionReasonClose)
	}
}

func TestLocalSession_IdleTTLReclaimsWithoutAnyAgentCall(t *testing.T) {
	cfg := testSessionConfig()
	cfg.IdleTTL = 250 * time.Millisecond
	cfg.TermGrace = 100 * time.Millisecond
	sp := newLocalSessionSpawner(cfg, zap.NewNop().Sugar(), t.TempDir())
	h, err := sp.Spawn(context.Background(), "bot-test", SpawnRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Deliberately never call Poll/Send/Watch/Spawn again. The process-level
	// TTL must fire from the session's own event loop (docs §3.7).
	deadline := time.Now().Add(4 * time.Second)
	var last SessionView
	for time.Now().Before(deadline) {
		last = h.View()
		if last.State == SessionExited {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if last.State != SessionExited {
		t.Fatalf("session survived its idle TTL without any agent call: state=%q", last.State)
	}
	if last.Reason != SessionReasonIdleTTL {
		t.Errorf("reason = %q, want %q", last.Reason, SessionReasonIdleTTL)
	}
	_ = h.Close()
}

func TestSessionRegistry_BotIsolationWatchReadOnly(t *testing.T) {
	cfg := testSessionConfig()
	sp := newLocalSessionSpawner(cfg, zap.NewNop().Sugar(), t.TempDir())
	reg := NewSessionRegistry(cfg, zap.NewNop().Sugar(), sp)

	v, err := reg.Spawn(context.Background(), "bot-a", "default", SpawnRequest{
		Command: "printf 'ready\\n'; sleep 5",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer reg.CloseAll("bot-a")

	before, err := reg.get("bot-a", v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	touchBefore := before.lastTouch

	// watch must not consume deltas, advance cursors, or refresh lastTouch.
	time.Sleep(30 * time.Millisecond)
	res := reg.Watch("bot-a", "default", 8, false)
	if res.Total != 1 {
		t.Errorf("watch total = %d, want 1", res.Total)
	}
	if reg.cursors[v.SessionID]["default"] != 0 {
		t.Error("watch advanced the per-agent cursor")
	}
	after, _ := reg.get("bot-a", v.SessionID)
	if !after.lastTouch.Equal(touchBefore) {
		t.Error("watch refreshed lastTouch; that would disable the idle TTL")
	}

	if _, err := reg.Poll("bot-b", "default", v.SessionID, 0, "screen", 40, 0); err != ErrSessionWrongBot {
		t.Errorf("cross-bot poll error = %v, want ErrSessionWrongBot", err)
	}
	if n := reg.Count("bot-b"); n != 0 {
		t.Errorf("bot-b sees %d sessions, want 0", n)
	}
	if other := reg.Watch("bot-b", "default", 8, false); other.Total != 0 {
		t.Errorf("bot-b watch total = %d, want 0", other.Total)
	}

	if n := reg.CloseAll("bot-a"); n != 1 {
		t.Errorf("closeAll closed %d sessions, want 1", n)
	}
	if reg.Count("bot-a") != 0 {
		t.Error("registry still holds sessions after closeAll")
	}
}
