package stages

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/storage"
	"github.com/kasuganosora/thinkbot/dao"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Exercise real persistence between automatic capture/write and prompt injection.
// A raw channel/user scope is an attribution category, not the sharing boundary.
func TestRecallStage_SQLiteAutomaticNotesShareOnlyWithinBot(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "recall.db")), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := dao.Migrate(db); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	logger := zap.NewNop().Sugar()
	tp := noop.NewTracerProvider()
	a := storage.NewSQLiteRepository(db, storage.SQLiteRepositoryConfig{BotID: "bot-a"})
	b := storage.NewSQLiteRepository(db, storage.SQLiteRepositoryConfig{BotID: "bot-b"})

	// Production capture -> ActionNote -> NoteHandler -> adapter -> bound SQLite.
	capture := NoteCaptureMiddleware("exchange", nil)(fakeReplyStage{text: "reply must not become a user memory"})
	incoming := core.NewEnvelope(core.Message{
		ID: "capture-1", BotID: "bot-a", Source: "wecom",
		Channel: "shared-private", UserID: "alice",
		Text: "channel-anchor 演示环境的日志主题是 demo_events",
	})
	captured, err := capture.Process(ctx, incoming)
	if err != nil {
		t.Fatal(err)
	}
	handler := outbound.NewNoteHandler(memory.NewNoteWriterAdapter(a, "bot-a"), logger, tp)
	var notes int
	for _, action := range captured.Actions() {
		if action.Type != core.ActionNote {
			continue
		}
		// Even incorrect action metadata must not replace the adapter's fixed owner.
		action.Metadata["bot_id"] = "bot-b"
		if err := handler.Handle(ctx, action); err != nil {
			t.Fatal(err)
		}
		notes++
	}
	if notes != 1 {
		t.Fatalf("expected one captured note, got %d", notes)
	}

	// The alternate automatic writer must preserve user scope and speaker metadata.
	userNote := core.NewEnvelope(core.Message{ID: "user-note-1", BotID: "bot-a", UserID: "alice"})
	userNote.AddAction(core.Action{
		Type: core.ActionNote, Payload: "user-anchor Alice 喜欢在发布之前确认数据库备份",
		Metadata: map[string]any{"speaker": "user", "bot_id": "bot-b"},
	})
	writer := memory.NewMemoryWriteStage("memory-write", a, tp, logger)
	if _, err := writer.Process(ctx, userNote); err != nil {
		t.Fatal(err)
	}

	// Another bot uses the exact same raw channel and user identifiers.
	otherHandler := outbound.NewNoteHandler(memory.NewNoteWriterAdapter(b, "bot-b"), logger, tp)
	if err := otherHandler.Handle(ctx, core.Action{
		Type: core.ActionNote, Channel: "shared-private", UserID: "alice",
		Payload:  "foreign-anchor 另一个机器人保存的独立项目资料",
		Metadata: map[string]any{"speaker": "user"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []memory.Scope{memory.ChannelScope("shared-private"), memory.UserScope("alice")} {
		if err := db.Create(&dao.EntryModel{
			ID: "legacy-" + string(scope.Kind), ScopeKind: string(scope.Kind), ScopeID: scope.ID,
			Content: "legacy-anchor 缺少机器人归属的旧记忆", CreatedAt: time.Now(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Verify actual stored attribution before checking retrieval. Neither writer
	// may flatten user/channel facts into the bot's own behavioral history.
	for _, scope := range []memory.Scope{memory.ChannelScope("shared-private"), memory.UserScope("alice")} {
		entries, err := a.Recent(ctx, scope, 10)
		if err != nil || len(entries) != 1 {
			t.Fatalf("stored %s entries = %+v, err = %v", scope.Key(), entries, err)
		}
		e := entries[0]
		if e.Scope != scope || e.Metadata["bot_id"] != "bot-a" || e.Metadata["user_id"] != "alice" || e.Metadata["speaker"] != "user" {
			t.Fatalf("stored attribution changed: %+v", e)
		}
	}

	// Recreate the reader over the persisted database, as happens after restart.
	reader := storage.NewSQLiteRepository(db, storage.SQLiteRepositoryConfig{BotID: "bot-a"})
	stage := NewRecallStage("memory-recall", reader, nil, logger)
	for _, tc := range []struct {
		name, source, channel, user string
	}{
		{"another private chat", "wecom", "wecom:dm:bob", "bob"},
		{"group chat", "wecom", "wecom:group:team", "charlie"},
		{"different platform", "web", "web:session", "diana"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := core.NewEnvelope(core.Message{BotID: "bot-a", Source: tc.source, Channel: tc.channel, UserID: tc.user})
			if _, err := stage.Process(ctx, env); err != nil {
				t.Fatal(err)
			}
			value, _ := env.Get(core.KVMemoryRecall)
			text, _ := value.(string)
			for _, want := range []string{"channel-anchor", "user-anchor"} {
				if !strings.Contains(text, want) {
					t.Fatalf("same bot's %s note was not recalled across conversations: %q", want, text)
				}
			}
			for _, excluded := range []string{"foreign-anchor", "legacy-anchor", "reply must not become"} {
				if strings.Contains(text, excluded) {
					t.Fatalf("unexpected memory %q injected: %q", excluded, text)
				}
			}
		})
	}

	otherStage := NewRecallStage("memory-recall", b, nil, logger)
	other := core.NewEnvelope(core.Message{BotID: "bot-b", Channel: "shared-private", UserID: "alice"})
	if _, err := otherStage.Process(ctx, other); err != nil {
		t.Fatal(err)
	}
	value, _ := other.Get(core.KVMemoryRecall)
	text, _ := value.(string)
	if !strings.Contains(text, "foreign-anchor") || strings.Contains(text, "channel-anchor") || strings.Contains(text, "user-anchor") || strings.Contains(text, "legacy-anchor") {
		t.Fatalf("second bot's recall crossed ownership: %q", text)
	}

	missing := core.NewEnvelope(core.Message{Channel: "shared-private", UserID: "alice"})
	if _, err := stage.Process(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if value, ok := missing.Get(core.KVMemoryRecall); ok {
		t.Fatalf("missing bot ID must not inject memory: %v", value)
	}
}
