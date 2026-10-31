package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestChannelBinaryIsContainerPID1 needs a Docker daemon. It does not build the
// full Chromium sandbox image (that path is docker/sandbox.EnsureImage, which
// apt-installs a browser and is not part of the default unit run).
//
// What it checks: a scratch image whose ENTRYPOINT is the static
// thinkbot-channel binary stays up when `docker run` is given `sleep infinity`
// (those words must not become PID 1), `thinkbot-channel health` succeeds, and
// `docker stop` returns because PID 1 handles SIGTERM.
//
// The full sandbox Dockerfile (COPY, LABEL, HEALTHCHECK, exec-form ENTRYPOINT)
// is asserted without Docker by docker/sandbox.TestDockerfileChannelIsPID1.
func TestChannelBinaryIsContainerPID1(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not available")
	}

	bin := filepath.Join(t.TempDir(), "thinkbot-channel")
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Dir = moduleDir(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build channel: %v\n%s", err, out)
	}

	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(`FROM scratch
COPY thinkbot-channel /usr/local/bin/thinkbot-channel
ENTRYPOINT ["/usr/local/bin/thinkbot-channel"]
HEALTHCHECK --interval=5s --timeout=3s --retries=3 CMD ["/usr/local/bin/thinkbot-channel", "health"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bin, filepath.Join(ctxDir, "thinkbot-channel")); err != nil {
		// Rename across filesystems falls back to copy.
		data, rerr := os.ReadFile(bin)
		if rerr != nil {
			t.Fatal(err)
		}
		if werr := os.WriteFile(filepath.Join(ctxDir, "thinkbot-channel"), data, 0o755); werr != nil {
			t.Fatal(werr)
		}
	}

	tag := "thinkbot-channel-pid1-test"
	buildImg := exec.Command("docker", "build", "-t", tag, ctxDir)
	if out, err := buildImg.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rmi", "-f", tag).Run()
	})

	name := "tbch-pid1-" + strings.ReplaceAll(t.Name(), "/", "-")
	_ = exec.Command("docker", "rm", "-f", name).Run()
	run := exec.Command("docker", "run", "-d", "--name", name, tag, "sleep", "infinity")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	deadline := time.Now().Add(10 * time.Second)
	var healthOut []byte
	var healthErr error
	for time.Now().Before(deadline) {
		healthOut, healthErr = exec.Command("docker", "exec", name, "/usr/local/bin/thinkbot-channel", "health").CombinedOutput()
		if healthErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if healthErr != nil {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		t.Fatalf("health: %v\n%s\nlogs:\n%s", healthErr, healthOut, logs)
	}

	// docker top reports host PIDs. Compare with the container init's host PID,
	// and inspect the executable field; "sleep infinity" remains in os.Args even
	// when the channel binary correctly ignores those trailing arguments.
	initPID, err := exec.Command("docker", "inspect", "--format", "{{.State.Pid}}", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect init PID: %v\n%s", err, initPID)
	}
	top, err := exec.Command("docker", "top", name, "-o", "pid,args").CombinedOutput()
	if err != nil {
		t.Fatalf("docker top: %v\n%s", err, top)
	}
	foundInit := false
	for _, line := range strings.Split(string(top), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if filepath.Base(fields[1]) == "sleep" {
			t.Fatalf("sleep process is running:\n%s", top)
		}
		if fields[0] == strings.TrimSpace(string(initPID)) {
			foundInit = filepath.Base(fields[1]) == "thinkbot-channel"
		}
	}
	if !foundInit {
		t.Fatalf("container init is not thinkbot-channel (host PID %s):\n%s", strings.TrimSpace(string(initPID)), top)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stop := exec.CommandContext(stopCtx, "docker", "stop", "-t", "5", name)
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v\n%s", err, out)
	}
}

func moduleDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "cmd", "thinkbot-channel")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
