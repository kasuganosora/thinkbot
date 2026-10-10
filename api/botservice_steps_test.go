package api

import (
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
)

// TestEffectiveLLMHardTimeoutDisabledByDefault 钉死「编排不再按墙钟腰斩」：
// 未显式配置时硬上限必须是 0（尽力跑完），只有运维显式设 agent.hard_timeout 才启用。
func TestEffectiveLLMHardTimeoutDisabledByDefault(t *testing.T) {
	store := config.NewStore(nil)
	if got := effectiveLLMHardTimeout(store); got != 0 {
		t.Fatalf("default hard timeout must be 0 (disabled, best-effort), got %v", got)
	}
	store.SetTemporary("agent.hard_timeout", "600")
	if got := effectiveLLMHardTimeout(store); got != 10*time.Minute {
		t.Fatalf("explicit agent.hard_timeout=600s must be honored, got %v", got)
	}
}

func TestEffectiveStepBudgets(t *testing.T) {
	cases := []struct {
		name     string
		def      *dao.BotDefinition
		wantSoft int
		wantHard int
	}{
		{
			name:     "zero means unlimited (不限制, default)",
			def:      &dao.BotDefinition{},
			wantSoft: defaultSoftMaxSteps, // 30
			wantHard: 0,                   // 0 = 不限制（无限）
		},
		{
			name:     "soft override only, hard derived as unlimited",
			def:      &dao.BotDefinition{MaxSteps: 50},
			wantSoft: 50,
			wantHard: 0, // hard=0 → 不限制（无限）
		},
		{
			name:     "explicit hard honored",
			def:      &dao.BotDefinition{MaxSteps: 20, HardMaxSteps: 200},
			wantSoft: 20,
			wantHard: 200,
		},
		{
			name:     "hard below soft is clamped up to soft",
			def:      &dao.BotDefinition{MaxSteps: 40, HardMaxSteps: 10},
			wantSoft: 40,
			wantHard: 40,
		},
		{
			name:     "negative treated as default",
			def:      &dao.BotDefinition{MaxSteps: -1, HardMaxSteps: -1},
			wantSoft: defaultSoftMaxSteps,
			wantHard: defaultHardMaxSteps,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			soft, hard := effectiveStepBudgets(c.def)
			if soft != c.wantSoft {
				t.Errorf("soft = %d, want %d", soft, c.wantSoft)
			}
			if hard != c.wantHard {
				t.Errorf("hard = %d, want %d", hard, c.wantHard)
			}
		})
	}
}
