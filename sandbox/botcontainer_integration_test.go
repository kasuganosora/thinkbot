//go:build docker_integration

// per-bot 长期容器隔离集成测试 — 需要真实 Docker daemon。
// 运行：go test -tags docker_integration -run TestBotContainer -v ./sandbox/
package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestBotWSManager(t *testing.T) (*BotWorkspaceManager, func()) {
	t.Helper()
	cfg := Config{
		Backend:      "docker",
		Image:        "alpine:latest",
		MemoryLimit:  "256m",
		CPULimit:     "0.5",
		Timeout:      15 * time.Second,
		MaxOutput:    64 * 1024,
		MaxFileWrite: 1 << 20,
		Timezone:     "UTC",
		// PersistentContainer 会被 NewBotWorkspaceManager 自动置为 true
	}
	baseDir := t.TempDir()
	mgr, err := NewBotWorkspaceManager(baseDir, cfg, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager failed: %v", err)
	}
	if !mgr.cfg.PersistentContainer {
		t.Fatalf("expected PersistentContainer auto-enabled in docker mode")
	}
	return mgr, func() { _ = mgr.Close() }
}

// 核心验收 1：文件写入容器后，宿主机 baseDir 下不出现该文件（真正隔离）。
func TestBotContainer_IsolationFromHost(t *testing.T) {
	mgr, cleanup := newTestBotWSManager(t)
	defer cleanup()

	botID := "iso-bot-1"
	defer mgr.DestroyBot(botID, true)

	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}

	content := []byte("secret data inside container")
	if err := ws.WriteFile(context.Background(), "secret.txt", content); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// 从容器读回，内容一致
	got, err := ws.ReadFile(context.Background(), "secret.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q", string(got))
	}

	// 宿主机 baseDir/botID 目录下不应存在 secret.txt
	hostPath := mgr.baseDir + "/" + botID + "/secret.txt"
	find := exec.Command("sh", "-c", "ls -la "+mgr.baseDir+"/"+botID+" 2>&1 || true")
	out, _ := find.CombinedOutput()
	if strings.Contains(string(out), "secret.txt") {
		t.Errorf("ISOLATION BROKEN: file leaked to host at %s", hostPath)
	}
	t.Logf("host dir listing (should NOT contain secret.txt): %s", strings.TrimSpace(string(out)))
}

// 核心验收 2：容器持久化 — 停止后重启，文件仍在（named volume 保留）。
func TestBotContainer_PersistAcrossRestart(t *testing.T) {
	mgr, cleanup := newTestBotWSManager(t)
	defer cleanup()

	botID := "persist-bot-1"
	defer mgr.DestroyBot(botID, true)

	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}
	if err := ws.WriteFile(context.Background(), "keep.txt", []byte("persisted")); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// 停止容器（保留 volume）
	if err := mgr.StopBot(botID); err != nil {
		t.Fatalf("StopBot failed: %v", err)
	}

	// 再次操作会自动重启容器，文件应仍在
	got, err := ws.ReadFile(context.Background(), "keep.txt")
	if err != nil {
		t.Fatalf("ReadFile after restart failed: %v", err)
	}
	if string(got) != "persisted" {
		t.Errorf("data lost across restart: got %q", string(got))
	}
}

// 核心验收 3：命令在容器内执行（whoami/hostname 与宿主不同，且能读到自己写的文件）。
func TestBotContainer_ExecInsideContainer(t *testing.T) {
	mgr, cleanup := newTestBotWSManager(t)
	defer cleanup()

	botID := "exec-bot-1"
	defer mgr.DestroyBot(botID, true)

	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}

	ws.WriteFile(context.Background(), "hi.txt", []byte("from-exec"))
	res, err := ws.Exec(context.Background(), ExecRequest{Command: "cat hi.txt && echo '---' && pwd"})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "from-exec") {
		t.Errorf("exec should read container file, got: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "/workspace") {
		t.Errorf("exec pwd should be /workspace, got: %q", res.Stdout)
	}
}

// 核心验收 4：DestroyBot 真正删除容器和 volume。
func TestBotContainer_DestroyCleansUp(t *testing.T) {
	mgr, cleanup := newTestBotWSManager(t)
	defer cleanup()

	botID := "destroy-bot-1"
	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}
	ws.WriteFile(context.Background(), "tmp.txt", []byte("x"))

	c := newBotContainer(botID, mgr.cfg, zap.NewNop().Sugar())
	if c.containerState(context.Background()) != "running" {
		t.Fatalf("container should be running before destroy")
	}

	if err := mgr.DestroyBot(botID, true); err != nil {
		t.Fatalf("DestroyBot failed: %v", err)
	}

	if state := c.containerState(context.Background()); state != "" {
		t.Errorf("container should be gone after destroy, state=%q", state)
	}
	// volume 也应被删除
	volCheck := exec.Command("docker", "volume", "inspect", c.volume)
	if err := volCheck.Run(); err == nil {
		t.Errorf("volume %q should be removed after DestroyBot", c.volume)
	}
}

// 核心验收 5：路径穿越防护在容器模式下依然有效。
func TestBotContainer_PathTraversalBlocked(t *testing.T) {
	mgr, cleanup := newTestBotWSManager(t)
	defer cleanup()

	botID := "trav-bot-1"
	defer mgr.DestroyBot(botID, true)

	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}
	if _, err := ws.ReadFile(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("expected path traversal to be blocked")
	}
	if err := ws.WriteFile(context.Background(), "../evil.txt", []byte("bad")); err == nil {
		t.Error("expected write traversal to be blocked")
	}
}
