package memory

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestSeedAndBackfillFromEventStream(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.ChatMessage{}, &dao.UserMessageEvent{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seed := []dao.ChatMessage{
		{BotID: "bot-x", UserID: "u1", SessionID: "s1", Role: dao.ChatRoleUser, Content: "我喜欢用 Go 语言", CreatedAt: now.Add(-48 * time.Hour)},
		{BotID: "bot-x", UserID: "u1", SessionID: "s1", Role: dao.ChatRoleAssistant, Content: "Go 很适合后端", CreatedAt: now.Add(-47 * time.Hour)},
		{BotID: "bot-x", UserID: "u2", Role: dao.ChatRoleUser, Content: "用 Docker 部署", CreatedAt: now.Add(-10 * time.Hour)},
		{BotID: "other", UserID: "z", Role: dao.ChatRoleUser, Content: "噪声", CreatedAt: now},
		{BotID: "bot-x", UserID: "u3", Role: dao.ChatRoleUser, Content: "", CreatedAt: now}, // 空内容不进事件流
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatal(err)
	}

	// 1) 一次性 seed：应为 bot-x 的 2 条 user 消息（空内容那条跳过）
	seeded, err := SeedUserMessageEvents(context.Background(), db, "bot-x", zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if seeded != 2 {
		t.Fatalf("expected 2 seeded, got %d", seeded)
	}
	// 2) 二次 seed 应被 guard 跳过（事件流已非空）
	seeded2, err := SeedUserMessageEvents(context.Background(), db, "bot-x", zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if seeded2 != 0 {
		t.Fatalf("second seed should be skipped (guard), got %d", seeded2)
	}
	// 事件流不应含其它 bot
	var otherCnt int64
	if err := db.Model(&dao.UserMessageEvent{}).Where("bot_id = ?", "other").Count(&otherCnt).Error; err != nil {
		t.Fatal(err)
	}
	if otherCnt != 0 {
		t.Fatalf("event stream must not contain other bot, got %d", otherCnt)
	}

	// 3) 从事件流回灌 L0
	ts := NewTieredStore(nil)
	store := NewTieredStoreAdapter(ts)
	written, maxID, err := BackfillFromChatHistory(context.Background(), store, NewDBUserMessageSource(db), "bot-x", 0, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if written != 2 {
		t.Fatalf("expected 2 written from event stream, got %d", written)
	}
	if maxID == 0 {
		t.Fatalf("expected non-zero watermark")
	}

	// 4) 增量：运行期新写入一条事件流消息，以 maxID 为水位线回灌应只取新增的
	if err := db.Create(&dao.UserMessageEvent{
		BotID: "bot-x", Channel: "s2", UserID: "u4", MessageID: "m-new", Content: "新增的发言", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	ts2 := NewTieredStore(nil)
	store2 := NewTieredStoreAdapter(ts2)
	w2, wm2, err := BackfillFromChatHistory(context.Background(), store2, NewDBUserMessageSource(db), "bot-x", maxID, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if w2 != 1 {
		t.Fatalf("incremental backfill should write only the newly-added event, got %d", w2)
	}
	if wm2 <= maxID {
		t.Fatalf("incremental watermark should advance past old maxID %d, got %d", maxID, wm2)
	}
}
