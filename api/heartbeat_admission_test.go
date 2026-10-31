package api

import (
	"context"
	"encoding/json"
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
	add := func(id, kind, scopeID, source, content string, owner ...string) {
		metadata := ""
		if len(owner) > 0 {
			b, _ := json.Marshal(map[string]string{"bot_id": owner[0]})
			metadata = string(b)
		}
		if err := db.Create(&dao.EntryModel{ID: id, ScopeKind: kind, ScopeID: scopeID, Source: source, Content: content, MetadataJSON: metadata, CreatedAt: later}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// noise: other bot, internal jobs, heartbeat itself
	add("o1", "bot", "b2", "note", "other bot")
	add("o2", "channel", "chat-2", "note", "other bot channel")
	add("i1", "bot", "b1", "compactor", "merged")
	add("i2", "bot", "b1", "dreaming", "dream")
	add("i3", "channel", "heartbeat", "note", "hb note", "b1")
	add("i4", "user", "system:heartbeat", "note", "hb user", "b1")
	add("xch:i3", "bot", "b1", "note", "[heartbeat] hb note")
	// Both bots can observe identical raw channel/user IDs. Neither another
	// owner's entries nor unattributed legacy entries may wake b1.
	add("foreign-channel", "channel", "chat-1", "note", "shared raw channel", "b2")
	add("foreign-user", "user", "u1", "note", "shared raw user", "b2")
	add("legacy-channel", "channel", "chat-1", "note", "unknown owner")
	add("legacy-user", "user", "u1", "note", "unknown owner")
	add("conflicting-bot", "bot", "b1", "note", "conflicting ownership", "b2")
	if n, err := countHeartbeatMemorySignals(context.Background(), db, "b1", since); err != nil || n != 0 {
		t.Fatalf("internal/other-bot entries must not count: n=%d err=%v", n, err)
	}
	add("s1", "channel", "chat-1", "note", "real inbound", "b1")
	add("s2", "user", "u1", "tool", "user memory", "b1")
	add("s3", "bot", "b1", "note", "bot memory")
	// Proven ownership does not depend on a matching historical raw scope.
	add("s4", "channel", "new-chat", "note", "new conversation", "b1")
	for botID, want := range map[string]int64{"b1": 4, "b2": 3, "": 0} {
		if n, err := countHeartbeatMemorySignals(context.Background(), db, botID, since); err != nil || n != want {
			t.Fatalf("bot %q signals: n=%d want=%d err=%v", botID, n, want, err)
		}
	}
}
