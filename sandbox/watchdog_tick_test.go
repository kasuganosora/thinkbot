package sandbox

import (
	"testing"
	"time"
)

// ============================================================================
// 卡死阈值与看门狗轮询间隔的推导（纯函数）
//
// 端到端行为验证见 watchdog_test.go；本文件只覆盖推导规则本身。
// ============================================================================

// TestWatchdogTickFor 锁定看门狗轮询间隔的推导规则。
//
// 这是**防回归测试**。原实现固定用 watchdogTick(5s)，而卡死阈值来自 LLM 工具
// 入参 `stuck_timeout`（sandbox/tools.go:232、:378 只校验 `> 0`，无下限保护）。
// 模型传 1 就得到 1s 阈值配 5s 轮询：首次唤醒已在 5s 后，用户显式要的
// 「1s 快速失败」完全失效，所有短阈值统统退化成约 5s 粒度。
//
// 硬上限也在同一个 tick 循环里判定，故间隔必须同时小于二者。
// 若有人把间隔改回固定常量、或只按其中一个阈值推导，本测试会失败。
func TestWatchdogTickFor(t *testing.T) {
	cases := []struct {
		name        string
		stuck, hard time.Duration
		want        time.Duration
	}{
		// 阈值较大：用上限，没必要频繁唤醒
		{"默认 5min → 5s 上限", defaultStuckTimeout, defaultStuckTimeout * hardTimeoutFactor, watchdogTick},
		{"30s → 5s 上限", 30 * time.Second, 90 * time.Second, watchdogTick},
		{"正好 10s → 5s（stuck/2 = 上限）", 10 * time.Second, 30 * time.Second, watchdogTick},

		// stuck 较窄：收紧到 stuck/2，保证一个窗口内至少醒两次
		{"stuck 8s → 4s", 8 * time.Second, 24 * time.Second, 4 * time.Second},
		{"stuck 2s → 1s", 2 * time.Second, 6 * time.Second, time.Second},
		{"stuck 1s → 500ms（工具入参可达）", time.Second, 3 * time.Second, 500 * time.Millisecond},

		// ↓ hard 比 stuck 更窄：必须按 hard 收紧，否则 1s 的硬上限会滞后到 5s。
		// TestRunCommandWithStreaming_HardCeiling 就是这个组合。
		{"hard 1s 远小于 stuck 30s → 按 hard 收紧", 30 * time.Second, time.Second, 500 * time.Millisecond},
		{"hard 4s 小于 stuck 60s", 60 * time.Second, 4 * time.Second, 2 * time.Second},

		// 阈值极小：收口到下限，避免空耗 CPU
		{"200ms → 100ms 下限", 200 * time.Millisecond, 600 * time.Millisecond, minWatchdogTick},
		{"1ms → 100ms 下限", time.Millisecond, 3 * time.Millisecond, minWatchdogTick},

		// 未设置（0）的阈值不参与收紧——0 表示调用方会回退为默认值
		{"两者皆 0 → 5s 上限", 0, 0, watchdogTick},
		{"stuck=0 时只看 hard", 0, 2 * time.Second, time.Second},
		{"hard=0 时只看 stuck", 2 * time.Second, 0, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := watchdogTickFor(c.stuck, c.hard)
			if got != c.want {
				t.Errorf("watchdogTickFor(stuck=%s, hard=%s): got %s, want %s",
					c.stuck, c.hard, got, c.want)
			}
			// 核心不变式：间隔必须严格小于每个已设置的阈值，
			// 否则可能整个判定窗口都没醒过。
			// 仅在阈值大于下限两倍时要求——更小的已明确牺牲精度换 CPU。
			for name, threshold := range map[string]time.Duration{"stuck": c.stuck, "hard": c.hard} {
				if threshold > minWatchdogTick*2 && got >= threshold {
					t.Errorf("tick %s must be < %s %s, otherwise detection can be missed",
						got, name, threshold)
				}
			}
		})
	}
}

// TestResolveExecTimeouts 锁定卡死阈值与硬上限的三级回退规则。
//
// 覆盖「工具入参传入极小阈值」这个真实可达的场景——它正是 watchdogTickFor
// 必须按阈值收紧间隔的原因。
func TestResolveExecTimeouts(t *testing.T) {
	cases := []struct {
		name      string
		req       ExecRequest
		cfg       Config
		wantStuck time.Duration
		wantHard  time.Duration
	}{
		{
			name:      "全空 → 默认阈值 + 联动硬上限",
			wantStuck: defaultStuckTimeout,
			wantHard:  defaultStuckTimeout * hardTimeoutFactor,
		},
		{
			name:      "cfg 提供阈值",
			cfg:       Config{StuckTimeout: 60 * time.Second},
			wantStuck: 60 * time.Second,
			wantHard:  60 * time.Second * hardTimeoutFactor,
		},
		{
			name:      "req 覆盖 cfg",
			req:       ExecRequest{StuckTimeout: 10 * time.Second},
			cfg:       Config{StuckTimeout: 60 * time.Second},
			wantStuck: 10 * time.Second,
			wantHard:  10 * time.Second * hardTimeoutFactor,
		},
		{
			name:      "显式硬上限不被联动值覆盖",
			req:       ExecRequest{StuckTimeout: 10 * time.Second, Timeout: 5 * time.Minute},
			wantStuck: 10 * time.Second,
			wantHard:  5 * time.Minute,
		},
		{
			// LLM 传 stuck_timeout:1 的真实路径——无下限保护，故必须能被正确处理
			name:      "工具入参 1s（无下限保护，真实可达）",
			req:       ExecRequest{StuckTimeout: time.Second},
			wantStuck: time.Second,
			wantHard:  time.Second * hardTimeoutFactor,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stuck, hard := resolveExecTimeouts(c.req, c.cfg)
			if stuck != c.wantStuck {
				t.Errorf("stuck: got %s, want %s", stuck, c.wantStuck)
			}
			if hard != c.wantHard {
				t.Errorf("hard: got %s, want %s", hard, c.wantHard)
			}
			// 不变式：硬上限不得小于卡死阈值，否则命令会在判定卡死前先被硬杀，
			// 「区分慢与卡死」这个设计目标就落空了
			if hard < stuck {
				t.Errorf("invariant violated: hard %s < stuck %s", hard, stuck)
			}
			// 间隔必须小于两个阈值（同 watchdogTickFor 的核心保证）
			tick := watchdogTickFor(stuck, hard)
			if stuck > minWatchdogTick*2 && tick >= stuck {
				t.Errorf("watchdog tick %s >= stuck %s: detection can be missed", tick, stuck)
			}
			if hard > minWatchdogTick*2 && tick >= hard {
				t.Errorf("watchdog tick %s >= hard %s: ceiling enforcement can be late", tick, hard)
			}
		})
	}
}
