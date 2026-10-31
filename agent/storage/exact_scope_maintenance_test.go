package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"go.uber.org/zap"
)

func seedOwnedClassifications(t *testing.T, repo *SQLiteRepository) {
	t.Helper()
	for _, scope := range []memory.Scope{memory.UserScope("u"), memory.ChannelScope("c"), memory.BotScope("a")} {
		for i := 1; i <= 2; i++ {
			model := entryToModel(memory.Entry{
				ID: fmt.Sprintf("%s-%d", scope.Kind, i), Scope: scope,
				Content: "fictional original " + scope.Key(), Source: "note",
				Metadata: map[string]any{"bot_id": "a"}, CreatedAt: time.Now().Add(-time.Hour),
			})
			if err := repo.db.Create(&model).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
}

func assertOriginalUserAndChannelRows(t *testing.T, repo *SQLiteRepository) {
	t.Helper()
	for _, scope := range []memory.Scope{memory.UserScope("u"), memory.ChannelScope("c")} {
		var rows []dao.EntryModel
		if err := repo.db.Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("%s original rows changed: %+v", scope.Key(), rows)
		}
		for _, row := range rows {
			e := modelToEntry(row)
			if e.Content != "fictional original "+scope.Key() || e.Source != "note" || row.DedupCheckedAt != nil || isEntryArchived(e.Metadata) || isEntryCompactSkipped(e.Metadata) {
				t.Fatalf("bot maintenance changed source classification/state: %+v", row)
			}
		}
	}
}

func TestBotMirrorEvictionPreservesOtherScopeCapacity(t *testing.T) {
	repo := NewSQLiteRepository(testDB(t), SQLiteRepositoryConfig{BotID: "a", MaxEntriesPerScope: 2})
	seedOwnedClassifications(t, repo)
	// All three scopes are exactly at capacity. The mirror exceeds only the
	// bot scope and is the first eviction candidate under the existing policy.
	if err := repo.Append(context.Background(), memory.Entry{ID: "xch:mirror", Scope: memory.BotScope("a"), Content: "channel mirror"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int64
		if err := repo.db.Model(&dao.EntryModel{}).Where("id = ?", "xch:mirror").Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break // the asynchronous eviction has completed its delete
		}
		if time.Now().After(deadline) {
			t.Fatal("mirror eviction did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertOriginalUserAndChannelRows(t, repo)
	if n, err := repo.Count(context.Background(), memory.BotScope("a")); err != nil || n != 6 {
		t.Fatalf("shared bot read lost entries: count=%d err=%v", n, err)
	}
}

func TestBotCompactionPreservesOtherScopeClassification(t *testing.T) {
	ctx := context.Background()
	repo := NewSQLiteRepository(testDB(t), SQLiteRepositoryConfig{BotID: "a"})
	seedOwnedClassifications(t, repo)
	provider := &mockClusterProvider{}
	compactor := NewSQLiteCompactor(SQLiteCompactorConfig{Provider: provider, Model: &llm.Model{ID: "mock"}}, zap.NewNop().Sugar())
	compactor.SetRepository(repo)
	if err := compactor.CompactScope(ctx, memory.BotScope("a")); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("expected one bot-scope merge, calls=%d", provider.calls)
	}
	assertOriginalUserAndChannelRows(t, repo)
	active, err := repo.GetAllActive(ctx, memory.BotScope("a"))
	if err != nil || len(active) != 1 || active[0].Scope != memory.BotScope("a") || active[0].Content != "MERGED-2" {
		t.Fatalf("bot compaction did not preserve its scope: %+v %v", active, err)
	}
	// ID-targeted maintenance must not silently expand a bot read alias either.
	if repo.ArchiveByID(ctx, memory.BotScope("a"), "user-1") {
		t.Fatal("bot archive targeted a user entry")
	}
	if n := repo.MarkCompactSkipped(ctx, memory.BotScope("a"), []memory.Entry{{ID: "channel-1"}}, "test"); n != 0 {
		t.Fatal("bot compact skip targeted a channel entry")
	}
	repo.deleteEvicted(ctx, memory.BotScope("a"), []string{"user-2", "channel-2"}, "test")
	assertOriginalUserAndChannelRows(t, repo)
	if err := repo.Clear(ctx, memory.BotScope("a")); err != nil {
		t.Fatal(err)
	}
	assertOriginalUserAndChannelRows(t, repo)
}
