package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindDockerDir_SkipsNonExecutable 验证候选目录里存在同名但不可执行的文件时不会误判。
func TestFindDockerDir_SkipsNonExecutable(t *testing.T) {
	tmp := t.TempDir()

	badDir := filepath.Join(tmp, "bad")
	goodDir := filepath.Join(tmp, "good")
	for _, d := range []string{badDir, goodDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// bad/docker 存在但没有执行位 → 必须跳过
	if err := os.WriteFile(filepath.Join(badDir, "docker"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write bad docker: %v", err)
	}
	// good/docker 可执行 → 应被选中
	if err := os.WriteFile(filepath.Join(goodDir, "docker"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write good docker: %v", err)
	}

	got := findDockerDir([]string{badDir, goodDir})
	if got != goodDir {
		t.Fatalf("findDockerDir = %q, want %q (non-executable candidate must be skipped)", got, goodDir)
	}
}

// TestFindDockerDir_SkipsDirectoryNamedDocker 验证名为 docker 的「目录」不会被当成可执行文件。
func TestFindDockerDir_SkipsDirectoryNamedDocker(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "docker"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := findDockerDir([]string{tmp}); got != "" {
		t.Fatalf("findDockerDir = %q, want \"\" (a directory must not match)", got)
	}
}

// TestFindDockerDir_NotFound 验证候选目录都不含 docker 时返回空。
func TestFindDockerDir_NotFound(t *testing.T) {
	if got := findDockerDir([]string{t.TempDir(), "", "/definitely/not/here"}); got != "" {
		t.Fatalf("findDockerDir = %q, want \"\"", got)
	}
}

// TestFindDockerDir_RespectsOrder 验证按候选顺序返回第一个命中（优先级语义）。
func TestFindDockerDir_RespectsOrder(t *testing.T) {
	tmp := t.TempDir()
	first := filepath.Join(tmp, "first")
	second := filepath.Join(tmp, "second")
	for _, d := range []string{first, second} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(d, "docker"), []byte("x"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := findDockerDir([]string{first, second}); got != first {
		t.Fatalf("findDockerDir = %q, want %q", got, first)
	}
}

// TestDockerSearchDirs_HonorsEnvOverride 验证 THINKBOT_DOCKER_BIN_DIR 拥有最高优先级。
//
// 这是「docker 装在非标准位置」时的逃生舱，必须排在所有内置候选之前。
func TestDockerSearchDirs_HonorsEnvOverride(t *testing.T) {
	t.Setenv(EnvDockerBinDir, "/custom/docker/bin")
	dirs := dockerSearchDirs()
	if len(dirs) == 0 {
		t.Fatal("dockerSearchDirs returned empty")
	}
	if dirs[0] != "/custom/docker/bin" {
		t.Fatalf("dirs[0] = %q, want the env override to come first", dirs[0])
	}
}

// TestDockerSearchDirs_IncludesHomebrewOnDarwin 是本次事故的直接回归测试。
//
// 事故：launchd 启动时 PATH 被裁剪为 /usr/bin:/bin:/usr/sbin:/sbin，不含
// /opt/homebrew/bin，导致 dockerAvailable() 判假、沙箱静默降级为 local。
// 因此 darwin 上的候选目录必须覆盖 Homebrew 路径。
func TestDockerSearchDirs_IncludesHomebrewOnDarwin(t *testing.T) {
	_ = os.Unsetenv(EnvDockerBinDir)
	dirs := dockerSearchDirs()

	want := map[string]bool{
		"/opt/homebrew/bin": false,
		"/usr/local/bin":    false,
	}
	for _, d := range dirs {
		if _, ok := want[d]; ok {
			want[d] = true
		}
	}
	for d, found := range want {
		if !found {
			t.Errorf("dockerSearchDirs missing %q; launchd-trimmed PATH would silently fall back to local sandbox", d)
		}
	}
}

// TestEnsureDockerPath_Idempotent 验证重复调用不会反复污染 PATH。
func TestEnsureDockerPath_Idempotent(t *testing.T) {
	before := os.Getenv("PATH")
	a := ensureDockerPath()
	mid := os.Getenv("PATH")
	b := ensureDockerPath()
	after := os.Getenv("PATH")

	if a != b {
		t.Errorf("ensureDockerPath not stable: %q then %q", a, b)
	}
	if mid != after {
		t.Errorf("PATH mutated on second call: %q -> %q", mid, after)
	}
	// 若本机 docker 本就在 PATH 中，则不应做任何修改。
	if a == "" && before != after {
		t.Errorf("PATH modified despite no dir resolved: %q -> %q", before, after)
	}
}
