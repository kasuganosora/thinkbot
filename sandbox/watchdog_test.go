package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunCommandWithStreaming_StuckKilled 验证：连续无输出超过卡死阈值的命令
// 被看门狗判定为「卡死」并终止（ExitCode=-1、Aborted、含卡死告警）。
// 这是「区分编译慢与死锁卡死」的核心：沉默的 sleep 不算正常慢，必须被杀。
func TestRunCommandWithStreaming_StuckKilled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 沉默命令：sleep 5，卡死阈值 1s。
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 5")
	res, err := runCommandWithStreaming(ctx, cancel, cmd, 0, nil, 1*time.Second, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("want ExitCode -1 (killed), got %d", res.ExitCode)
	}
	if !res.Aborted {
		t.Error("want Aborted true")
	}
	if !containsWarning(res.Warnings, "stuck watchdog") {
		t.Errorf("want stuck warning, got %v", res.Warnings)
	}
}

// TestRunCommandWithStreaming_OutputHeartbeatSurvives 验证：持续有输出（哪怕缓慢）
// 的命令不会被卡死看门狗误杀，能正常跑完（ExitCode=0、未 Aborted）。
// 每 0.3s 输出一次 tick，共 6 次（约 1.8s），卡死阈值 1s——输出间隔 < 阈值，应存活。
func TestRunCommandWithStreaming_OutputHeartbeatSurvives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c",
		"for i in 1 2 3 4 5 6; do echo tick; sleep 0.3; done")
	res, err := runCommandWithStreaming(ctx, cancel, cmd, 0, nil, 1*time.Second, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("want ExitCode 0 (completed), got %d (aborted=%v)", res.ExitCode, res.Aborted)
	}
	if res.Aborted {
		t.Error("slow-but-outputting command must NOT be aborted")
	}
	if strings.Count(res.Stdout, "tick") != 6 {
		t.Errorf("want 6 ticks in stdout, got %q", res.Stdout)
	}
}

// TestRunCommandWithStreaming_HardCeiling 验证：即便持续有输出，超过硬上限仍被强制终止。
func TestRunCommandWithStreaming_HardCeiling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 每 0.1s 输出一次、永不结束；硬上限 1s → 应在 ~1s 被强杀（reason=hard）。
	cmd := exec.CommandContext(ctx, "sh", "-c", "while true; do echo tick; sleep 0.1; done")
	res, err := runCommandWithStreaming(ctx, cancel, cmd, 0, nil, 30*time.Second, 1*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("want ExitCode -1 (hard ceiling), got %d", res.ExitCode)
	}
	if !res.Aborted {
		t.Error("want Aborted true")
	}
	if !containsWarning(res.Warnings, "hard time limit") {
		t.Errorf("want hard-ceiling warning, got %v", res.Warnings)
	}
}
