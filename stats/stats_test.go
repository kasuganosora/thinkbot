package stats

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.UsageDaily{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestRecorder_BasicRecordAndQuery(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	ctx := context.Background()

	// 记录两条相同维度的指标
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID:   "bot1",
		Model:   "claude-sonnet-4",
		Feature: "reply",
		Usage: llm.Usage{
			InputTokens:       100,
			OutputTokens:      50,
			TotalTokens:       150,
			CachedInputTokens: 80,
			InputTokenDetails: llm.InputTokenDetail{
				CacheReadTokens: 80,
				NoCacheTokens:   20,
			},
		},
		ToolCalls: 2,
		Steps:     1,
	})

	r.RecordUsage(ctx, llm.UsageMetric{
		BotID:   "bot1",
		Model:   "claude-sonnet-4",
		Feature: "reply",
		Usage: llm.Usage{
			InputTokens:  200,
			OutputTokens: 100,
			TotalTokens:  300,
			InputTokenDetails: llm.InputTokenDetail{
				NoCacheTokens: 200,
			},
		},
		ToolCalls: 1,
		Steps:     2,
	})

	// 记录不同模型
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID:   "bot1",
		Model:   "gpt-4o",
		Feature: "chat",
		Usage: llm.Usage{
			InputTokens:       500,
			OutputTokens:      300,
			TotalTokens:       800,
			CachedInputTokens: 500,
			InputTokenDetails: llm.InputTokenDetail{
				CacheReadTokens: 500,
			},
		},
		Steps: 1,
	})

	// 记录不同 bot
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID:   "bot2",
		Model:   "claude-sonnet-4",
		Feature: "reply",
		Usage: llm.Usage{
			InputTokens:  1000,
			OutputTokens: 500,
			TotalTokens:  1500,
		},
		Steps: 1,
	})

	r.SyncFlush()

	// 查询 bot1 的模型统计
	bot1Stats, err := GetBotModelStats(db, "bot1", nil, nil)
	if err != nil {
		t.Fatalf("GetBotModelStats: %v", err)
	}
	if len(bot1Stats) != 2 {
		t.Fatalf("expected 2 models for bot1, got %d", len(bot1Stats))
	}

	// 找到 claude-sonnet-4 的统计
	var claudeStat *BotModelStat
	for i := range bot1Stats {
		if bot1Stats[i].Model == "claude-sonnet-4" {
			claudeStat = &bot1Stats[i]
			break
		}
	}
	if claudeStat == nil {
		t.Fatal("claude-sonnet-4 stat not found")
	}

	// 验证累加
	if claudeStat.TotalRequests != 2 {
		t.Errorf("TotalRequests: got %d, want 2", claudeStat.TotalRequests)
	}
	if claudeStat.TotalTokens != 450 {
		t.Errorf("TotalTokens: got %d, want 450", claudeStat.TotalTokens)
	}
	if claudeStat.CacheHitRequests != 1 {
		t.Errorf("CacheHitRequests: got %d, want 1", claudeStat.CacheHitRequests)
	}
	if claudeStat.CacheMissRequests != 1 {
		t.Errorf("CacheMissRequests: got %d, want 1", claudeStat.CacheMissRequests)
	}
	if claudeStat.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens: got %d, want 80", claudeStat.CacheReadTokens)
	}
	if claudeStat.ToolCalls != 3 {
		t.Errorf("ToolCalls: got %d, want 3", claudeStat.ToolCalls)
	}

	// 验证 gpt-4o 的统计
	var gptStat *BotModelStat
	for i := range bot1Stats {
		if bot1Stats[i].Model == "gpt-4o" {
			gptStat = &bot1Stats[i]
			break
		}
	}
	if gptStat == nil {
		t.Fatal("gpt-4o stat not found")
	}
	if gptStat.TotalRequests != 1 {
		t.Errorf("gpt-4o TotalRequests: got %d, want 1", gptStat.TotalRequests)
	}
	if gptStat.CacheHitRequests != 1 {
		t.Errorf("gpt-4o CacheHitRequests: got %d, want 1", gptStat.CacheHitRequests)
	}
}

func TestRecorder_ModelFeatureStats(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	ctx := context.Background()

	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot1", Model: "claude-sonnet-4", Feature: "reply",
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	})
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot1", Model: "claude-sonnet-4", Feature: "vision",
		Usage: llm.Usage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
	})
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot1", Model: "claude-sonnet-4", Feature: "vision",
		Usage: llm.Usage{InputTokens: 50, OutputTokens: 25, TotalTokens: 75},
	})

	r.SyncFlush()

	stats, err := GetModelFeatureStats(db, "bot1", "claude-sonnet-4", nil, nil)
	if err != nil {
		t.Fatalf("GetModelFeatureStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 features, got %d", len(stats))
	}

	// 按 total_requests 降序，vision 应该在前（2 > 1）
	if stats[0].Feature != "vision" {
		t.Errorf("expected vision first, got %s", stats[0].Feature)
	}
	if stats[0].TotalRequests != 2 {
		t.Errorf("vision TotalRequests: got %d, want 2", stats[0].TotalRequests)
	}
	if stats[1].Feature != "reply" {
		t.Errorf("expected reply second, got %s", stats[1].Feature)
	}
}

func TestRecorder_AllBotsStats(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	ctx := context.Background()

	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot1", Model: "gpt-4o", Feature: "reply",
		Usage: llm.Usage{TotalTokens: 100},
	})
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot2", Model: "gpt-4o", Feature: "reply",
		Usage: llm.Usage{TotalTokens: 200},
	})

	r.SyncFlush()

	allStats, err := GetAllBotsModelStats(db, nil, nil)
	if err != nil {
		t.Fatalf("GetAllBotsModelStats: %v", err)
	}
	if len(allStats) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(allStats))
	}
}

