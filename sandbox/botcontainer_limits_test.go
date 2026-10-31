package sandbox

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// TestUpdateLimitsRejectsClearing 守住「不限制不能在线生效」这件事。
//
// docker update 无法取消已有上限：--memory 0 / --cpus 0 都被当成"不改这个字段"，
// -1 不是合法值（实测确认）。所以只能在重建容器时才真正不限。
// 这里必须在碰 docker 之前就返回，否则测试会真的去调 docker。
func TestUpdateLimitsRejectsClearing(t *testing.T) {
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
