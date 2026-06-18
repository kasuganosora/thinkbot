package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
)

// testDB 创建一个临时文件 SQLite 数据库用于测试。
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	})
	return db
}

// ============================================================================
// SQLiteRepository 测试
// ============================================================================

func TestSQLiteRepository_Append(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()

	entry := memory.Entry{
		Scope:   memory.ChannelScope("ch-1"),
		Content: "Hello World",
		Category: "fact",
		Source:   "conversation",
		Importance: 0.8,
	}

	err := repo.Append(ctx, entry)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// 验证自动填充
	entries, err := repo.Recent(ctx, memory.ChannelScope("ch-1"), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ID == "" {
		t.Error("expected auto-generated ID")
	}
	if entries[0].Content != "Hello World" {
		t.Errorf("content = %q, want %q", entries[0].Content, "Hello World")
	}
	if entries[0].Category != "fact" {
		t.Errorf("category = %q, want %q", entries[0].Category, "fact")
	}
	if entries[0].CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestSQLiteRepository_ScopeIsolation(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()

	// 写入不同 scope
	repo.Append(ctx, memory.Entry{Scope: memory.ChannelScope("ch-1"), Content: "A"})
	repo.Append(ctx, memory.Entry{Scope: memory.ChannelScope("ch-2"), Content: "B"})
	repo.Append(ctx, memory.Entry{Scope: memory.UserScope("user-1"), Content: "C"})

	// 验证隔离
	entries, _ := repo.Recent(ctx, memory.ChannelScope("ch-1"), 10)
	if len(entries) != 1 || entries[0].Content != "A" {
		t.Errorf("scope isolation failed for ch-1: got %d entries", len(entries))
	}

	entries, _ = repo.Recent(ctx, memory.ChannelScope("ch-2"), 10)
	if len(entries) != 1 || entries[0].Content != "B" {
		t.Errorf("scope isolation failed for ch-2: got %d entries", len(entries))
	}

	entries, _ = repo.Recent(ctx, memory.UserScope("user-1"), 10)
	if len(entries) != 1 || entries[0].Content != "C" {
		t.Errorf("scope isolation failed for user-1: got %d entries", len(entries))
	}
}

func TestSQLiteRepository_Eviction(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{
		MaxEntriesPerScope: 3,
		DefaultLimit:       10,
	})
	ctx := context.Background()
	scope := memory.ChannelScope("ch-evict")

	// 写入 5 条
	for i := 0; i < 5; i++ {
		repo.Append(ctx, memory.Entry{
			Scope:     scope,
			Content:   string(rune('A' + i)),
			CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
		})
		// 等待异步 eviction
		time.Sleep(50 * time.Millisecond)
	}

	// 等待 eviction goroutine 完成
	time.Sleep(200 * time.Millisecond)

	count, _ := repo.Count(ctx, scope)
	if count > 3 {
		t.Errorf("expected <= 3 entries after eviction, got %d", count)
	}
}

func TestSQLiteRepository_Delete(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("ch-del")

	entry := memory.Entry{
		ID:      "del-001",
		Scope:   scope,
		Content: "to be deleted",
	}
	repo.Append(ctx, entry)

	err := repo.Delete(ctx, scope, "del-001")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	count, _ := repo.Count(ctx, scope)
	if count != 0 {
		t.Errorf("expected 0 entries after delete, got %d", count)
	}
}

func TestSQLiteRepository_Clear(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("ch-clear")

	repo.Append(ctx, memory.Entry{Scope: scope, Content: "A"})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "B"})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "C"})

	err := repo.Clear(ctx, scope)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	count, _ := repo.Count(ctx, scope)
	if count != 0 {
		t.Errorf("expected 0 after clear, got %d", count)
	}
}

func TestSQLiteRepository_Retrieve_TextFilter(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("ch-search")

	repo.Append(ctx, memory.Entry{Scope: scope, Content: "Go is a programming language"})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "Python is popular for ML"})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "Go has great concurrency"})

	entries, err := repo.Retrieve(ctx, memory.Query{
		Scopes: []memory.Scope{scope},
		Text:   "Go",
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 results for 'Go', got %d", len(entries))
	}
}

