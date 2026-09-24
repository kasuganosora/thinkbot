package api

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// TestUsageCostRow_EffectiveCost 锁定「已落库花费 + 未落库用量回算」的口径。
//
// 这是踩过的坑：早期实现先 SUM(cost_total) 再判断是否需要回算，于是同一模型下
// 只要有**一条**新记录（SUM>0），所有 cost_total=0 的存量行就被跳过 ——
// 实测总花费从 ¥0.5531 掉到 ¥0.2622，只因为补跑了一次梦境产生了新记录。
// 因此已落库与未落库必须在 SQL 层就分开统计，本测试锁住这个不变量。
func TestUsageCostRow_EffectiveCost(t *testing.T) {
	priceFor := func(id string) (llm.ModelPrice, bool) {
		if id != "glm-5.3" {
			return llm.ModelPrice{}, false
		}
		// 官方单价：输入 8 / 输出 28 / 缓存命中 2（元/百万 tokens）
		return llm.ModelPrice{InputPer1M: 8, OutputPer1M: 28, CacheReadPer1M: 2}, true
	}

	// 1) 纯存量行（cost_total=0）：1000 输入 + 500 输出 → 0.008 + 0.014 = 0.022
	r := usageCostRow{Model: "glm-5.3", UnpricedIn: 1000, UnpricedOut: 500}
	if got := r.effectiveCost(priceFor); !approx(got, 0.022) {
		t.Fatalf("legacy-only row = %v, want 0.022", got)
	}

	// 2) 纯新记录（已落库）：直接取落库值，不再回算
	r2 := usageCostRow{Model: "glm-5.3", PricedCost: 0.5}
	if got := r2.effectiveCost(priceFor); !approx(got, 0.5) {
		t.Fatalf("priced-only row = %v, want 0.5", got)
	}

	// 3) 混合行（本 bug 的核心）：已落库 0.2 + 未落库 0.022 = 0.222
	//    若退化成「SUM>0 就跳过回算」，这里会得到 0.2，凭空少掉 10%。
	r3 := usageCostRow{Model: "glm-5.3", PricedCost: 0.2, UnpricedIn: 1000, UnpricedOut: 500}
	if got := r3.effectiveCost(priceFor); !approx(got, 0.222) {
		t.Fatalf("mixed row = %v, want 0.222 (priced 0.2 + recomputed 0.022)", got)
	}

	// 4) 无单价的模型：未落库部分无法回算，保持已落库值（不能凭空编造）
	r4 := usageCostRow{Model: "unknown-model", UnpricedIn: 100000}
	if got := r4.effectiveCost(priceFor); got != 0 {
		t.Fatalf("unpriced model must stay 0, got %v", got)
	}
}

// TestSplitCompositeKey 锁定复合维度键的拆分（bot_id || '|' || feature）。
func TestSplitCompositeKey(t *testing.T) {
	botID, feature := splitCompositeKey("bot-1|dream_extract")
	if botID != "bot-1" || feature != "dream_extract" {
		t.Fatalf("got %q / %q", botID, feature)
	}
	// 无 feature 的行（早期数据 / 事件行）不能把 bot_id 当成 feature
	botID, feature = splitCompositeKey("bot-1|")
	if botID != "bot-1" || feature != "" {
		t.Fatalf("empty feature: got %q / %q", botID, feature)
	}
}

// approx 判断两个浮点数是否足够接近（计费金额是除法算出来的，不能比相等）。
func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}
