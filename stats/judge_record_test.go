package stats

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// JudgeRecorder / JudgeSink — 判定结果落库
//
// 背景：判定结果此前只用于派生「参不参与」的决策，用完即弃——
// 改了 prompt、换了模型，无从判断变好还是变坏。本文件锁死落库行为。
// ============================================================================

func newJudgeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.JudgeRecord{}); err != nil {
		t.Fatalf("migrate judge_records: %v", err)
	}
	return db
}

func TestJudgeRecorder_Persists(t *testing.T) {
	db := newJudgeTestDB(t)
	r := NewJudgeRecorder(db, zap.NewNop().Sugar())

	r.Record(context.Background(), dao.JudgeRecord{
		BotID:     "bot1",
		Channel:   "telegram:123",
		Feature:   "engagement",
		Model:     "glm-4.6",
		Engage:    true,
		Score:     78,
		Reason:    "用户直接提问",
		Tier:      "llm",
		LatencyMS: 320,
	})
	r.SyncFlush()

	var got dao.JudgeRecord
	if err := db.First(&got).Error; err != nil {
		t.Fatalf("query judge record: %v", err)
	}

	if got.BotID != "bot1" {
		t.Errorf("BotID: got %q, want %q", got.BotID, "bot1")
	}
	if got.Channel != "telegram:123" {
		t.Errorf("Channel: got %q, want %q", got.Channel, "telegram:123")
	}
	if got.Model != "glm-4.6" {
		t.Errorf("Model: got %q, want %q", got.Model, "glm-4.6")
	}
	if !got.Engage {
		t.Error("Engage should be true")
	}
	if got.Score != 78 {
		t.Errorf("Score: got %d, want 78", got.Score)
	}
	if got.LatencyMS != 320 {
		t.Errorf("LatencyMS: got %d, want 320", got.LatencyMS)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be auto-filled")
	}
}

// TestJudgeRecorder_NoAggregation 判定是逐条明细，不做聚合。
//
// 这是与 UsageDaily 的关键区别：把这类明细塞进日聚合表会让它退化成明细表
// （行数爆炸、唯一索引语义被破坏）。本测试锁死「一次判定一行」。
func TestJudgeRecorder_NoAggregation(t *testing.T) {
	db := newJudgeTestDB(t)
	r := NewJudgeRecorder(db, zap.NewNop().Sugar())

	for i := 0; i < 5; i++ {
		r.Record(context.Background(), dao.JudgeRecord{
			BotID:   "bot1",
			Model:   "m",
			Channel: "c",
			Engage:  i%2 == 0,
			Score:   i * 10,
		})
	}
	r.SyncFlush()

	var count int64
	if err := db.Model(&dao.JudgeRecord{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5 detail rows (one per judgement), got %d", count)
	}
}

// TestJudgeRecorder_NilDB db 为 nil（纯内存模式）时不应 panic。
func TestJudgeRecorder_NilDB(t *testing.T) {
	r := NewJudgeRecorder(nil, zap.NewNop().Sugar())
	// 不应 panic，也不应阻塞
	r.Record(context.Background(), dao.JudgeRecord{BotID: "bot1", Engage: true})
	r.SyncFlush()
}

// TestJudgeRecorder_ChannelFullDoesNotBlock channel 打满时必须丢弃而非阻塞。
//
// 落库是旁路观测，绝不能把参与决策的主链路拖住。
func TestJudgeRecorder_ChannelFullDoesNotBlock(t *testing.T) {
	db := newJudgeTestDB(t)
	r := NewJudgeRecorder(db, zap.NewNop().Sugar())

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 远超 channel 容量（1024）地写入，若阻塞则本测试超时
		for i := 0; i < 3000; i++ {
			r.Record(context.Background(), dao.JudgeRecord{BotID: "bot1", Score: i})
		}
	}()

	select {
	case <-done:
		// 正常返回
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked when channel was full")
	}
}

func TestJudgeSink_MapsFields(t *testing.T) {
	db := newJudgeTestDB(t)
	rec := NewJudgeRecorder(db, zap.NewNop().Sugar())
	sink := NewJudgeSink(rec)

	sink.RecordJudge(context.Background(), engagement.JudgeRecord{
		BotID:     "bot2",
		Channel:   "web:s1",
		Model:     "gpt-4o-mini",
		Engage:    false,
		Score:     12,
		Reason:    "与 bot 兴趣无关",
		Tier:      "llm",
		LatencyMS: 88,
	})
	rec.SyncFlush()

	var got dao.JudgeRecord
	if err := db.First(&got).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.BotID != "bot2" || got.Channel != "web:s1" || got.Model != "gpt-4o-mini" {
		t.Errorf("dimension fields not mapped: %+v", got)
	}
	if got.Engage {
		t.Error("Engage should be false")
	}
	if got.Score != 12 {
		t.Errorf("Score: got %d, want 12", got.Score)
	}
	if got.Feature != "engagement" {
		t.Errorf("Feature: got %q, want %q", got.Feature, "engagement")
	}
	if got.LatencyMS != 88 {
		t.Errorf("LatencyMS: got %d, want 88", got.LatencyMS)
	}
}

// TestJudgeSink_TruncatesLongReason 超长理由必须截断。
// 理由由 LLM 生成、长度不可控，不截断会让写入随机失败。
func TestJudgeSink_TruncatesLongReason(t *testing.T) {
	db := newJudgeTestDB(t)
	rec := NewJudgeRecorder(db, zap.NewNop().Sugar())
	sink := NewJudgeSink(rec)

	long := strings.Repeat("很长的理由", 200) // 远超 512 列宽
	sink.RecordJudge(context.Background(), engagement.JudgeRecord{
		BotID:  "bot3",
		Reason: long,
	})
	rec.SyncFlush()

	var got dao.JudgeRecord
	if err := db.First(&got).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got.Reason) > 512 {
		t.Errorf("reason should be truncated to fit the column, got %d bytes", len(got.Reason))
	}
	if !strings.HasPrefix(got.Reason, "很长的理由") {
		t.Error("truncation should keep the beginning (conclusion usually comes first)")
	}
}

// TestJudgeSink_NilSafe nil sink 不应 panic——调用方可能未配置落库。
func TestJudgeSink_NilSafe(t *testing.T) {
	var sink *JudgeSink
	sink.RecordJudge(context.Background(), engagement.JudgeRecord{BotID: "bot"})

	empty := &JudgeSink{}
	empty.RecordJudge(context.Background(), engagement.JudgeRecord{BotID: "bot"})
}
