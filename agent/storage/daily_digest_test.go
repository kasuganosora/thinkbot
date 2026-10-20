package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

type digestProvider struct {
	calls   int
	prompts []string
}

func (p *digestProvider) Name() string { return "mock" }
func (p *digestProvider) DoGenerate(_ context.Context, gp llm.GenerateParams) (*llm.GenerateResult, error) {
	p.calls++
	text := extractPromptText(gp.Messages[0])
	p.prompts = append(p.prompts, text)
	ids := []string{}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "[") {
			if i := strings.Index(line, "]"); i > 0 {
				ids = append(ids, line[1:i])
			}
		}
	}
	q := func(xs []string) string { return `"` + strings.Join(xs, `","`) + `"` }
	return &llm.GenerateResult{FinishReason: llm.FinishReasonStop,
		Text: fmt.Sprintf(`{"events":[{"text":"事件一","source_ids":[%s]},{"text":"事件二","source_ids":[]}]}`, q(ids))}, nil
}
func (p *digestProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

func TestDailyDigest(t *testing.T) {
	core.SetListenChannelsOverride([]string{"test:timeline"})
	defer core.SetListenChannelsOverride(nil)
	db := testDB(t)
	ctx := context.Background()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	scope := memory.ChannelScope("test:timeline")
	repo := NewSQLiteRepository(db)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, loc)
	add := func(id string, at time.Time, bot string) {
		meta := fmt.Sprintf(`{"bot_id":"%s"}`, bot)
		if err := db.Create(&dao.EntryModel{ID: id, ScopeKind: "channel", ScopeID: "test:timeline", Source: "note",
			Content: "帖子 " + id, MetadataJSON: meta, CreatedAt: at}).Error; err != nil {
			t.Fatal(err)
		}
	}
	add("n1", time.Date(2026, 10, 18, 10, 0, 0, 0, loc), "b1")
	add("n2", time.Date(2026, 10, 18, 23, 59, 0, 0, loc), "b1")
	add("x1", time.Date(2026, 10, 18, 12, 0, 0, 0, loc), "b2") // other bot
	add("t1", time.Date(2026, 10, 20, 8, 0, 0, 0, loc), "b1")  // today: not yet
	// 10-19: no notes → no call

	prov := &digestProvider{}
	d := NewDailyDigester(repo, DailyDigestConfig{Provider: prov, Model: &llm.Model{ID: "mock"}, BotID: "b1", Location: loc, BacklogDays: 3}, zap.NewNop().Sugar())
	d.now = func() time.Time { return now }
	if err := d.DigestScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Fatalf("one call for the one day with notes, got %d", prov.calls)
	}
	var dg dao.EntryModel
	if err := db.Where("source = ?", DailyDigestSource).First(&dg).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dg.Content, "n1") || !strings.Contains(dg.Content, "事件一") {
		t.Fatalf("injected text must hold events, not ids: %q", dg.Content)
	}
	if !strings.Contains(dg.MetadataJSON, `"events":[{"index":0,"source_ids":["n1","n2"]}`) {
		t.Fatalf("per-event ids in metadata: %s", dg.MetadataJSON)
	}
	if dg.DedupCheckedAt == nil {
		t.Fatal("digest must not be a dedup seed")
	}
	active, _ := repo.GetAllActive(ctx, scope)
	ids := map[string]bool{}
	for _, e := range active {
		ids[e.ID] = true
	}
	if ids["n1"] || ids["n2"] || !ids["x1"] || !ids["t1"] {
		t.Fatalf("archive exactly the digested notes: %v", ids)
	}
	// idempotent
	if err := d.DigestScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Model(&dao.EntryModel{}).Where("source = ?", DailyDigestSource).Count(&n)
	if prov.calls != 1 || n != 1 {
		t.Fatalf("re-run must not duplicate: calls=%d digests=%d", prov.calls, n)
	}
	// listen scopes are excluded from dedup
	if !isListenScope(scope) {
		t.Fatal("listen scope expected")
	}
}

func TestDailyDigestChunks(t *testing.T) {
	var notes []memory.Entry
	for i := 0; i < 10; i++ {
		notes = append(notes, memory.Entry{ID: fmt.Sprintf("n%d", i), Content: strings.Repeat("字", 100)})
	}
	if c := chunkNotes(notes, 350); len(c) != 4 {
		t.Fatalf("chunks=%d", len(c))
	}
	prov := &digestProvider{}
	d := NewDailyDigester(nil, DailyDigestConfig{Provider: prov, Model: &llm.Model{ID: "m"}, MaxInputChars: 350}, nil)
	if _, err := d.summarize(context.Background(), memory.ChannelScope("x"), "2026-10-19", notes); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 5 {
		t.Fatalf("4 chunks + 1 combine, got %d", prov.calls)
	}
}

func TestBuildDigestBounded(t *testing.T) {
	evs := []digestEvent{{Text: strings.Repeat("a", 50)}, {Text: strings.Repeat("b", 50)}, {Text: strings.Repeat("c", 50)}}
	content, meta := buildDigest("2026-10-19", evs, nil, 130)
	if len([]rune(content)) > 130 || len(meta["events"].([]map[string]any)) != 2 {
		t.Fatalf("bounded: %d %v", len(content), meta["events"])
	}
}
