package memory

import (
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestLLMConsolidator_BuildPromptIncludesAllExisting 验证：合并阶段传给 LLM 的
// 去重参考会包含调用方提供的全部已有 L1 条目（不被内部截断）。
//
// 背景：曾因调用方只取 50 条已有 L1 做去重参考，而 L1 每 scope 上限 500，
// 超出 50 的已有条目进不了提示词，模型无法感知其存在 → 同一实体反复新建、画像碎片化。
// 修复后调用方改取 consolidateL1RefLimit(200)；本测试锁定「传入 N 条则提示词含 N 条」
// 这一不变量，使该修复不被 future 的截断逻辑悄悄破坏。
func TestLLMConsolidator_BuildPromptIncludesAllExisting(t *testing.T) {
	c := &LLMConsolidator{logger: zap.NewNop().Sugar()}

	// 构造 60 条已有 L1（超过旧的 50 上限），覆盖不同实体观察
	const n = 60
	existing := make([]TieredEntry, n)
	for i := 0; i < n; i++ {
		existing[i] = TieredEntry{
			Entry: Entry{
				ID:       fmt.Sprintf("l1-%d", i),
				Category: "fact",
				Content:  "some observed fact",
			},
			Tier: Tier1LongTerm,
		}
	}

	prompt := c.buildPrompt(nil, existing)

	// 每条 existing 在提示词里以 "[id] (category) content" 形式出现一次
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("l1-%d", i)
		if cnt := strings.Count(prompt, "["+id+"]"); cnt != 1 {
			t.Fatalf("existing L1 %s should appear exactly once in dedup reference, got %d", id, cnt)
		}
	}
}

// TestConsolidateL1RefLimitSane 锁定「L1 去重参考窗口必须覆盖完整 L1 上限」，
// 防止有人把上限改回小于常规 L1 规模的值（那样已有 L1 会被截断出去重提示词，
// 导致模型为同一实体反复新建、画像碎片化）。
func TestConsolidateL1RefLimitSane(t *testing.T) {
	cfg := DefaultTierConfigs()
	l1Max := cfg[Tier1LongTerm].MaxEntries
	if consolidateL1RefLimit < l1Max {
		t.Fatalf("consolidateL1RefLimit(%d) < L1 MaxEntries(%d); existing L1 beyond the limit would be truncated from dedup prompt",
			consolidateL1RefLimit, l1Max)
	}
}
