//go:build docker_integration

// Docker 集成测试 — 需要真实 Docker daemon。
// 运行方式（交叉编译后在 WSL/Linux 执行）：
//
//	go test -c -tags docker_integration -o /tmp/docker_test ./sandbox/
//	/tmp/docker_test -test.v -test.run TestDockerIntegration
package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestDockerSandbox(t *testing.T) (Sandbox, func()) {
	t.Helper()
	sb, err := newDockerSandbox(Config{
		Backend:         "docker",
		Image:           "alpine:latest",
		MemoryLimit:     "256m",
		CPULimit:        "0.5",
		NetworkDisabled: true,
		Timeout:         15 * time.Second,
		MaxOutput:       64 * 1024,
		MaxFileWrite:    1 << 20,
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("failed to create docker sandbox: %v", err)
	}
	return sb, func() { _ = sb.Close() }
}

func TestDockerIntegration_Backend(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()
	if sb.Backend() != "docker" {
		t.Errorf("Backend = %q, want docker", sb.Backend())
	}
}

func TestDockerIntegration_CreateAndClose(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-create-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if ws.ID() != "di-create-1" {
		t.Errorf("ID = %q, want di-create-1", ws.ID())
	}
	if ws.WorkDir() != "/workspace" {
		t.Errorf("WorkDir = %q, want /workspace", ws.WorkDir())
	}
	if err := ws.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestDockerIntegration_ExecEcho(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-exec-echo")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "echo 'hello docker'",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello docker") {
		t.Errorf("Stdout = %q, should contain 'hello docker'", result.Stdout)
	}
}

func TestDockerIntegration_ExecNonZero(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-exec-exit")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "exit 7",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", result.ExitCode)
	}
}

func TestDockerIntegration_ExecTimeout(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-exec-timeout")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "sleep 30",
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (timeout)", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "timed out") {
		t.Errorf("Stderr = %q, should contain 'timed out'", result.Stderr)
	}
}

func TestDockerIntegration_WriteAndReadFile(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-io")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	data := []byte("hello from docker test\nsecond line")
	err = ws.WriteFile(context.Background(), "sub/dir/test.txt", data)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := ws.ReadFile(context.Background(), "sub/dir/test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(data))
	}
}

func TestDockerIntegration_WriteFileWithSpaces(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-spaces")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	data := []byte("file with spaces in name")
	err = ws.WriteFile(context.Background(), "my file.txt", data)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := ws.ReadFile(context.Background(), "my file.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(data))
	}
}

func TestDockerIntegration_WriteFileWithSpecialChars(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-special")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	data := []byte("special chars in name")
	// 含单引号的路径名
	err = ws.WriteFile(context.Background(), "it's a file.txt", data)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := ws.ReadFile(context.Background(), "it's a file.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(data))
	}
}

func TestDockerIntegration_WriteUTF8(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-utf8")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	data := []byte("你好世界！こんにちは！🎮🎮🎮")
	err = ws.WriteFile(context.Background(), "unicode.txt", data)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := ws.ReadFile(context.Background(), "unicode.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(data))
	}
}

func TestDockerIntegration_ListDir(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-list")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	ws.WriteFile(context.Background(), "file1.txt", []byte("a"))
	ws.WriteFile(context.Background(), "file2.txt", []byte("bb"))
	ws.WriteFile(context.Background(), "sub/file3.txt", []byte("ccc"))

	entries, err := ws.ListDir(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d: %+v", len(entries), entries)
	}

	// 验证 sub 目录存在
	foundSub := false
	for _, e := range entries {
		if e.Name == "sub" && e.IsDir {
			foundSub = true
		}
	}
	if !foundSub {
		t.Error("expected 'sub' directory in listing")
	}
}

func TestDockerIntegration_PathTraversal(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-traversal")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	_, err = ws.ReadFile(context.Background(), "../etc/passwd")
	if err == nil {
		t.Error("expected error reading ../etc/passwd")
	}

	err = ws.WriteFile(context.Background(), "../evil.txt", []byte("bad"))
	if err == nil {
		t.Error("expected error writing ../evil.txt")
	}
}

func TestDockerIntegration_NetworkDisabled(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-network")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	// 网络被禁用，ping 应该失败
	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "ping -c 1 -W 2 8.8.8.8 2>&1 || echo 'network blocked'",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "network blocked") {
		t.Errorf("expected network to be blocked, stdout = %q", result.Stdout)
	}
}

func TestDockerIntegration_ResourceLimit(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	ws, err := sb.Create("di-resource")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer ws.Close()

	// 检查内存限制是否生效
	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || echo 'unknown'",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	t.Logf("memory limit: %s", strings.TrimSpace(result.Stdout))
}

func TestDockerIntegration_FullLifecycle(t *testing.T) {
	sb, cleanup := newTestDockerSandbox(t)
	defer cleanup()

	// 完整生命周期：create → write → exec → read → list → close
	ws, err := sb.Create("di-lifecycle")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// 写文件
	err = ws.WriteFile(context.Background(), "main.py", []byte("print('hello from python')"))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// 执行命令
	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "cat main.py && echo '---' && wc -l main.py",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello from python") {
		t.Errorf("stdout should contain file content, got: %q", result.Stdout)
	}

	// 读取
	_, err = ws.ReadFile(context.Background(), "main.py")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	// 列目录
	entries, err := ws.ListDir(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(entries) < 1 {
		t.Errorf("expected at least 1 entry, got %d", len(entries))
	}

	// 关闭
	if err := ws.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
