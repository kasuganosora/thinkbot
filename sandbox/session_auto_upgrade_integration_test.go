//go:build unix

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real-PTY auto-upgrade smoke test. Fake registries prove the decision logic,
// but docs §11 insists "mechanism exists != mechanism works": this drives the
// actual SessionRegistry with a real PTY process on two opposite shapes.
func TestAutoUpgrade_RealPTY(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.AutoUpgradeWindow = 120 * time.Millisecond
	cfg.AutoUpgradeMaxWait = 600 * time.Millisecond
	cfg = fillSessionDefaults(cfg)

	root := t.TempDir()
	spawner := newLocalSessionSpawner(cfg, nil, root)
	reg := NewSessionRegistry(cfg, nil, spawner)
	defer reg.CloseAll("bot-real")

	t.Run("short command returns one-shot without losing output", func(t *testing.T) {
		out, err := runAutoUpgrade(context.Background(), reg, "bot-real",
			SpawnRequest{
				Command: "printf 'AUTO_MARKER_42\\n'",
				Cwd:     root, Background: true,
			}, cfg)
		if err != nil {
			t.Fatalf("runAutoUpgrade: %v", err)
		}
		if out["upgraded"] != false {
			t.Fatalf("short command must not upgrade: %#v", out)
		}
		stdout, _ := out["stdout"].(string)
		if !strings.Contains(stdout, "AUTO_MARKER_42") {
			t.Errorf("captured output lost: %q", stdout)
		}
		if reg.Count("bot-real") != 0 {
			t.Error("one-shot branch leaked a registry entry")
		}
	})

	t.Run("alive with visible output upgrades to a drivable session", func(t *testing.T) {
		// A long-running process that immediately prints something and keeps
		// running: the shape auto is meant to hand back as a sessionId.
		out, err := runAutoUpgrade(context.Background(), reg, "bot-real",
			SpawnRequest{
				Command: "printf 'READY_LONG\\n'; sleep 30",
				Cwd:     root, Background: true,
			}, cfg)
		if err != nil {
			t.Fatalf("runAutoUpgrade: %v", err)
		}
		if out["upgraded"] != true {
			t.Fatalf("long-running process should upgrade: %#v", out)
		}
		sid, _ := out["sessionId"].(string)
		if sid == "" {
			t.Fatal("upgraded result has no sessionId")
		}
		// The whole point of the handle: the agent can poll the already-captured
		// output and then drive the process.
		view, err := reg.Poll("bot-real", sessionAgent, sid, 0, "screen", 24, 0)
		if err != nil {
			t.Fatalf("poll upgraded session: %v", err)
		}
		if !strings.Contains(view.Screen, "READY_LONG") {
			t.Errorf("already-captured output missing from screen: %q", view.Screen)
		}
		if view.State != SessionRunning {
			t.Errorf("state = %s, want running", view.State)
		}
		_ = reg.Close("bot-real", sid)
	})

	t.Run("zero-output alive is never upgraded", func(t *testing.T) {
		// Hidden startup failure shape (§3.2): alive, no bytes on screen.
		out, err := runAutoUpgrade(context.Background(), reg, "bot-real",
			SpawnRequest{
				Command: "sleep 30",
				Cwd:     root, Background: true,
			}, cfg)
		if err != nil {
			t.Fatalf("runAutoUpgrade: %v", err)
		}
		if out["upgraded"] != false || out["reason"] != autoReasonNoOutput {
			t.Fatalf("zero-output alive must not upgrade: %#v", out)
		}
		if reg.Count("bot-real") != 0 {
			t.Error("rejected session must be closed, not left as an orphan")
		}
	})
}

func TestAutoUpgrade_RealPTY_WorkdirIsHonoured(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.AutoUpgradeWindow = 100 * time.Millisecond
	cfg.AutoUpgradeMaxWait = 400 * time.Millisecond
	root := t.TempDir()
	sub := filepath.Join(root, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	spawner := newLocalSessionSpawner(cfg, nil, root)
	reg := NewSessionRegistry(cfg, nil, spawner)
	defer reg.CloseAll("bot-cwd")
	out, err := runAutoUpgrade(context.Background(), reg, "bot-cwd",
		SpawnRequest{
			Command: "printf 'CWD_MARKER\\n'",
			Cwd:     sub, Background: true,
		}, cfg)
	if err != nil {
		t.Fatalf("runAutoUpgrade: %v", err)
	}
	if out["workdir"] != sub {
		t.Errorf("workdir = %v, want %v", out["workdir"], sub)
	}
	if !strings.Contains(out["stdout"].(string), "CWD_MARKER") {
		t.Errorf("command did not run in nested cwd: %v", out["stdout"])
	}
}
