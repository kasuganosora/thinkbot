package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

func TestNoteWriterAdapter_BindsBotOwnership(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	writer := NewNoteWriterAdapter(repo, "owner")
	note := outbound.NoteEntry{
		ScopeKind: "global", ScopeID: "other",
		Content: "记住部署前需要先确认数据库备份", Category: "fact",
		Metadata: map[string]any{"bot_id": "other", "source_message": "m1"},
	}
	if err := writer.WriteNote(ctx, note); err != nil {
		t.Fatal(err)
	}
	entries, err := repo.Retrieve(ctx, Query{Limit: 10})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, err = %v", entries, err)
	}
	if entries[0].Scope != BotScope("owner") || entries[0].Metadata["bot_id"] != "owner" {
		t.Fatalf("caller-controlled scope changed ownership: %+v", entries[0])
	}
	if entries[0].Metadata["source_message"] != "m1" || note.Metadata["bot_id"] != "other" {
		t.Fatal("metadata must be preserved without mutating the caller's map")
	}
	if err := NewNoteWriterAdapter(repo, "").WriteNote(ctx, note); err == nil {
		t.Fatal("missing bot ID must reject the write even if note metadata contains one")
	}
	entries, _ = repo.Retrieve(ctx, Query{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("unowned note was written: %+v", entries)
	}
}

func TestNoteWriterAdapter_PreservesAttributionScopes(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	writer := NewNoteWriterAdapter(repo, "owner")
	for _, scope := range []Scope{ChannelScope("private-chat"), UserScope("alice")} {
		note := outbound.NoteEntry{
			ScopeKind: string(scope.Kind), ScopeID: scope.ID, Content: "记住发布前确认数据库备份",
			Metadata: map[string]any{"bot_id": "other", "user_id": "alice", "speaker": "user"},
		}
		if err := writer.WriteNote(ctx, note); err != nil {
			t.Fatal(err)
		}
		entries, err := repo.Recent(ctx, scope, 10)
		if err != nil || len(entries) != 1 {
			t.Fatalf("entries=%+v err=%v", entries, err)
		}
		entry := entries[0]
		if entry.Scope != scope || entry.Metadata["bot_id"] != "owner" || entry.Metadata["user_id"] != "alice" || entry.Metadata["speaker"] != "user" {
			t.Fatalf("attribution or ownership lost: %+v", entry)
		}
	}
}

func TestMemoryStages_UseBotOwnership(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	writer := NewMemoryWriteStage("memory-write", repo, noop.NewTracerProvider(), zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{BotID: "owner", Channel: "private-chat", UserID: "alice"})
	env.AddAction(core.Action{Type: core.ActionNote, Payload: "shared-note 记得每次发布前确认备份", Metadata: map[string]any{"bot_id": "other", "speaker": "user"}})
	if _, err := writer.Process(ctx, env); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []Entry{
		{Scope: BotScope("other"), Content: "foreign-note 另一个机器人记下的事实"},
		{Scope: ChannelScope("group-chat"), Content: "legacy-note 旧会话未确认归属的事实"},
		{Scope: UserScope("bob"), Content: "legacy-note 旧用户未确认归属的事实"},
	} {
		if err := repo.Append(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := repo.Retrieve(ctx, Query{Limit: 100})
	if err != nil || len(entries) != 5 {
		t.Fatalf("entries = %+v, err = %v", entries, err)
	}
	for _, e := range entries {
		if !strings.Contains(e.Content, "shared-note") {
			continue
		}
		if e.Metadata["bot_id"] != "owner" || e.Metadata["user_id"] != "alice" || e.Metadata["speaker"] != "user" {
			t.Fatalf("write lost authoritative ownership or user attribution: %+v", e)
		}
		if e.Scope != ChannelScope("private-chat") && e.Scope != UserScope("alice") {
			t.Fatalf("write lost channel/user classification: %+v", e)
		}
	}
	// Neither normal notes nor suppression rules may enter a global/raw scope without a bot.
	unowned := core.NewEnvelope(core.Message{Channel: "group-chat", UserID: "bob", Text: "你再提科目二 我就删掉你的记忆"})
	unowned.AddAction(core.Action{Type: core.ActionNote, Payload: "unowned-note 不应进入任何记忆空间"})
	if _, err := writer.Process(ctx, unowned); err != nil {
		t.Fatal(err)
	}
	entries, _ = repo.Retrieve(ctx, Query{Limit: 100})
	if len(entries) != 5 {
		t.Fatalf("unowned note or suppression rule was written: %+v", entries)
	}
	// A suppression instruction is shared only within the issuing bot as well.
	owned := core.NewEnvelope(core.Message{BotID: "owner", Channel: "private-chat", UserID: "alice", Text: "你再提科目二 我就删掉你的记忆"})
	if _, err := writer.Process(ctx, owned); err != nil {
		t.Fatal(err)
	}
	entries, _ = repo.Recent(ctx, ChannelScope("private-chat"), 10)
	if len(entries) != 2 {
		t.Fatalf("expected note and bot-wide suppression instruction, got %+v", entries)
	}
}

func TestMemoryStage_OnlyReadsRequestedBotScope(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	for _, e := range []Entry{
		{Scope: BotScope("owner"), Content: "shared-note 当前机器人记下的事实"},
		{Scope: BotScope("other"), Content: "foreign-note 另一个机器人记下的事实"},
		{Scope: ChannelScope("group-chat"), Content: "legacy-note 旧会话未确认归属的事实"},
		{Scope: UserScope("bob"), Content: "legacy-note 旧用户未确认归属的事实"},
	} {
		if err := repo.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	reader := NewMemoryStage("memory", repo, MemoryStageConfig{}, noop.NewTracerProvider(), zap.NewNop().Sugar())
	for _, tc := range []struct {
		bot, want, excluded string
	}{
		{"owner", "shared-note", "foreign-note"},
		{"other", "foreign-note", "shared-note"},
		{"", "", "shared-note"},
	} {
		t.Run("read-"+tc.bot, func(t *testing.T) {
			incoming := core.NewEnvelope(core.Message{BotID: tc.bot, Channel: "group-chat", UserID: "bob"})
			if _, err := reader.Process(ctx, incoming); err != nil {
				t.Fatal(err)
			}
			value, _ := incoming.Get("memory.context")
			text, _ := value.(string)
			if tc.want != "" && !strings.Contains(text, tc.want) {
				t.Fatalf("expected %q in %q", tc.want, text)
			}
			if strings.Contains(text, tc.excluded) || strings.Contains(text, "legacy-note") || (tc.bot == "" && text != "") {
				t.Fatalf("memory outside bot ownership was injected: %q", text)
			}
		})
	}
}

func TestContextManager_ExplicitEmptyScopesDoNotSearchAll(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	if err := repo.Append(ctx, Entry{Scope: BotScope("other"), Content: "sensitive matching memory"}); err != nil {
		t.Fatal(err)
	}
	mgr := NewContextManager(repo, NewContextBuilder(ContextBuilderConfig{}), nil, nil, ContextManagerConfig{})
	result, err := mgr.AssembleContextScopes(ctx, nil, "matching")
	if err != nil || result.ContextText != "" || result.EntriesUsed != 0 {
		t.Fatalf("empty authorized scopes must not broaden to the entire repository: %+v, %v", result, err)
	}
}
