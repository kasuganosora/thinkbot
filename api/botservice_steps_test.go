package api

import (
	"testing"

	"github.com/kasuganosora/thinkbot/dao"
)

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
			wantHard: 0,                    // 0 = 不限制（无限）
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
