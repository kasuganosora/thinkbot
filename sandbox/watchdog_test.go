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
//
// 同时断言**及时性**：硬上限判定与卡死判定共用同一个 tick 循环，若轮询间隔
// 只按 stuck 推导（这里 stuck=30s），1s 的硬上限会滞后到 5s 才生效。
// 修复前本用例确实耗时 ~5s，"通过"只是因为没有断言耗时。
func TestRunCommandWithStreaming_HardCeiling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 每 0.1s 输出一次、永不结束；硬上限 1s → 应在 ~1s 被强杀（reason=hard）。
	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", "while true; do echo tick; sleep 0.1; done")
	res, err := runCommandWithStreaming(ctx, cancel, cmd, 0, nil, 30*time.Second, 1*time.Second, nil)
	elapsed := time.Since(start)

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
	// 上界给到 3s：硬上限 1s + 首个 tick(500ms) + 进程收尾与调度抖动
	const maxAcceptable = 3 * time.Second
	if elapsed > maxAcceptable {
		t.Errorf("hard ceiling took %s with a 1s limit (want < %s); "+
			"the watchdog tick likely ignores the hard timeout", elapsed, maxAcceptable)
	}
}

// TestRunCommandWithStreaming_ShortStuckIsTimely 验证短卡死阈值**及时**生效。
//
// 这是防回归测试，针对一个真实缺陷：看门狗原先固定 5s 轮询，而卡死阈值可由
// LLM 工具入参（`stuck_timeout`）传成 1s——首次唤醒已在 5s 后，短阈值统统
// 退化成约 5s 粒度。
//
// 上面的 StuckKilled 用例没能暴露它，纯属巧合：那里的命令是 `sleep 5`，恰好
// 活到第一个 5s tick 才被抓到（修复前实测耗时 5.2s，而阈值只有 1s）。
// 若当初写 `sleep 3`，测试就会失败。
//
// 本用例通过**断言耗时上界**来锁死及时性：命令 sleep 10、阈值 1s，
// 必须在远早于 5s 时被判定。只断言"被杀"是不够的——那正是原用例漏掉它的原因。
func TestRunCommandWithStreaming_ShortStuckIsTimely(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const stuck = 1 * time.Second

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 10")
	res, err := runCommandWithStreaming(ctx, cancel, cmd, 0, nil, stuck, 30*time.Second, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.Aborted {
		t.Fatal("silent command must be aborted by the stuck watchdog")
	}

	// 上界给到 3s：阈值 1s + 首个 tick(500ms) + 进程收尾与调度抖动。
	// 修复前这里会是 ~5s（受固定 tick 支配）而超出上界。
	const maxAcceptable = 3 * time.Second
	if elapsed > maxAcceptable {
		t.Errorf("stuck detection took %s with a %s threshold (want < %s); "+
			"the watchdog tick is likely no longer derived from the threshold",
			elapsed, stuck, maxAcceptable)
	}
}
