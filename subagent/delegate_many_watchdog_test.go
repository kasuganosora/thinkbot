package subagent

import (
	"context"
	"testing"
	"time"
)

// TestComputeDelegateManyBounds 锁定 DelegateMany 卡死看门狗阈值的推导规则：
// stuck 默认 180s（绝不 = effectiveTimeout/factor），hard = stuck*factor 收口到
// effectiveTimeout，且 stuck<=hard 恒成立。这是修复「看门狗误杀长工具调用」的核心。
func TestComputeDelegateManyBounds(t *testing.T) {
	cases := []struct {
		name                           string
		stuckTimeout, effectiveTimeout time.Duration
		wantStuck, wantHard            time.Duration
	}{
		{"默认：无 eff", 0, 0, defaultDelegateStuckTimeout, defaultDelegateStuckTimeout * delegateHardTimeoutFactor},
		{"默认：eff=10min", 0, 10 * time.Minute, defaultDelegateStuckTimeout, 10 * time.Minute},
		{"显式 stuck=60s, eff=10min", 60 * time.Second, 10 * time.Minute, 60 * time.Second, 10 * time.Minute},
		{"显式 stuck=60s, 无 eff", 60 * time.Second, 0, 60 * time.Second, 60 * time.Second * delegateHardTimeoutFactor},
		{"eff 小于 stuck 时收口", defaultDelegateStuckTimeout, 30 * time.Second, 30 * time.Second, 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, h := computeDelegateManyBounds(c.stuckTimeout, c.effectiveTimeout)
			if s != c.wantStuck {
				t.Errorf("stuck: got %s want %s", s, c.wantStuck)
			}
			if h != c.wantHard {
				t.Errorf("hard: got %s want %s", h, c.wantHard)
			}
			if s > h {
				t.Errorf("invariant stuck<=hard violated: stuck=%s hard=%s", s, h)
			}
		})
	}
}

// TestWatchdogTickFor 锁定看门狗轮询间隔的推导规则。
//
// 这是**防回归测试**：曾经固定用 delegateWatchdogTick(5s)，导致看门狗无法检测
// 比 5s 更短的卡死阈值——下面的 TestDelegateMany_ExplicitStuckStillKills 设
// stuck=2s，能否判定取决于 tick 与流结束的竞速，因此长期 flaky。
//
// 硬上限也在同一个 tick 循环里判定，故间隔必须同时小于二者。
// 若有人把间隔改回固定值、或只按其中一个阈值推导，本测试会失败。
func TestWatchdogTickFor(t *testing.T) {
	cases := []struct {
		name        string
		stuck, hard time.Duration
		want        time.Duration
	}{
		// 阈值较大：用上限，没必要频繁唤醒
		{"默认 180s → 5s 上限", defaultDelegateStuckTimeout,
			defaultDelegateStuckTimeout * delegateHardTimeoutFactor, delegateWatchdogTick},
		{"60s → 5s 上限", 60 * time.Second, 600 * time.Second, delegateWatchdogTick},
		{"正好 10s → 5s（stuck/2 = 上限）", 10 * time.Second, 100 * time.Second, delegateWatchdogTick},

		// stuck 较窄：收紧到 stuck/2，保证一个窗口内至少醒两次
		{"stuck 2s → 1s", 2 * time.Second, 20 * time.Second, time.Second},
		{"stuck 1s → 500ms", time.Second, 10 * time.Second, 500 * time.Millisecond},
		{"stuck 400ms → 200ms", 400 * time.Millisecond, 4 * time.Second, 200 * time.Millisecond},

		// hard 比 stuck 更窄：computeDelegateManyBounds 把 hard 收口到
		// effectiveTimeout 时会出现，此时必须按 hard 收紧
		{"hard 2s 小于 stuck 60s → 按 hard 收紧", 60 * time.Second, 2 * time.Second, time.Second},

		// 阈值极小：收口到下限，避免空耗 CPU
		{"200ms → 100ms 下限", 200 * time.Millisecond, 2 * time.Second, delegateMinWatchdogTick},
		{"50ms → 100ms 下限", 50 * time.Millisecond, 500 * time.Millisecond, delegateMinWatchdogTick},

		// 未设置（0）的阈值不参与收紧
		{"两者皆 0 → 5s 上限", 0, 0, delegateWatchdogTick},
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
			// 否则可能整个判定窗口都没醒过
			for name, threshold := range map[string]time.Duration{"stuck": c.stuck, "hard": c.hard} {
				if threshold > delegateMinWatchdogTick*2 && got >= threshold {
					t.Errorf("tick %s must be < %s %s, otherwise detection can be missed",
						got, name, threshold)
				}
			}
		})
	}
}

// TestDelegateMany_NoFalseStuckKill 端到端回归：DelegateMany 不应把「沉默较长时间
// （模拟单条长工具调用，期间编排循环不吐流片段）」的子 Agent 误判卡死。
//
// 旧实现 stuck = effectiveTimeout/10：effectiveTimeout=30s 时 stuck=3s，6s 沉默会
// 触发误杀（res.Success=false）。修复后 stuck=180s（默认），6s 沉默远小于阈值，
// 子 Agent 正常存活并返回完整文本。
func TestDelegateMany_NoFalseStuckKill(t *testing.T) {
	provider := &scriptedStreamProvider{
		name: "script",
		steps: []streamStep{
			{wait: 0, token: "hi"},             // 首 token（gotFirst=1）
			{wait: 6 * time.Second, token: ""}, // 沉默 6s，模拟长工具调用
			{wait: 0, token: "bye"},            // 收尾 token
		},
	}
	mgr := NewSubAgentManager(provider, "test-model")

	results := mgr.DelegateMany(context.Background(), "", []string{"task"},
		WithCallTimeout(30*time.Second),
	)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if !r.Success {
		t.Fatalf("subagent falsely killed as stuck: %v", r.Error)
	}
	if r.Text != "hibye" {
		t.Errorf("expected 'hibye', got %q", r.Text)
	}
}

// TestDelegateMany_ExplicitStuckStillKills 反向确认：显式 WithStuckTimeout 仍生效——
// 沉默超过该阈值应被判定卡死。证明修复没有改变「真卡死要杀」的语义。
func TestDelegateMany_ExplicitStuckStillKills(t *testing.T) {
	provider := &scriptedStreamProvider{
		name: "script",
		steps: []streamStep{
			{wait: 0, token: "hi"},
			{wait: 5 * time.Second, token: ""}, // 沉默 5s > stuck(2s)
		},
	}
	mgr := NewSubAgentManager(provider, "test-model")

	results := mgr.DelegateMany(context.Background(), "", []string{"task"},
		WithCallTimeout(30*time.Second),
		WithStuckTimeout(2*time.Second),
	)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Success {
		t.Fatalf("expected stuck kill, but succeeded: %q", r.Text)
	}
}
