package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCountBotL0MemoryDoesNotBlockBootstrapForOtherBotsOrTiers(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bootstrap.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.TieredMemoryModel{}); err != nil {
		t.Fatal(err)
	}
	add := func(id string, tier int, kind, scope, metadata string) {
		t.Helper()
		if err := db.Create(&dao.TieredMemoryModel{
			ID: id, Tier: tier, ScopeKind: kind, ScopeID: scope,
			MetadataJSON: metadata, Content: "fictional memory", CreatedAt: time.Now(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	assertCount := func(botID string, want int64) {
		t.Helper()
		if n, err := countBotL0Memory(context.Background(), db, botID); err != nil || n != want {
			t.Fatalf("bot %q L0 count: n=%d want=%d err=%v", botID, n, want, err)
		}
	}
	// A different bot's L0 and our own derived tiers must not prevent bootstrap.
	add("other-l0", 0, "channel", "shared-channel", `{"bot_id":"b2"}`)
	add("own-l1", 1, "channel", "shared-channel", `{"bot_id":"b1"}`)
	add("own-l2", 2, "bot", "b1", "")
	add("own-l3", 3, "user", "shared-user", `{"bot_id":"b1"}`)
	add("unattributed-l0", 0, "channel", "shared-channel", "")
	add("conflicting-l0", 0, "bot", "b1", `{"bot_id":"b2"}`)
	assertCount("b1", 0)
	assertCount("b2", 1)
	assertCount("", 0)

	// Existing owned L0 still prevents duplicate bootstrap, including trusted
	// legacy bot scope rows that have no metadata owner.
	add("own-l0", 0, "channel", "shared-channel", `{"bot_id":"b1"}`)
	add("own-bot-l0", 0, "bot", "b1", "")
	assertCount("b1", 2)
	assertCount("b2", 1)
}
