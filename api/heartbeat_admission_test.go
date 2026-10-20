package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestCountHeartbeatMemorySignals(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "hb.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	later := time.Now()
	db.Create(&dao.UserMessageEvent{BotID: "b1", Channel: "chat-1", UserID: "u1", MessageID: "m1", CreatedAt: since.Add(-time.Hour)})
	db.Create(&dao.UserMessageEvent{BotID: "b2", Channel: "chat-2", UserID: "u2", MessageID: "m2", CreatedAt: since.Add(-time.Hour)})
	add := func(id, kind, scopeID, source, content string) {
		if err := db.Create(&dao.EntryModel{ID: id, ScopeKind: kind, ScopeID: scopeID, Source: source, Content: content, CreatedAt: later}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// noise: other bot, internal jobs, heartbeat itself
	add("o1", "bot", "b2", "note", "other bot")
	add("o2", "channel", "chat-2", "note", "other bot channel")
	add("i1", "bot", "b1", "compactor", "merged")
	add("i2", "bot", "b1", "dreaming", "dream")
	add("i3", "channel", "heartbeat", "note", "hb note")
	add("i4", "user", "system:heartbeat", "note", "hb user")
	add("xch:i3", "bot", "b1", "note", "[heartbeat] hb note")
	if n, err := countHeartbeatMemorySignals(context.Background(), db, "b1", since); err != nil || n != 0 {
		t.Fatalf("internal/other-bot entries must not count: n=%d err=%v", n, err)
	}
	add("s1", "channel", "chat-1", "note", "real inbound")
	add("s2", "user", "u1", "tool", "user memory")
	add("s3", "bot", "b1", "note", "bot memory")
	if n, _ := countHeartbeatMemorySignals(context.Background(), db, "b1", since); n != 3 {
		t.Fatalf("own scopes must count: n=%d", n)
	}
}