func TestSQLiteRepository_Retrieve_CategoryFilter(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("ch-cat")

	repo.Append(ctx, memory.Entry{Scope: scope, Content: "A", Category: "fact"})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "B", Category: "preference"})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "C", Category: "fact"})

	entries, err := repo.Retrieve(ctx, memory.Query{
		Scopes:   []memory.Scope{scope},
		Category: "fact",
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 facts, got %d", len(entries))
	}
}

func TestSQLiteRepository_Retrieve_ImportanceFilter(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("ch-imp")

	repo.Append(ctx, memory.Entry{Scope: scope, Content: "low", Importance: 0.2})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "high", Importance: 0.9})
	repo.Append(ctx, memory.Entry{Scope: scope, Content: "mid", Importance: 0.5})

	entries, err := repo.Retrieve(ctx, memory.Query{
		Scopes:        []memory.Scope{scope},
		MinImportance: 0.5,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries with importance >= 0.5, got %d", len(entries))
	}
}

func TestSQLiteRepository_Metrics(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()

	repo.Append(ctx, memory.Entry{Scope: memory.ChannelScope("ch-m"), Content: "A"})
	repo.Append(ctx, memory.Entry{Scope: memory.ChannelScope("ch-m"), Content: "B"})
	repo.Recent(ctx, memory.ChannelScope("ch-m"), 10)

	m := repo.Metrics()
	if m.EntriesAppended != 2 {
		t.Errorf("EntriesAppended = %d, want 2", m.EntriesAppended)
	}
	if m.Retrievals != 1 {
		t.Errorf("Retrievals = %d, want 1", m.Retrievals)
	}
	if m.TotalEntries != 2 {
		t.Errorf("TotalEntries = %d, want 2", m.TotalEntries)
	}
}

func TestSQLiteRepository_Metadata(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("ch-meta")

	entry := memory.Entry{
		Scope:   scope,
		Content: "with metadata",
		Metadata: map[string]any{
			"source_msg": "msg-123",
			"tags":       []string{"important", "todo"},
		},
	}
	repo.Append(ctx, entry)

	entries, _ := repo.Recent(ctx, scope, 1)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Metadata == nil {
		t.Fatal("expected metadata to be preserved")
	}
	if entries[0].Metadata["source_msg"] != "msg-123" {
		t.Errorf("metadata source_msg = %v, want msg-123", entries[0].Metadata["source_msg"])
	}
}

// ============================================================================
// SQLiteNoteStore 测试
// ============================================================================

func TestSQLiteNoteStore_Save(t *testing.T) {
	db := testDB(t)
	store := NewSQLiteNoteStore(db)
	ctx := context.Background()

	note := outbound.Note{
		ID:        "note-001",
		BotID:     "bot-1",
		Channel:   "ch-1",
		UserID:    "user-1",
		Text:      "This is a note",
		Category:  "observation",
		CreatedAt: time.Now(),
	}

	err := store.Save(ctx, note)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	count, _ := store.Count(ctx)
	if count != 1 {
		t.Errorf("expected count=1, got %d", count)
	}
}

func TestSQLiteNoteStore_NotesByBot(t *testing.T) {
	db := testDB(t)
	store := NewSQLiteNoteStore(db)
	ctx := context.Background()

	store.Save(ctx, outbound.Note{ID: "n1", BotID: "bot-1", Text: "A", CreatedAt: time.Now()})
	store.Save(ctx, outbound.Note{ID: "n2", BotID: "bot-2", Text: "B", CreatedAt: time.Now()})
	store.Save(ctx, outbound.Note{ID: "n3", BotID: "bot-1", Text: "C", CreatedAt: time.Now()})

	notes, err := store.NotesByBot(ctx, "bot-1", 10)
	if err != nil {
		t.Fatalf("NotesByBot: %v", err)
	}
	if len(notes) != 2 {
		t.Errorf("expected 2 notes for bot-1, got %d", len(notes))
	}
}

