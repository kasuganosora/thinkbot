package pipeline

import (
	"errors"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/stats"
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

// ---------------------------------------------------------------------------
// 全局维度共享（回归：曾因 per-bot state 使全局预算退化成「每 bot 一份」）
// ---------------------------------------------------------------------------

func TestCostQuotaStateGlobalSharedAcrossBots(t *testing.T) {
	// 同一个 state 服务两个 bot —— 这正是生产环境的形态（进程级共享）。
	st := NewCostQuotaState("monthly")

	st.RecordCost("bot-a", "reply", 10)
	st.RecordCost("bot-b", "reply", 20)

	if got := st.Usage(costDimSystem()); got != 30 {
		t.Errorf("system usage = %v, want 30 (global wall must aggregate across bots)", got)
	}
	if got := st.Usage(costDimFeature("reply")); got != 30 {
		t.Errorf("feature:reply usage = %v, want 30", got)
	}
	if got := st.Usage(costDimBot("bot-a")); got != 10 {
		t.Errorf("bot-a usage = %v, want 10", got)
	}
	if got := st.Usage(costDimBot("bot-b")); got != 20 {
		t.Errorf("bot-b usage = %v, want 20", got)
	}

	// 全局墙被 bot-a + bot-b 的合计撞到（而非各自独立额度）
	sys := SystemCostQuotaConfig{Period: "monthly", Total: 25}
	rA := NewCostQuotaResolver(CostQuotaConfig{Enabled: true, Total: 1000}, sysReaderOf(sys))
	rB := NewCostQuotaResolver(CostQuotaConfig{Enabled: true, Total: 1000}, sysReaderOf(sys))
	if e := st.CheckWalls(rB, "bot-b", "reply"); e == nil || e.Dimension != costDimSystem() {
		t.Fatalf("system wall should trigger on aggregate 30 > 25, got %+v", e)
	}
	// bot-a 虽然自己只花了 10（< 25），但全局已耗尽 → 同样被拦
	if e := st.CheckWalls(rA, "bot-a", "reply"); e == nil || e.Dimension != costDimSystem() {
		t.Fatalf("bot-a must also be blocked by exhausted global wall, got %+v", e)
	}
}

// ---------------------------------------------------------------------------
// period 实时生效（改周期无需重启 bot）
// ---------------------------------------------------------------------------

func TestCostQuotaStatePeriodReaderLive(t *testing.T) {
	period := "monthly"
	st := NewCostQuotaStateWithPeriodReader(func() string { return period })

	st.RecordCost("bot-1", "reply", 10)
	if got := st.Usage(costDimBot("bot-1")); got != 10 {
		t.Fatalf("usage = %v, want 10", got)
	}
	if st.Period() != "monthly" {
		t.Fatalf("Period() = %q, want monthly", st.Period())
	}

	// 运行期把周期改成 daily → 桶标识变化 → 计数器自动归零（新周期从头计）
	period = "daily"
	if st.Period() != "daily" {
		t.Fatalf("Period() after change = %q, want daily (period must be read live)", st.Period())
	}
	if got := st.Usage(costDimBot("bot-1")); got != 0 {
		t.Errorf("usage after period switch = %v, want 0 (new bucket)", got)
	}
}

// ---------------------------------------------------------------------------
// 恢复幂等：ResetBot / ResetGlobal 只清自己辖区，避免重启重复累加
// ---------------------------------------------------------------------------

func TestCostQuotaStateResetBotIsScoped(t *testing.T) {
	st := NewCostQuotaState("monthly")
	st.RecordCost("bot-a", "reply", 10)
	st.RecordCost("bot-b", "reply", 20)

	// 只清 bot-a（含其 feature 维度），bot-b 与全局维度不受影响
	st.ResetBot("bot-a")
	if got := st.Usage(costDimBot("bot-a")); got != 0 {
		t.Errorf("bot-a after reset = %v, want 0", got)
	}
	if got := st.Usage(costDimBotFeature("bot-a", "reply")); got != 0 {
		t.Errorf("bot-a feature after reset = %v, want 0", got)
	}
	if got := st.Usage(costDimBot("bot-b")); got != 20 {
		t.Errorf("bot-b must survive bot-a reset, got %v want 20", got)
	}
	if got := st.Usage(costDimSystem()); got != 30 {
		t.Errorf("system must survive bot-a reset, got %v want 30", got)
	}

	st.ResetGlobal()
	if got := st.Usage(costDimSystem()); got != 0 {
		t.Errorf("system after ResetGlobal = %v, want 0", got)
	}
	if got := st.Usage(costDimFeature("reply")); got != 0 {
		t.Errorf("feature:reply after ResetGlobal = %v, want 0", got)
	}
	if got := st.Usage(costDimBot("bot-b")); got != 20 {
		t.Errorf("bot-b must survive ResetGlobal, got %v want 20", got)
	}
}

// ---------------------------------------------------------------------------
// 恢复口径：优先用落库的 cost_total，而非按当前单价重算历史
// ---------------------------------------------------------------------------

func TestRowCostPrefersStoredCost(t *testing.T) {
	// 有 cost_total → 原样采用，即使当前单价算出来不同（改单价不应篡改历史）
	row := costRestoreRow{Model: "m1", Input: 1000, Output: 1000, Cost: 7.5}
	if got := rowCost(row, nil); got != 7.5 {
		t.Errorf("rowCost with stored = %v, want 7.5", got)
	}

	// 无 cost_total（存量行） → 按当前单价回算
	row2 := costRestoreRow{Model: "m1", Input: 1_000_000, Output: 0}
	priceFor := func(id string) (llm.ModelPrice, bool) {
		if id != "m1" {
			return llm.ModelPrice{}, false
		}
		return llm.ModelPrice{InputPer1M: 2, OutputPer1M: 8, Currency: "CNY"}, true
	}
	if got := rowCost(row2, priceFor); got != 2 {
		t.Errorf("rowCost fallback = %v, want 2 (1M input × ¥2/1M)", got)
	}

	// 既无 cost_total 也无单价 → 0（不计费、不限额）
	if got := rowCost(costRestoreRow{Model: "unknown"}, priceFor); got != 0 {
		t.Errorf("rowCost without price = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 周期桶口径：必须与 stats_usage_daily.date 的写入口径（stats.TruncateToDate）一致
//
// 回归：曾把 periodStart/periodKey 改成 UTC 日历日，导致东八区每月/每周/每日
// 的头 8 小时被算进上一个周期（本地 9/1 07:00 的调用落进 8 月桶）。
// ---------------------------------------------------------------------------

func TestPeriodStartMatchesStatsDateBucket(t *testing.T) {
	// 用本地时区构造一个「早上 7 点」的时刻：UTC 日历日仍在前一天，
	// 但本地日历日已经是当天 —— 正是曾经出错的窗口。
	now := time.Date(2026, 9, 1, 7, 0, 0, 0, time.Local)

	for _, p := range []string{"daily", "monthly"} {
		got := periodStart(now, p)
		if want := stats.TruncateToDate(now); p == "daily" && !got.Equal(want) {
			t.Errorf("daily periodStart = %v, want %v (must equal stats date bucket)", got, want)
		}
		// 桶起点永不能超过「今天」这一行的 date 值，否则整天被漏掉
		if got.After(stats.TruncateToDate(now)) {
			t.Errorf("%s periodStart %v is after today's date row %v — the whole day would be filtered out",
				p, got, stats.TruncateToDate(now))
		}
	}

	// daily 桶起点必须正好等于当天的 date 值
	if got, want := periodStart(now, "daily"), stats.TruncateToDate(now); !got.Equal(want) {
		t.Errorf("daily bucket = %v, want %v", got, want)
	}
	// monthly 桶起点必须是本地当月 1 号的 UTC 零点（不是 UTC 月份）
	if got, want := periodStart(now, "monthly"), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("monthly bucket = %v, want %v", got, want)
	}
	// 周期键同样按本地日历日：本地 9/1 07:00 应归到 9 月桶
	if got := periodKey(now, "monthly"); got != "2026-09" {
		t.Errorf("monthly key = %q, want 2026-09 (local calendar day)", got)
	}
}

// ---------------------------------------------------------------------------
// 功能组：配「dreaming」必须能限住 dream_extract / dream_cluster 等细分阶段
//
// 回归：统计表里的功能标签是阶段名，用户在预算里只会写功能名，
// 精确匹配会让这类预算永远等不到数据（进度恒为 0、永不拦截）。
// ---------------------------------------------------------------------------

func TestCostFeatureGroupBudgetAppliesToSubPhases(t *testing.T) {
	st := NewCostQuotaState("monthly")
	sys := SystemCostQuotaConfig{
		Period:   "monthly",
		Features: map[string]float64{"dreaming": 50},
	}
	r := NewCostQuotaResolver(CostQuotaConfig{Enabled: true}, sysReaderOf(sys))

	st.RecordCost("bot-1", "dream_extract", 30)

	// 细分维度保留（看板仍可下钻到阶段）
	if got := st.Usage(costDimFeature("dream_extract")); got != 30 {
		t.Errorf("feature:dream_extract = %v, want 30", got)
	}
	// 组维度同步累加（配 "dreaming" 才能生效）
	if got := st.Usage(costDimFeature("dreaming")); got != 30 {
		t.Errorf("feature:dreaming = %v, want 30 (group must aggregate sub-phases)", got)
	}
	// bot 级组维度同样累加
	if got := st.Usage(costDimBotFeature("bot-1", "dreaming")); got != 30 {
		t.Errorf("bot feature group = %v, want 30", got)
	}
	// 未超预算不拦
	if e := st.CheckWalls(r, "bot-1", "dream_extract"); e != nil {
		t.Fatalf("should not block below budget, got %+v", e)
	}

	// 另一个阶段再花 25 → 组累计 55 > 50 → 所有梦境阶段都被拦
	st.RecordCost("bot-1", "dream_cluster", 25)
	e := st.CheckWalls(r, "bot-1", "dream_score")
	if e == nil || e.Dimension != costDimFeature("dreaming") {
		t.Fatalf("dreaming group wall should trigger at 55/50, got %+v", e)
	}
	// 无关的 reply 不受梦境预算影响
	if e := st.CheckWalls(r, "bot-1", "reply"); e != nil {
		t.Errorf("reply must not be blocked by dreaming budget, got %+v", e)
	}
}

func TestCostFeatureGroupMapping(t *testing.T) {
	cases := map[string]string{
		"dream_extract": "dreaming",
		"dream_cluster": "dreaming",
		"dreaming":      "dreaming", // 已是组名 → 原样返回，不重复记
		"memory_dedup":  "memory",
		"subagent":      "subagent",
		"reply":         "reply",
		"":              "",
	}
	for in, want := range cases {
		if got := costFeatureGroup(in); got != want {
			t.Errorf("costFeatureGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFriendlyCostReplyRoutable 锁定「被额度拦住时用户真的能看到提示」。
//
// 踩过的坑：友好回复只填了 Channel，没带 Action.Metadata["source_channel"]，
// ChannelReplyHandler 找不到 Sender 直接报 "no source_channel in action metadata"
// 派发失败 —— 日志里写着「已转友好回复」，用户端却一直转圈到超时。
// 拦截生效但收不到任何反馈，排查时会误判成「额度没生效」。
func TestFriendlyCostReplyRoutable(t *testing.T) {
	msg := core.Message{
		ID:      "msg-1",
		BotID:   "bot-1",
		Source:  "web:1",
		Channel: "web:1",
		UserID:  "u-1",
		Metadata: map[string]any{
			"reply_target": "web:session-9",
		},
	}
	env := core.NewEnvelope(msg)
	ce := &llm.CostQuotaExceededError{
		Dimension: costDimSystem(),
		Current:   0.79,
		Limit:     0.05,
		Period:    "2026-09-24",
	}

	out := friendlyCostReply(env, ce, "daily", "CNY")
	acts := out.Actions()
	if len(acts) != 1 {
		t.Fatalf("actions = %d, want 1", len(acts))
	}
	a := acts[0]
	if a.Type != core.ActionReply || a.Payload == "" {
		t.Fatalf("want a non-empty ActionReply, got %+v", a)
	}
	// 回复目标：优先 reply_target，不是 msg.Channel
	if a.Channel != "web:session-9" {
		t.Errorf("channel = %q, want reply_target web:session-9", a.Channel)
	}
	// 路由元数据：缺了就派发失败
	sc, ok := a.Metadata["source_channel"].(string)
	if !ok || sc != "web:1" {
		t.Fatalf("source_channel = %v (present=%v), want web:1", a.Metadata["source_channel"], ok)
	}
	if a.Metadata["bot_id"] != "bot-1" || a.Metadata["message_id"] != "msg-1" {
		t.Errorf("metadata = %+v, want bot_id/message_id carried", a.Metadata)
	}
}

// TestFriendlyCostReplyFallsBackToChannel 无 reply_target 时回退到 msg.Channel。
func TestFriendlyCostReplyFallsBackToChannel(t *testing.T) {
	env := core.NewEnvelope(core.Message{ID: "m", BotID: "b", Source: "tg", Channel: "tg-chat"})
	out := friendlyCostReply(env, &llm.CostQuotaExceededError{
		Dimension: costDimSystem(), Current: 1, Limit: 1, Period: "p",
	}, "daily", "CNY")
	a := out.Actions()[0]
	if a.Channel != "tg-chat" {
		t.Fatalf("channel = %q, want fallback to msg.Channel tg-chat", a.Channel)
	}
	if a.Metadata["source_channel"] != "tg" {
		t.Fatalf("source_channel = %v, want tg", a.Metadata["source_channel"])
	}
}
