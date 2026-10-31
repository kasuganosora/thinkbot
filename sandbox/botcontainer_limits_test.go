package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// TestUpdateLimitsRejectsClearing 守住「不限制不能在线生效」这件事。
//
// docker update 无法取消已有上限：--memory 0 / --cpus 0 都被当成"不改这个字段"，
// -1 不是合法值（实测确认）。所以只能在重建容器时才真正不限。
// 这里必须在碰 docker 之前就返回，否则测试会真的去调 docker。
func TestUpdateLimitsRejectsClearing(t *testing.T) {
	fakeLimitDocker(t, "1000000000 2147483648")
	c := newBotContainer("bot", Config{}, zap.NewNop().Sugar())

	if err := c.UpdateCPULimit(context.Background(), ""); !errors.Is(err, ErrLimitNotClearable) {
		t.Errorf("UpdateCPULimit(\"\") = %v, want ErrLimitNotClearable", err)
	}
	if err := c.UpdateMemoryLimit(context.Background(), 0); !errors.Is(err, ErrLimitNotClearable) {
		t.Errorf("UpdateMemoryLimit(0) = %v, want ErrLimitNotClearable", err)
	}
	if err := c.UpdateMemoryLimit(context.Background(), -1); !errors.Is(err, ErrLimitNotClearable) {
		t.Errorf("UpdateMemoryLimit(-1) = %v, want ErrLimitNotClearable", err)
	}
}

// TestApplyLimitsNoopWithoutDockerBackend 保证非 docker 后端下这两个入口是空操作，
// 不会去 exec docker（local 部署的机器上没有 docker 也照样能用面板保存配置）。
func TestApplyLimitsNoopWithoutDockerBackend(t *testing.T) {
	m, err := NewBotWorkspaceManager(t.TempDir(), Config{Backend: "local"}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager: %v", err)
	}
	if m.backend != "local" {
		t.Fatalf("backend = %q, want local", m.backend)
	}
	if err := m.ApplyCPULimit(context.Background(), "bot", "16"); err != nil {
		t.Errorf("ApplyCPULimit on local backend = %v, want nil", err)
	}
	if err := m.ApplyMemoryLimit(context.Background(), "bot", 2048); err != nil {
		t.Errorf("ApplyMemoryLimit on local backend = %v, want nil", err)
	}
}

// Only a fake executable is used; no test mutates host containers.
func fakeLimitDocker(t *testing.T, limits string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = inspect ]; then echo '" + limits + "'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestApplyUnlimitedRetainsDesiredLimitsForRecreate(t *testing.T) {
	fakeLimitDocker(t, "1000000000 2147483648")
	c := newBotContainer("bot", DefaultConfig(), zap.NewNop().Sugar())
	m := &BotWorkspaceManager{backend: "docker", cfg: DefaultConfig(), logger: zap.NewNop().Sugar(),
		workspaces: map[string]*botWorkspace{"bot": {container: c}}, memoryOverrides: map[string]int64{}}
	if err := m.ApplyCPULimit(context.Background(), "bot", ""); !errors.Is(err, ErrLimitNotClearable) {
		t.Fatal(err)
	}
	if m.cfg.CPULimit != "" || c.cfg.CPULimit != "" {
		t.Fatal("recreate would retain old CPU limit")
	}
	if err := m.ApplyMemoryLimit(context.Background(), "bot", 0); !errors.Is(err, ErrLimitNotClearable) {
		t.Fatal(err)
	}
	if c.effectiveMemoryLocked() != "" {
		t.Fatal("recreate would retain old memory limit")
	}
	if mb, ok := m.memoryOverrides["bot"]; !ok || mb != 0 {
		t.Fatal("manager lost unlimited setting")
	}
}

func TestUpdateLimitsDistinguishesMissingFromInspectionFailure(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wantErr      bool
	}{
		{"missing", "echo 'Error: No such object: bot' >&2; exit 1", false},
		{"daemon unavailable", "echo 'Cannot connect to the Docker daemon' >&2; exit 1", true},
		{"permission denied", "echo 'permission denied' >&2; exit 1", true},
		{"malformed", "echo 'not limits'", true},
		{"partial", "echo '0'", true},
		{"unlimited", "echo '0 0'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			c := newBotContainer("bot", DefaultConfig(), zap.NewNop().Sugar())
			if err := c.UpdateCPULimit(context.Background(), ""); (err != nil) != tc.wantErr {
				t.Fatalf("cpu err=%v", err)
			}
			if err := c.UpdateMemoryLimit(context.Background(), 0); (err != nil) != tc.wantErr {
				t.Fatalf("memory err=%v", err)
			}
		})
	}
}

func TestFailedLiveUpdatesRetainDesiredLimitsForRecreation(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"inspect fails", "echo 'Cannot connect to the Docker daemon' >&2; exit 1"},
		{"update fails", "if [ \"$1\" = inspect ]; then echo '1000000000 2147483648'; exit 0; fi; echo 'update rejected' >&2; exit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			c := newBotContainer("bot", DefaultConfig(), zap.NewNop().Sugar())
			m := &BotWorkspaceManager{backend: "docker", cfg: DefaultConfig(), logger: zap.NewNop().Sugar(),
				workspaces: map[string]*botWorkspace{"bot": {container: c}}, memoryOverrides: map[string]int64{}}
			if err := m.ApplyCPULimit(context.Background(), "bot", "4"); err == nil {
				t.Fatal("failed CPU update reported success")
			}
			if m.cfg.CPULimit != "4" || c.cfg.CPULimit != "4" {
				t.Fatal("recreation would use stale CPU limit")
			}
			if err := m.ApplyMemoryLimit(context.Background(), "bot", 4096); err == nil {
				t.Fatal("failed memory update reported success")
			}
			if c.effectiveMemoryLocked() != "4096m" || m.getBotMemoryLimitLocked("bot") != "4096m" {
				t.Fatal("recreation would use stale memory limit")
			}
			// Replacing the handle must keep the same desired values as reusing it.
			fresh := newBotContainer("bot", m.cfg, zap.NewNop().Sugar())
			fresh.SetMemoryOverride(m.getBotMemoryLimitLocked("bot"))
			if fresh.cfg.CPULimit != "4" || fresh.effectiveMemoryLocked() != "4096m" {
				t.Fatal("fresh handle lost desired limits")
			}
		})
	}
}
