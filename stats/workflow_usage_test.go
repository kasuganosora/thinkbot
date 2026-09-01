package stats

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 工作流维度成本归因
//
// 核心保证：**加工作流维度不得改变 UsageDaily 的聚合结果**。
// 那张表是五维日聚合（bot/model/feature/channel/date）带唯一索引，
// 一旦把 workflow/node 并进聚合维度，日表会退化成明细表。
// 这里用逐条明细表旁路承接工作流维度，本文件锁死这个分工。
// ============================================================================

func newWorkflowUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.UsageDaily{}, &dao.WorkflowUsage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestWorkflowUsage_UsageDailyUnaffected 回归护栏：工作流维度不参与日聚合。
//
// 同一个 (bot, model, feature, channel, date) 下，来自**不同工作流、不同节点**
// 的调用必须被合并成**一行**，token 累加。
func TestWorkflowUsage_UsageDailyUnaffected(t *testing.T) {
	db := newWorkflowUsageTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())
	ctx := context.Background()

	base := llm.UsageMetric{
		BotID:   "bot1",
		Model:   "glm-4.6",
		Feature: "subagent",
		Usage: llm.Usage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}

	// 三条调用：无工作流维度 / 工作流 A 节点 1 / 工作流 B 节点 2
	m1 := base
	m2 := base
	m2.WorkflowID = "wf-A"
	m2.NodeID = "n1"
	m3 := base
	m3.WorkflowID = "wf-B"
	m3.NodeID = "n2"

	r.RecordUsage(ctx, m1)
	r.RecordUsage(ctx, m2)
	r.RecordUsage(ctx, m3)
	r.SyncFlush()

	// 日聚合：仍应是**一行**（维度相同），token = 3 × 150
	var rows []dao.UsageDaily
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("query usage_daily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage_daily must stay aggregated across workflows: got %d rows, want 1", len(rows))
	}
	if rows[0].TotalTokens != 450 {
		t.Errorf("TotalTokens: got %d, want 450 (3 x 150)", rows[0].TotalTokens)
	}
	if rows[0].TotalRequests != 3 {
		t.Errorf("TotalRequests: got %d, want 3", rows[0].TotalRequests)
	}
}

// TestWorkflowUsage_DetailRows 明细表逐条记录，带完整归因维度。
func TestWorkflowUsage_DetailRows(t *testing.T) {
	db := newWorkflowUsageTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())

	r.RecordUsage(context.Background(), llm.UsageMetric{
		BotID:      "bot1",
		Model:      "glm-4.6",
		Feature:    "subagent",
		WorkflowID: "wf-A",
		NodeID:     "n1",
		Usage: llm.Usage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
			InputTokenDetails: llm.InputTokenDetail{
				CacheReadTokens: 30,
			},
		},
		ToolCalls: 4,
		Steps:     2,
	})
	r.RecordUsage(context.Background(), llm.UsageMetric{
		BotID:      "bot1",
		Model:      "glm-4.6",
		Feature:    "subagent",
		WorkflowID: "wf-A",
		NodeID:     "n2",
		Usage: llm.Usage{
			InputTokens:  200,
			OutputTokens: 80,
			TotalTokens:  280,
		},
		ToolCalls: 9,
	})
	r.SyncFlush()

	var rows []dao.WorkflowUsage
	if err := db.Order("node_id").Find(&rows).Error; err != nil {
		t.Fatalf("query workflow_usage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 detail rows, got %d", len(rows))
	}

	if rows[0].WorkflowID != "wf-A" || rows[0].NodeID != "n1" {
		t.Errorf("row0 attribution wrong: %+v", rows[0])
	}
	if rows[0].TotalTokens != 150 || rows[0].CacheReadTokens != 30 {
		t.Errorf("row0 tokens wrong: %+v", rows[0])
	}
	if rows[0].ToolCalls != 4 || rows[0].Steps != 2 {
		t.Errorf("row0 orchestration metrics wrong: %+v", rows[0])
	}
	if rows[1].NodeID != "n2" || rows[1].TotalTokens != 280 {
		t.Errorf("row1 wrong: %+v", rows[1])
	}
}

// TestWorkflowUsage_NonWorkflowCallsIgnored 非工作流路径不产生明细。
// 否则 reply / dream / memory_compress 等调用会白白撑大明细表。
func TestWorkflowUsage_NonWorkflowCallsIgnored(t *testing.T) {
	db := newWorkflowUsageTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())

	for i := 0; i < 3; i++ {
		r.RecordUsage(context.Background(), llm.UsageMetric{
			BotID:   "bot1",
			Model:   "m",
			Feature: "reply",
			Usage:   llm.Usage{TotalTokens: 10},
		})
	}
	r.SyncFlush()

	var count int64
	if err := db.Model(&dao.WorkflowUsage{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("non-workflow calls must not produce detail rows, got %d", count)
	}

	// 但日聚合照常记录——主链路不受影响
	var daily int64
	if err := db.Model(&dao.UsageDaily{}).Count(&daily).Error; err != nil {
		t.Fatalf("count daily: %v", err)
	}
	if daily != 1 {
		t.Errorf("usage_daily should still aggregate non-workflow calls, got %d rows", daily)
	}
}

// TestWorkflowUsage_FlushFailureDoesNotBreakAggregation 明细写入失败不影响日聚合。
func TestWorkflowUsage_FlushFailureDoesNotBreakAggregation(t *testing.T) {
	db := newWorkflowUsageTestDB(t)
	r := NewRecorder(db, zap.NewNop().Sugar())

	// 明细表被删除，写入必然失败
	if err := db.Migrator().DropTable(&dao.WorkflowUsage{}); err != nil {
		t.Fatalf("drop workflow_usage: %v", err)
	}

	err := r.flushBatch([]llm.UsageMetric{
		{
			BotID:      "bot1",
			Model:      "m",
			Feature:    "subagent",
			WorkflowID: "wf-A",
			NodeID:     "n1",
			Usage:      llm.Usage{TotalTokens: 100},
		},
	})
	if err != nil {
		t.Fatalf("flushBatch must not fail when only the detail write fails: %v", err)
	}

	// 日聚合仍然成功
	var rows []dao.UsageDaily
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0].TotalTokens != 100 {
		t.Errorf("daily aggregation should still succeed, got %+v", rows)
	}
}