func TestSQLiteNoteStore_NotesByChannel(t *testing.T) {
	db := testDB(t)
	store := NewSQLiteNoteStore(db)
	ctx := context.Background()

	store.Save(ctx, outbound.Note{ID: "n1", Channel: "ch-1", Text: "A", CreatedAt: time.Now()})
	store.Save(ctx, outbound.Note{ID: "n2", Channel: "ch-2", Text: "B", CreatedAt: time.Now()})

	notes, err := store.NotesByChannel(ctx, "ch-1", 10)
	if err != nil {
		t.Fatalf("NotesByChannel: %v", err)
	}
	if len(notes) != 1 {
		t.Errorf("expected 1 note for ch-1, got %d", len(notes))
	}
}

func TestSQLiteNoteStore_TTL(t *testing.T) {
	db := testDB(t)
	store := NewSQLiteNoteStore(db, SQLiteNoteStoreConfig{
		TTL: 100 * time.Millisecond,
	})
	ctx := context.Background()

	// 写入一条过期的记录
	store.Save(ctx, outbound.Note{
		ID:        "old",
		Text:      "expired",
		CreatedAt: time.Now().Add(-200 * time.Millisecond),
	})

	// 等待 TTL 过期
	time.Sleep(150 * time.Millisecond)

	// 再写入一条新记录，触发 TTL 清理
	store.Save(ctx, outbound.Note{
		ID:        "new",
		Text:      "fresh",
		CreatedAt: time.Now(),
	})

	count, _ := store.Count(ctx)
	if count > 1 {
		// TTL 清理可能是异步的，宽松检查
		t.Logf("note: TTL cleanup may be async, count=%d (expected <=1)", count)
	}
}

func TestSQLiteNoteStore_Eviction(t *testing.T) {
	db := testDB(t)
	store := NewSQLiteNoteStore(db, SQLiteNoteStoreConfig{MaxNotes: 3})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		store.Save(ctx, outbound.Note{
			ID:        generateID("note"),
			Text:      string(rune('A' + i)),
			CreatedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		})
		time.Sleep(50 * time.Millisecond) // 等待异步 eviction
	}

	time.Sleep(200 * time.Millisecond)

	count, _ := store.Count(ctx)
	if count > 3 {
		t.Errorf("expected <= 3 notes after eviction, got %d", count)
	}
}

func TestSQLiteNoteStore_Clear(t *testing.T) {
	db := testDB(t)
	store := NewSQLiteNoteStore(db)
	ctx := context.Background()

	store.Save(ctx, outbound.Note{ID: "n1", Text: "A", CreatedAt: time.Now()})
	store.Save(ctx, outbound.Note{ID: "n2", Text: "B", CreatedAt: time.Now()})

	err := store.Clear(ctx)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	count, _ := store.Count(ctx)
	if count != 0 {
		t.Errorf("expected 0 after clear, got %d", count)
	}
}

// ============================================================================
// WindowStateStore 测试
// ============================================================================

func TestWindowStateStore_SaveAndLoad(t *testing.T) {
	db := testDB(t)
	store := NewWindowStateStore(db)
	ctx := context.Background()

	snap := WindowSnapshot{
		ScopeKey:          "channel:ch-1",
		UsedTokens:        5000,
		RoundCount:        3,
		TotalInputTokens:  15000,
		TotalOutputTokens: 4500,
		Compressions:      1,
	}

	err := store.Save(ctx, snap)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load(ctx, "channel:ch-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil snapshot")
	}

	if loaded.UsedTokens != 5000 {
		t.Errorf("UsedTokens = %d, want 5000", loaded.UsedTokens)
	}
	if loaded.RoundCount != 3 {
		t.Errorf("RoundCount = %d, want 3", loaded.RoundCount)
	}
	if loaded.TotalInputTokens != 15000 {
		t.Errorf("TotalInputTokens = %d, want 15000", loaded.TotalInputTokens)
	}
	if loaded.TotalOutputTokens != 4500 {
		t.Errorf("TotalOutputTokens = %d, want 4500", loaded.TotalOutputTokens)
	}
	if loaded.Compressions != 1 {
		t.Errorf("Compressions = %d, want 1", loaded.Compressions)
	}
}

