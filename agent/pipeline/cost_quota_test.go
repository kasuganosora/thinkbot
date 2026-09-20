package pipeline

import (
	"errors"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// sysReaderOf 构造一个返回固定全局配置的 sysReader 闭包。
func sysReaderOf(cfg SystemCostQuotaConfig) func() (SystemCostQuotaConfig, bool) {
	return func() (SystemCostQuotaConfig, bool) { return cfg, true }
}

func TestParseCostQuotaConfig(t *testing.T) {
	// 空字符串 → 零值且不启用
	if cfg, ok := ParseCostQuotaConfig(""); ok || cfg.Enabled {
		t.Fatalf("empty raw should be (zero,false), got (%+v,%v)", cfg, ok)
	}
	// 非法 JSON → 不启用
	if _, ok := ParseCostQuotaConfig("{not json"); ok {
		t.Fatal("invalid json should be false")
	}
	// 合法且启用
	raw := `{"enabled":true,"currency":"USD","total":100,"features":{"reply":50,"dreaming":10}}`
	cfg, ok := ParseCostQuotaConfig(raw)
	if !ok {
		t.Fatal("valid enabled config should be ok=true")
	}
	if cfg.Total != 100 || cfg.Currency != "USD" || cfg.Features["reply"] != 50 || cfg.Features["dreaming"] != 10 {
		t.Fatalf("parsed fields mismatch: %+v", cfg)
	}
	// 合法但 disabled（enabled=false）→ ok=false
	raw2 := `{"enabled":false,"total":100}`
	if _, ok := ParseCostQuotaConfig(raw2); ok {
		t.Fatal("disabled config should be ok=false")
	}
}

func TestPeriodKeyAndStart(t *testing.T) {
	// 固定一个周日：2026-09-20
	now := time.Date(2026, 9, 20, 15, 4, 0, 0, time.UTC)

	// daily
	if got := periodKey(now, "daily"); got != "2026-09-20" {
		t.Fatalf("daily key = %q, want 2026-09-20", got)
	}
	// monthly
	if got := periodKey(now, "monthly"); got != "2026-09" {
		t.Fatalf("monthly key = %q, want 2026-09", got)
	}
	// weekly（ISO，周一为起点）
	wk := periodKey(now, "weekly")
	if wk != "2026-W38" {
		t.Fatalf("weekly key = %q, want 2026-W38", wk)
	}
	// 周期起点：周日 2026-09-20 的周起点应为周一 2026-09-14
	start := periodStart(now, "weekly")
	if start.Weekday() != time.Monday {
		t.Fatalf("weekly start weekday = %v, want Monday", start.Weekday())
	}
	if start.Day() != 14 || start.Month() != time.September {
		t.Fatalf("weekly start = %v, want 2026-09-14", start)
	}
	// daily / monthly 起点
	if ds := periodStart(now, "daily"); ds.Day() != 20 || ds.Hour() != 0 {
		t.Fatalf("daily start = %v, want midnight of 2026-09-20", ds)
	}
	if ms := periodStart(now, "monthly"); ms.Day() != 1 || ms.Month() != time.September {
		t.Fatalf("monthly start = %v, want 2026-09-01", ms)
	}
}

func TestResolverWalls(t *testing.T) {
	bot := CostQuotaConfig{
		Enabled:  true,
		Currency: "CNY",
		Total:    100,
		Features: map[string]float64{"reply": 50, "dreaming": 10},
	}
	sys := SystemCostQuotaConfig{
		Period:   "monthly",
		Currency: "CNY",
		Total:    500,
		Features: map[string]float64{"reply": 200, "heartbeat": 5},
	}
	r := NewCostQuotaResolver(bot, sysReaderOf(sys))

	// reply 同时命中四堵墙
	walls := r.Walls("bot-1", "reply")
	if len(walls) != 4 {
		t.Fatalf("reply walls = %d, want 4 (bot/system/botFeature/feature)", len(walls))
	}
	dims := map[string]float64{}
	for _, w := range walls {
		dims[w.dim] = w.limit
	}
	if dims[costDimBot("bot-1")] != 100 {
		t.Errorf("bot wall limit = %v, want 100", dims[costDimBot("bot-1")])
	}
	if dims[costDimSystem()] != 500 {
		t.Errorf("system wall limit = %v, want 500", dims[costDimSystem()])
	}
	if dims[costDimBotFeature("bot-1", "reply")] != 50 {
		t.Errorf("botFeature wall limit = %v, want 50", dims[costDimBotFeature("bot-1", "reply")])
	}
	if dims[costDimFeature("reply")] != 200 {
		t.Errorf("feature wall limit = %v, want 200", dims[costDimFeature("reply")])
	}

	// heartbeat：bot 总预算墙（bot 启用+有总预算，与功能无关）+ 全局总预算 + 全局功能预算 = 3
	walls2 := r.Walls("bot-1", "heartbeat")
	if len(walls2) != 3 {
		t.Fatalf("heartbeat walls = %d, want 3 (bot total + system total + feature)", len(walls2))
	}
	hasFeature := false
	for _, w := range walls2 {
		if w.dim == costDimFeature("heartbeat") {
			hasFeature = true
		}
	}
	if !hasFeature {
		t.Fatal("heartbeat should include global feature wall")
	}

	// 未配置的功能（cron）→ 只有两堵总预算墙
	walls3 := r.Walls("bot-1", "cron")
	if len(walls3) != 2 {
		t.Fatalf("cron walls = %d, want 2 (bot + system total)", len(walls3))
	}

	// bot 未启用 → 只剩全局两堵墙
	botOff := CostQuotaConfig{Enabled: false, Total: 100, Features: map[string]float64{"reply": 50}}
	rOff := NewCostQuotaResolver(botOff, sysReaderOf(sys))
	if len(rOff.Walls("bot-1", "reply")) != 2 {
		t.Fatal("disabled bot should only have 2 global walls")
	}

	// 全局总预算为 0 → 不出现 system 墙
	sysNoTotal := SystemCostQuotaConfig{Period: "monthly", Total: 0, Features: map[string]float64{"reply": 200}}
	rNoTotal := NewCostQuotaResolver(bot, sysReaderOf(sysNoTotal))
	wt := rNoTotal.Walls("bot-1", "reply")
	if len(wt) != 3 {
		t.Fatalf("no global total → walls = %d, want 3 (bot + botFeature + feature)", len(wt))
	}
	for _, w := range wt {
		if w.dim == costDimSystem() {
			t.Fatal("system wall should not appear when sys.Total=0")
		}
	}
}

func TestCostQuotaStateLowestWallFirst(t *testing.T) {
	bot := CostQuotaConfig{Enabled: true, Total: 100, Features: map[string]float64{"reply": 50}}
	sys := SystemCostQuotaConfig{Period: "monthly", Total: 1000, Features: map[string]float64{"reply": 800}}
	r := NewCostQuotaResolver(bot, sysReaderOf(sys))
	st := NewCostQuotaState("monthly")

	// 记 60 到 reply：bot 总预算 100 未超，但 bot 功能预算 50 已超 → 先碰最低墙（botFeature）
	st.RecordCost("bot-1", "reply", 60)
	err := st.CheckWalls(r, "bot-1", "reply")
	if err == nil {
		t.Fatal("expected exceeded error")
	}
	if err.Dimension != costDimBotFeature("bot-1", "reply") {
		t.Fatalf("lowest wall dim = %q, want %q (feature budget 50 hit before bot total 100)",
			err.Dimension, costDimBotFeature("bot-1", "reply"))
	}
	if err.Feature != "reply" {
		t.Fatalf("err.Feature = %q, want reply", err.Feature)
	}

	// 另一功能 cron（无功能预算，bot 总预算已用 60<100）→ 不超
	if e2 := st.CheckWalls(r, "bot-1", "cron"); e2 != nil {
		t.Fatalf("cron should not exceed yet, got %v", e2)
	}

	// 继续把 bot 总预算推过 100：再记 50（cron 也算 bot 总预算）
	st.RecordCost("bot-1", "cron", 50) // bot total = 110
	e3 := st.CheckWalls(r, "bot-1", "cron")
	if e3 == nil || e3.Dimension != costDimBot("bot-1") {
		t.Fatalf("bot total wall should trigger, got %+v", e3)
	}

	// 全局总预算 1000 已用 110 < 1000 → 不超
	if e4 := st.CheckWalls(r, "bot-1", "reply"); e4.Dimension == costDimSystem() {
		t.Fatalf("system wall should not trigger yet, got %v", e4)
	}
}

func TestCostQuotaStateRecordAndUsage(t *testing.T) {
	st := NewCostQuotaState("monthly")
	st.RecordCost("bot-1", "reply", 10)
	// 四维度均 +10
	if got := st.Usage(costDimBot("bot-1")); got != 10 {
		t.Errorf("bot usage = %v, want 10", got)
	}
	if got := st.Usage(costDimSystem()); got != 10 {
		t.Errorf("system usage = %v, want 10", got)
	}
	if got := st.Usage(costDimBotFeature("bot-1", "reply")); got != 10 {
		t.Errorf("botFeature usage = %v, want 10", got)
	}
	if got := st.Usage(costDimFeature("reply")); got != 10 {
		t.Errorf("feature usage = %v, want 10", got)
	}

	// 空 feature → 只记两总预算维度，不污染 feature 维度
	st.RecordCost("bot-1", "", 5)
	if got := st.Usage(costDimBot("bot-1")); got != 15 {
		t.Errorf("bot usage after empty-feature = %v, want 15", got)
	}
	if got := st.Usage(costDimSystem()); got != 15 {
		t.Errorf("system usage after empty-feature = %v, want 15", got)
	}
	if got := st.Usage(costDimFeature("reply")); got != 10 {
		t.Errorf("feature usage should stay 10 (empty feature not recorded), got %v", got)
	}

	// 负数/零 cost 不计入
	st.RecordCost("bot-1", "reply", 0)
	if got := st.Usage(costDimBot("bot-1")); got != 15 {
		t.Errorf("zero cost should not accumulate, got %v", got)
	}

	// Snapshot 返回多维度
	snap := st.Snapshot()
	if snap[costDimBot("bot-1")] != 15 {
		t.Errorf("snapshot bot = %v, want 15", snap[costDimBot("bot-1")])
	}
}

func TestCostQuotaStateReset(t *testing.T) {
	st := NewCostQuotaState("monthly")
	st.RecordCost("bot-1", "reply", 30)
	if got := st.Usage(costDimBot("bot-1")); got != 30 {
		t.Fatalf("before reset = %v, want 30", got)
	}
	st.Reset(costDimBot("bot-1"))
	if got := st.Usage(costDimBot("bot-1")); got != 0 {
		t.Fatalf("after reset = %v, want 0", got)
	}
	// 其余维度不受影响
	if got := st.Usage(costDimSystem()); got != 30 {
		t.Fatalf("system should stay 30, got %v", got)
	}
}

func TestCostPeriodCounterCrossBucket(t *testing.T) {
	c := newCostPeriodCounter()
	// 当前桶 2026-09
	if got := c.add(50, "2026-09"); got != 50 {
		t.Fatalf("first add = %v, want 50", got)
	}
	// 同桶累加
	if got := c.add(10, "2026-09"); got != 60 {
		t.Fatalf("same bucket add = %v, want 60", got)
	}
	// 跨桶（新月份）自动归零再累加
	if got := c.add(5, "2026-10"); got != 5 {
		t.Fatalf("cross-bucket add = %v, want 5 (reset to 0 then +5)", got)
	}
	if got := c.get("2026-10"); got != 5 {
		t.Fatalf("cross-bucket get = %v, want 5", got)
	}
}

func TestCostQuotaExceededErrorIs(t *testing.T) {
	err := llm.NewCostQuotaExceeded("bot:x", "reply", 60, 50, "2026-09")
	if err.Dimension != "bot:x" || err.Feature != "reply" || err.Current != 60 || err.Limit != 50 {
		t.Fatalf("fields mismatch: %+v", err)
	}
	// errors.Is 支持
	if !errors.Is(err, llm.ErrCostQuotaExceeded) {
		t.Fatal("errors.Is(err, ErrCostQuotaExceeded) should be true")
	}
	// 其他错误不误判
	if errors.Is(errors.New("boom"), llm.ErrCostQuotaExceeded) {
		t.Fatal("unrelated error must not match ErrCostQuotaExceeded")
	}
}
