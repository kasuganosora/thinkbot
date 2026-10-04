package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if ConfineIfRequested() {
		return
	}
	os.Exit(m.Run())
}

func TestLocalGuardWriteLimits(t *testing.T) {
	root := t.TempDir()
	g := localGuard{project: "proj"}
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.check(root, "proj/ok.txt", true); err != nil {
		t.Fatal(err)
	}
	if err := g.check(root, "elsewhere.txt", true); err == nil {
		t.Fatal("write outside the project should fail")
	}
	if err := g.check(root, "proj/.git/config", true); err == nil {
		t.Fatal(".git write should fail")
	}
	if err := g.check(root, "proj/.git/config", false); err != nil {
		t.Fatal(".git read should stay allowed")
	}
	if err := g.check(root, ".ssh/id_ed25519", false); err == nil {
		t.Fatal("credential read should fail")
	}
}

func TestLocalOfflineHasNoNetwork(t *testing.T) {
	dir := t.TempDir()
	sb, err := newLocalSandbox(Config{Backend: "local", BaseDir: dir, LocalOffline: true, Timeout: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := sb.Create("bot")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	res, err := ws.Exec(context.Background(), ExecRequest{Command: "python3 -c 'import urllib.request; urllib.request.urlopen(\"http://example.com\", timeout=3)'"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("offline command reached the network: %#v", res)
	}
}

func TestLocalExecCannotEscapeProjectOrSecrets(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bot")
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := "super-secret-key"
	if err := os.WriteFile(filepath.Join(root, ".ssh", "id"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proj", ".git", "config"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := newLocalSandbox(Config{
		Backend: "local", BaseDir: dir, LocalProject: "proj", LocalOffline: true, Timeout: 15 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := sb.Create("bot")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ctx := context.Background()
	res, err := ws.Exec(ctx, ExecRequest{Command: "cat .ssh/id; echo; echo hack > proj/.git/config; echo hack > outside.txt; echo ok > proj/a.txt; umount proj/.git; echo hacked > proj/.git/config"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, secret) || strings.Contains(res.Stderr, secret) {
		t.Fatalf("secret leaked: %#v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "proj", "a.txt")); err != nil {
		t.Fatalf("project write should work: %#v err=%v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "outside.txt")); err == nil {
		t.Fatal("write escaped the project")
	}
	git, _ := os.ReadFile(filepath.Join(root, "proj", ".git", "config"))
	if strings.Contains(string(git), "hack") {
		t.Fatalf(".git was writable: %q cmd=%#v", git, res)
	}
}

func TestTermSessionReattachAndIdle(t *testing.T) {
	h := NewTermHub(time.Minute)
	now := time.Now()
	h.now = func() time.Time { return now }
	s, expired := h.Attach("bot", "", "/data")
	if expired || s.ID == "" {
		t.Fatalf("first attach: %+v expired=%v", s, expired)
	}
	again, expired := h.Attach("bot", s.ID, "")
	if expired || again.ID != s.ID {
		t.Fatalf("reattach: %+v expired=%v", again, expired)
	}
	other, expired := h.Attach("other-bot", s.ID, "")
	if !expired || other.ID == s.ID {
		t.Fatalf("session leaked across bots: %+v expired=%v", other, expired)
	}
	now = now.Add(2 * time.Minute)
	_, expired = h.Attach("bot", s.ID, "/data")
	if !expired {
		t.Fatal("idle session should not reattach")
	}
	if !DescribeDesktop().Available {
		t.Fatal("desktop stream should be part of this build")
	}
	if DescribeDesktop().Surface == "" {
		t.Fatal(DescribeDesktop().Reason)
	}
}