func TestRecorder_DailyStats(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	ctx := context.Background()

	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot1", Model: "gpt-4o", Feature: "reply",
		Usage: llm.Usage{TotalTokens: 100},
	})
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot1", Model: "gpt-4o", Feature: "reply",
		Usage: llm.Usage{TotalTokens: 200},
	})

	r.SyncFlush()

	daily, err := GetDailyStats(db, "bot1", nil, nil)
	if err != nil {
		t.Fatalf("GetDailyStats: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("expected 1 day, got %d", len(daily))
	}
	if daily[0].TotalRequests != 2 {
		t.Errorf("TotalRequests: got %d, want 2", daily[0].TotalRequests)
	}
	if daily[0].TotalTokens != 300 {
		t.Errorf("TotalTokens: got %d, want 300", daily[0].TotalTokens)
	}
}

func TestRecorder_NilSafe(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())

	// 大量记录不应阻塞或 panic
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		r.RecordUsage(ctx, llm.UsageMetric{
			BotID: "bot1", Model: "test-model", Feature: "test",
			Usage: llm.Usage{TotalTokens: 1},
		})
	}
}

func TestRecorder_AsyncWithStartStop(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	r.Start()

	ctx := context.Background()
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID: "bot1", Model: "gpt-4o", Feature: "reply",
		Usage: llm.Usage{TotalTokens: 100},
	})

	r.Stop() // Stop 会 drain + flush

	stats, err := GetBotModelStats(db, "bot1", nil, nil)
	if err != nil {
		t.Fatalf("GetBotModelStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 row after Start/Stop, got %d", len(stats))
	}
	if stats[0].TotalTokens != 100 {
		t.Errorf("TotalTokens: got %d, want 100", stats[0].TotalTokens)
	}
}

func TestTruncateToDate(t *testing.T) {
	input := time.Date(2025, 6, 20, 15, 30, 45, 123456789, time.UTC)
	got := truncateToDate(input)
	want := time.Date(2025, 6, 20, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("truncateToDate: got %v, want %v", got, want)
	}
}

// TestRecorder_AggregatedRequests 锁住「一条 metric 可代表多次 LLM 调用」的语义。
//
// 回归：stage 层（reply / llmroute）把**整轮编排**聚合成一条 UsageMetric 上报，
// Usage 是这一轮 N 次 LLM 调用的 token 之和。此前入库时一律按 1 个请求计，
// 导致「总请求数」被低估、平均每请求 token 虚高一个量级（线上实测 7 万+/请求）。
func TestRecorder_AggregatedRequests(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	ctx := context.Background()

	// 一轮编排：6 次 LLM 调用，token 为 6 次之和
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID:    "bot1",
		Model:    "m1",
		Feature:  "reply",
		Requests: 6,
		Usage:    llm.Usage{InputTokens: 6000, OutputTokens: 600, TotalTokens: 6600},
	})
	// 单次调用（未填 Requests）仍按 1 计
	r.RecordUsage(ctx, llm.UsageMetric{
		BotID:   "bot1",
		Model:   "m1",
		Feature: "dream_extract",
		Usage:   llm.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	})
	r.SyncFlush()

	var rows []dao.UsageDaily
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	byFeature := map[string]dao.UsageDaily{}
	for _, row := range rows {
		byFeature[row.Feature] = row
	}

	if got := byFeature["reply"].TotalRequests; got != 6 {
		t.Errorf("reply total_requests = %d, want 6 (one metric may cover N LLM calls)", got)
	}
	if got := byFeature["reply"].CacheMissRequests; got != 6 {
		t.Errorf("reply cache_miss_requests = %d, want 6 (must scale with request count)", got)
	}
	if got := byFeature["dream_extract"].TotalRequests; got != 1 {
		t.Errorf("dream_extract total_requests = %d, want 1 (default when Requests unset)", got)
	}
}

// TestRecorder_CostDoesNotDoubleCountCache 锁定落库 cost_* 的口径：
// InputTokens 含缓存，命中部分按缓存价计一次；cost_input + cost_output == cost_total。
func TestRecorder_CostDoesNotDoubleCountCache(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	r.SetPriceResolver(func(id string) (llm.ModelPrice, bool) {
		return llm.ModelPrice{InputPer1M: 8, OutputPer1M: 28, CacheReadPer1M: 2}, true
	})
	r.RecordUsage(context.Background(), llm.UsageMetric{
		BotID: "bot1", Model: "glm-5.3", Feature: "reply",
		Usage: llm.Usage{
			InputTokens: 1_000_000, OutputTokens: 100_000, TotalTokens: 1_100_000,
			CachedInputTokens: 800_000,
			InputTokenDetails: llm.InputTokenDetail{CacheReadTokens: 800_000, NoCacheTokens: 200_000},
		},
	})
	r.SyncFlush()

	var row dao.UsageDaily
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	near := func(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }
	// input = 0.2M*8 + 0.8M*2 = 3.2；output = 0.1M*28 = 2.8；total = 6.0（旧公式 11.6）
	if !near(row.CostInput, 3.2) || !near(row.CostOutput, 2.8) || !near(row.CostTotal, 6.0) {
		t.Fatalf("cost (in,out,total) = (%v,%v,%v), want (3.2,2.8,6.0)", row.CostInput, row.CostOutput, row.CostTotal)
	}
}
