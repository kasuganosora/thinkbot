//go:build docker_integration

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/util/idgen"
	"go.uber.org/zap"
)

func TestDockerExecCancellationIsScoped(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()
	ws, err := sb.Create("cancel-scoped")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	assertExecCancellationIsScoped(t, ws)
}

func TestBotContainerExecCancellationIsScoped(t *testing.T) {
	mgr, cleanup := newTestBotWSManager(t)
	defer cleanup()
	const botID = "cancel-scoped-bot"
	t.Cleanup(func() { _ = mgr.DestroyBot(botID, true) })
	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatal(err)
	}
	assertExecCancellationIsScoped(t, ws)
}

func TestLegacyDockerExecCancellationIsScoped(t *testing.T) {
	// This legacy path gives each command a different container sharing files.
	ws := &botWorkspace{root: t.TempDir(), backend: "docker", logger: zap.NewNop().Sugar(), cfg: Config{
		Image: "alpine:latest", Timeout: 15 * time.Second, MaxOutput: 64 * 1024,
	}}
	assertExecCancellationIsScoped(t, ws)
}

func TestDockerExecCancellationBeforeLeaderStarts(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()
	ws, err := sb.Create("cancel-before-start")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	dockerWS := ws.(*dockerWorkspace)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, stop, err := dockerExecCommand(ctx, dockerWS.container, dockerWS.workDir, "echo escaped > late-start.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("late start must decline cancelled work: %s", output)
	}
	if data, err := ws.ReadFile(ctx, "late-start.txt"); err == nil {
		t.Fatalf("late-started command executed after cancellation: %q", data)
	}
}

func TestDockerExecPreservesStdinAndExitCode(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()
	ws, err := sb.Create("cancel-wrapper-io")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	dockerWS := ws.(*dockerWorkspace)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, stop, err := dockerExecCommand(ctx, dockerWS.container, dockerWS.workDir, "cat; exit 7", true)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runCommandWithStreaming(ctx, cancel, cmd, 1024, []byte("input data\n"), 10*time.Second, 20*time.Second, nil, stop)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "input data\n" || res.ExitCode != 7 {
		t.Fatalf("wrapper changed IO or exit: %+v", res)
	}
	// A completed command must not leave its control directory behind.
	if out, err := exec.CommandContext(ctx, "docker", "exec", dockerWS.container, "sh", "-c", "find /tmp -maxdepth 1 -name 'thinkbot-exec-*'").CombinedOutput(); err != nil || len(out) > 0 {
		t.Fatalf("control directory leaked after completion: %s %v", out, err)
	}
	if entries, err := ws.ListDir(ctx, "/"); err != nil || len(entries) != 0 {
		t.Fatalf("empty directory must succeed: %+v %v", entries, err)
	}
	if _, err := ws.ListDir(ctx, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory must return a typed error: %v", err)
	}
	if err := ws.WriteFile(ctx, "file", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ListDir(ctx, "file"); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("non-directory must return a typed error: %v", err)
	}
}

func TestDockerConcurrentCreateRejectsForeignContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := newBotContainer(idgen.New("foreign-collision"), Config{Image: "alpine:latest"}, zap.NewNop().Sugar())
	// Even matching image/name/volume is insufficient without the exact managed
	// labels. A collision must never commandeer another application's container.
	if out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", c.container,
		"-v", c.volume+":"+containerWorkDir, "alpine:latest", "sleep", "infinity").CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", c.container).Run()
		_ = exec.Command("docker", "volume", "rm", c.volume).Run()
	})
	if err := c.create(ctx); err == nil || c.ready {
		t.Fatal("foreign container accepted after name conflict")
	}
}

func TestDockerExecCancellationAfterShellExits(t *testing.T) {
	mgr, cleanup := newTestBotWSManager(t)
	defer cleanup()
	botID := idgen.New("background-cancel")
	t.Cleanup(func() { _ = mgr.DestroyBot(botID, true) })
	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stopCtx, stop := context.WithCancel(ctx)
	defer stop()
	res, err := ws.(StreamWorkspace).ExecStream(stopCtx, ExecRequest{
		Command: "(sleep 0.4; echo orphan-ready; sleep 1.6; echo escaped > orphan.txt) &",
	}, func(c ExecChunk) {
		if strings.Contains(c.Data, "orphan-ready") {
			stop()
		}
	})
	if err != nil || !res.Aborted || res.Reliable {
		t.Fatalf("background cancellation: %+v %v", res, err)
	}
	check, err := ws.Exec(ctx, ExecRequest{Command: "sleep 2; test ! -e orphan.txt"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("background descendant escaped cancellation: %+v %v", check, err)
	}
	// Explicitly redirecting output still allows a process to detach. Waiting
	// for every process in the group would unexpectedly hang server startup.
	detachedCtx, detachedCancel := context.WithTimeout(ctx, 8*time.Second)
	defer detachedCancel()
	detached, err := ws.Exec(detachedCtx, ExecRequest{Command: "(while [ ! -e release.txt ]; do sleep 0.1; done; echo detached > detached.txt) >/dev/null 2>&1 &"})
	if err != nil || detached.ExitCode != 0 || detached.Aborted {
		t.Fatalf("detached job blocked exec: %+v %v", detached, err)
	}
	// This cannot complete if Exec is still waiting for the detached job.
	if err := ws.WriteFile(ctx, "release.txt", []byte("release")); err != nil {
		t.Fatal(err)
	}
}
