package dao

import (
	"strconv"
	"testing"
	"time"
)

// TestMessageCountRecount2_RegisteredAndIdempotent: the 2026-10-20 recount is
// registered after the notify move, fixes drift (incl. session 22's +4 case)
// and re-running changes nothing.
func TestMessageCountRecount2_RegisteredAndIdempotent(t *testing.T) {
	notifyIdx, recountIdx := -1, -1
	for i, m := range dataMigrations {
		switch m.name {
		case NotifyLegacySessionMoveName:
			notifyIdx = i
		case messageCountRecount2Name:
			recountIdx = i
		}
	}
	if recountIdx < 0 || recountIdx < notifyIdx {
		t.Fatalf("recount must be registered after notify move: notify=%d recount=%d", notifyIdx, recountIdx)
	}

	db := newMigrationTestDB(t)
	now := time.Now()
	a := ChatSession{BotID: "b", Title: "a", Status: SessionStatusActive, MessageCount: 7, CreatedAt: now, UpdatedAt: now}
	e := ChatSession{BotID: "b", Title: "e", Status: SessionStatusActive, MessageCount: 2, CreatedAt: now, UpdatedAt: now}
	db.Create(&a)
	db.Create(&e)
	for i := 0; i < 3; i++ {
		db.Create(&ChatMessage{BotID: "b", UserID: "u", SessionID: strconv.FormatUint(a.ID, 10), Role: ChatRoleUser, Content: "x", CreatedAt: now})
	}
	for run := 0; run < 2; run++ {
		if _, err := recountSessionStats(db); err != nil {
			t.Fatal(err)
		}
		var got []ChatSession
		db.Order("id").Find(&got)
		if got[0].MessageCount != 3 || got[1].MessageCount != 0 {
			t.Fatalf("run %d: counts %d,%d want 3,0", run, got[0].MessageCount, got[1].MessageCount)
		}
	}
}

func TestRecountSession_IgnoresLegacyKeys(t *testing.T) {
	db := newMigrationTestDB(t)
	if err := RecountSession(db, "tg:76017910"); err != nil {
		t.Fatal(err)
	}
}
