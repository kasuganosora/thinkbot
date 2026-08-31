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
