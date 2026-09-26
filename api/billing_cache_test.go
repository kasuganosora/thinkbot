package api

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

func glmPriceFor(id string) (llm.ModelPrice, bool) {
	if id != "glm-5.3" {
		return llm.ModelPrice{}, false
	}
	return llm.ModelPrice{InputPer1M: 8, OutputPer1M: 28, CacheReadPer1M: 2}, true
}

// TestUsageCostRow_EffectiveCostCacheNotDoubleCounted 锁定存量行回算不再双计缓存命中。
//
// stats_usage_daily.input_tokens 含缓存（OpenAI 兼容协议 prompt_tokens 已包含
// cached_tokens），旧公式 input*8 + cache*2 会把命中部分按全价与缓存价各算一次。
func TestUsageCostRow_EffectiveCostCacheNotDoubleCounted(t *testing.T) {
	// 1M 输入（其中 800K 命中缓存）：0.2M*8 + 0.8M*2 = 1.6 + 1.6 = 3.2（旧公式 9.6）
	r := usageCostRow{Model: "glm-5.3", UnpricedIn: 1_000_000, UnpricedCache: 800_000}
	if got := r.effectiveCost(glmPriceFor); !approx(got, 3.2) {
		t.Fatalf("legacy cached row = %v, want 3.2", got)
	}
}

// TestScanUsageCostRows_SQLAggregationConsistent 走真实 SQL 聚合：
// 已落库行取 cost_total，cost_total=0 的存量行按修正后的公式回算，二者相加。
func TestScanUsageCostRows_SQLAggregationConsistent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.UsageDaily{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	rows := []dao.UsageDaily{
		// 存量行（单价补配之前写入，cost_total=0）
		{BotID: "b", Model: "glm-5.3", Feature: "reply", Date: day,
			InputTokens: 1_000_000, CacheReadTokens: 800_000, OutputTokens: 100_000},
		// 已落库行（新代码按正确公式写入的花费）
		{BotID: "b", Model: "glm-5.3", Feature: "llm", Date: day.AddDate(0, 0, 1),
			InputTokens: 500_000, CacheReadTokens: 400_000, OutputTokens: 10_000, CostTotal: 1.88},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got, err := scanUsageCostRows(db, "model", "date >= ?", day)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 model row, got %d", len(got))
	}
	r := got[0]
	if r.UnpricedIn != 1_000_000 || r.UnpricedCache != 800_000 || r.UnpricedOut != 100_000 {
		t.Fatalf("unpriced split wrong: %+v", r)
	}
	// 存量行：0.2M*8 + 0.8M*2 + 0.1M*28 = 1.6 + 1.6 + 2.8 = 6.0；已落库 1.88
	if cost := r.effectiveCost(glmPriceFor); !approx(cost, 7.88) {
		t.Fatalf("effective cost = %v, want 7.88 (stored 1.88 + legacy 6.0)", cost)
	}

	// 与墙口径一致：同一行用 llm.ComputeCost 直接算应得到相同的存量花费。
	_, _, legacy := llm.ComputeCost(llm.Usage{
		InputTokens: 1_000_000, OutputTokens: 100_000,
		InputTokenDetails: llm.InputTokenDetail{CacheReadTokens: 800_000},
	}, llm.ModelPrice{InputPer1M: 8, OutputPer1M: 28, CacheReadPer1M: 2})
	if !approx(legacy, 6.0) {
		t.Fatalf("ComputeCost legacy = %v, want 6.0", legacy)
	}
}
