package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	now = now.Add(2 * time.Minute)
	_, expired = h.Attach("bot", s.ID, "/data")
	if !expired {
		t.Fatal("idle session should not reattach")
	}
	if DescribeDesktop().Available {
		t.Fatal("desktop stream is not bundled")
	}
	if !strings.Contains(DescribeDesktop().Reason, "VNC") {
		t.Fatal(DescribeDesktop().Reason)
	}
}
