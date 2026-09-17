package bot

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// TestApplyDreamPhaseDefaultsPropagatesLLMImportance 锁住一个曾经静默出现的回归：
// 生产调用方（botservice.NewDreamingBundle）只设置 Enabled/Schedule 等少数字段，
// Deep 整段为零值传入。applyDreamPhaseDefaults 必须把它未显式枚举的字段
// （UseLLMImportance / LLMImportanceWeight）从默认补齐，否则 UseLLMImportance
// 会保持零值 false，runDeep 跳过 LLM 重要性评分，使「LLM 主导降噪」的意图被关闭
// （实证：0/68 条 L1 带 dream_llm_importance）。
func TestApplyDreamPhaseDefaultsPropagatesLLMImportance(t *testing.T) {
	cfg := memory.DreamConfig{} // 模拟生产调用方：Deep 全零值
	applyDreamPhaseDefaults(&cfg)

	if !cfg.Deep.UseLLMImportance {
		t.Fatalf("UseLLMImportance 未从默认补齐，期望 true，实得 false（LLM 降噪会被静默关闭）")
	}
	if cfg.Deep.LLMImportanceWeight == 0 {
		t.Fatalf("LLMImportanceWeight 未从默认补齐，期望非 0，实得 0")
	}
	// 既有字段也应正常补齐，确保补齐逻辑整体有效。
	if cfg.Deep.MinScore == 0 {
		t.Fatalf("MinScore 未补齐，期望非 0，实得 0")
	}
	if cfg.Deep.MaxPromotions == 0 {
		t.Fatalf("MaxPromotions 未补齐，期望非 0，实得 0")
	}
}