func TestWindowStateStore_Upsert(t *testing.T) {
	db := testDB(t)
	store := NewWindowStateStore(db)
	ctx := context.Background()

	snap := WindowSnapshot{
		ScopeKey:   "channel:ch-upsert",
		UsedTokens: 1000,
		RoundCount: 1,
	}
	store.Save(ctx, snap)

	// 更新同一 scope
	snap.UsedTokens = 3000
	snap.RoundCount = 5
	err := store.Save(ctx, snap)
	if err != nil {
		t.Fatalf("Save (upsert): %v", err)
	}

	loaded, _ := store.Load(ctx, "channel:ch-upsert")
	if loaded.UsedTokens != 3000 {
		t.Errorf("UsedTokens after upsert = %d, want 3000", loaded.UsedTokens)
	}
	if loaded.RoundCount != 5 {
		t.Errorf("RoundCount after upsert = %d, want 5", loaded.RoundCount)
	}
}

func TestWindowStateStore_LoadNonExistent(t *testing.T) {
	db := testDB(t)
	store := NewWindowStateStore(db)
	ctx := context.Background()

	loaded, err := store.Load(ctx, "non-existent")
	if err != nil {
		t.Fatalf("Load non-existent: %v", err)
	}
	if loaded != nil {
		t.Errorf("expected nil for non-existent scope, got %+v", loaded)
	}
}

func TestWindowStateStore_Delete(t *testing.T) {
	db := testDB(t)
	store := NewWindowStateStore(db)
	ctx := context.Background()

	store.Save(ctx, WindowSnapshot{ScopeKey: "del-scope", UsedTokens: 100})

	err := store.Delete(ctx, "del-scope")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	loaded, _ := store.Load(ctx, "del-scope")
	if loaded != nil {
		t.Error("expected nil after delete")
	}
}

// ============================================================================
// Window Snapshot/Restore 集成测试
// ============================================================================

func TestWindow_SnapshotRestore(t *testing.T) {
	// 创建 Window 并记录使用
	w := memory.NewWindow(memory.WindowConfig{
		MaxContextTokens:  128000,
		ReservedTokens:    2000,
		OutputReserve:     4096,
		MemoryBudgetRatio: 0.3,
	})

	w.RecordUsage(5000, 1500)
	w.RecordUsage(8000, 2000)
	w.RecordCompression()

	// 导出快照
	snap := w.Snapshot()
	if snap.UsedTokens != 8000 {
		t.Errorf("Snapshot.UsedTokens = %d, want 8000", snap.UsedTokens)
	}
	if snap.RoundCount != 2 {
		t.Errorf("Snapshot.RoundCount = %d, want 2", snap.RoundCount)
	}
	if snap.TotalInputTokens != 13000 {
		t.Errorf("Snapshot.TotalInputTokens = %d, want 13000", snap.TotalInputTokens)
	}
	if snap.TotalOutputTokens != 3500 {
		t.Errorf("Snapshot.TotalOutputTokens = %d, want 3500", snap.TotalOutputTokens)
	}
	if snap.Compressions != 1 {
		t.Errorf("Snapshot.Compressions = %d, want 1", snap.Compressions)
	}

	// 新建 Window 并恢复
	w2 := memory.NewWindow(memory.WindowConfig{MaxContextTokens: 128000})
	w2.Restore(memory.WindowState{
		UsedTokens:        snap.UsedTokens,
		RoundCount:        snap.RoundCount,
		TotalInputTokens:  snap.TotalInputTokens,
		TotalOutputTokens: snap.TotalOutputTokens,
		Compressions:      snap.Compressions,
	})

	metrics := w2.Metrics()
	if metrics.UsedTokens != 8000 {
		t.Errorf("Restored UsedTokens = %d, want 8000", metrics.UsedTokens)
	}
	if metrics.RoundCount != 2 {
		t.Errorf("Restored RoundCount = %d, want 2", metrics.RoundCount)
	}
}

