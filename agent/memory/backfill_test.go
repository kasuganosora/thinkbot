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

func TestBackfillFromChatHistory(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.ChatMessage{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	seed := []dao.ChatMessage{
		{BotID: "bot-x", UserID: "u1", SessionID: "s1", Role: dao.ChatRoleUser, Content: "我喜欢用 Go 语言", CreatedAt: now.Add(-48 * time.Hour)},
		{BotID: "bot-x", UserID: "u1", SessionID: "s1", Role: dao.ChatRoleAssistant, Content: "好的，Go 很适合后端开发", CreatedAt: now.Add(-47 * time.Hour)},
		{BotID: "bot-x", UserID: "u2", Role: dao.ChatRoleUser, Content: "用 Docker 部署应用", CreatedAt: now.Add(-10 * time.Hour)},
		{BotID: "other", UserID: "z", Role: dao.ChatRoleUser, Content: "噪声不应被灌入", CreatedAt: now},
		{BotID: "bot-x", UserID: "u3", Role: dao.ChatRoleUser, Content: "", CreatedAt: now}, // 空内容跳过
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatal(err)
	}

	// 纯内存 store（不持久化），专注验证 L0 填充与 bot 过滤
	ts := NewTieredStore(nil)
	store := NewTieredStoreAdapter(ts)
	written, maxID, err := BackfillFromChatHistory(context.Background(), store, db, "bot-x", 0, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if written != 3 {
		t.Fatalf("expected 3 written (bot-x only, empty content skipped), got %d", written)
	}

	l0, err := ts.Retrieve(context.Background(), Tier0Working, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(l0) != 3 {
		t.Fatalf("expected 3 L0 entries, got %d", len(l0))
	}
	for _, e := range l0 {
		if e.Source != "chat_history" {
			t.Fatalf("expected source chat_history, got %s", e.Source)
		}
		// 必须以当前时间写入，否则会被 dreaming 的 LookbackDays/ActiveThreshold 跳过
		if e.CreatedAt.Before(now.Add(-time.Minute)) {
			t.Fatalf("backfilled entry must use recent CreatedAt, got %v", e.CreatedAt)
		}
		if e.Metadata["chat_message_id"] == nil {
			t.Fatalf("backfilled entry missing chat_message_id metadata")
		}
	}

	// 空消息表的 bot 不应写入任何内容
	store2 := NewTieredStoreAdapter(NewTieredStore(nil))
	if n, _, err := BackfillFromChatHistory(context.Background(), store2, db, "nobody", 0, zap.NewNop().Sugar()); err != nil || n != 0 {
		t.Fatalf("expected 0 written for unknown bot, got n=%d err=%v", n, err)
	}

	// 增量幂等：以 maxID 为水位线再次回灌，不应重复写入任何内容
	store3 := NewTieredStoreAdapter(NewTieredStore(nil))
	if n, wm, err := BackfillFromChatHistory(context.Background(), store3, db, "bot-x", maxID, zap.NewNop().Sugar()); err != nil || n != 0 {
		t.Fatalf("incremental backfill above watermark should write 0, got n=%d wm=%d err=%v", n, wm, err)
	} else if wm != maxID {
		t.Fatalf("incremental backfill should preserve watermark, got %d want %d", wm, maxID)
	}
}
