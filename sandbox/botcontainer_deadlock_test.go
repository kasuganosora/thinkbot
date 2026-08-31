package sandbox

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestBotContainerEnsureNoDeadlock 验证 ensure→create→effectiveMemoryLocked
// 路径不再发生 sync.Mutex 重入死锁（修复回归用例）。
func TestBotContainerEnsureNoDeadlock(t *testing.T) {
	logger := zap.NewNop().Sugar()
	botID := "testdeadlockfix"
	cfg := Config{
		Image:       "alpine:latest",
		MemoryLimit: "2g",
		Timezone:    "Asia/Shanghai",
		Timeout:     30 * time.Second,
	}

	c := newBotContainer(botID, cfg, logger)

	// 设置 per-bot 内存覆盖，覆盖 create() 内部 effectiveMemoryLocked 的读取路径。
	c.SetMemoryOverride("512m")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// 清理测试容器与 volume，避免遗留。
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", c.container).Run()
		_ = exec.Command("docker", "volume", "rm", "-f", c.volume).Run()
	})

	t.Log("calling ensure (expect create -> effectiveMemoryLocked, no deadlock)")
	if err := c.ensure(ctx); err != nil {
		t.Fatalf("ensure returned error (possible deadlock/timeout): %v", err)
	}
	if !c.ready {
		t.Fatalf("container not marked ready after ensure")
	}
	t.Log("ensure succeeded without deadlock")
}
