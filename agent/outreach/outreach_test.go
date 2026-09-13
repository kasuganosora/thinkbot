package outreach

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&dao.OutreachCommitment{}, &dao.OutreachRecord{}, &dao.OutreachLastSeen{}, &dao.IdentityMapping{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func testHarness(t *testing.T, now time.Time) (*Executor, *Repo, *fakeRunner) {
	t.Helper()
	db := testDB(t)
	repo := NewRepo(db)
	cfgStore := NewConfigStore(t.TempDir())
	cfg := DefaultConfig()
	cfg.Enabled = true
	web := cfg.Platform("web")
	web.Enabled = true
	cfg.Platforms["web"] = web
	if err := cfgStore.Save("bot-1", cfg); err != nil {
		t.Fatalf("save cfg: %v", err)
	}
	runner := &fakeRunner{}
	exec := NewExecutor(ExecutorConfig{
		BotID:     "bot-1",
		CfgStore:  cfgStore,
		Repo:      repo,
		Logger:    zap.NewNop().Sugar(),
		Runner:    runner,
		Now:       func() time.Time { return now },
		AllowPost: func(string) bool { return true },
	})
	return exec, repo, runner
}

type fakeRunner struct {
	calls   int
	reply   string
	err     error
	lastEnv *core.Envelope
}

func (f *fakeRunner) ProcessSync(_ context.Context, env *core.Envelope) (*core.Envelope, []core.Action, error) {
	f.calls++
	f.lastEnv = env
	if f.err != nil {
		return env, nil, f.err
	}
	text := f.reply
	if text == "" {
		text = "该练灯光题了"
	}
	env.AddAction(core.Action{Type: core.ActionReply, Payload: text, UserID: env.Message.UserID})
	return env, env.Actions(), nil
}

func seedDue(t *testing.T, repo *Repo, kind string, due time.Time) *dao.OutreachCommitment {
	t.Helper()
	c := &dao.OutreachCommitment{
		BotID:       "bot-1",
		Kind:        kind,
		IdentityKey: "user:1",
		UserID:      "1",
		Channel:     "web-bot-1",
		ChannelType: "web",
		SessionID:   "sess-1",
		DueAt:       due,
		Topic:       "练灯光题",
		Context:     "明天提醒我练灯光题",
		Status:      dao.OutreachPending,
		CreatedAt:   due.Add(-time.Hour),
	}
	if err := repo.CreateCommitment(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return c
}

func lastRecord(t *testing.T, repo *Repo) dao.OutreachRecord {
	t.Helper()
	rows, err := repo.ListRecords(context.Background(), "bot-1", "all", "", 10)
	if err != nil || len(rows) == 0 {
		t.Fatalf("records: %v len=%d", err, len(rows))
	}
	return rows[0]
}

func TestExecute_NothingDue_SilentNoRunner(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, _, runner := testHarness(t, now)
	res, err := exec.Execute(context.Background(), &cron.Job{})
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatalf("expected 0 ProcessSync, got %d", runner.calls)
	}
	if !strings.Contains(res.Output, "silent") {
		t.Fatalf("output = %q", res.Output)
	}
	rec := lastRecord(t, exec.repo)
	if rec.Status != StatusSilent {
		t.Fatalf("status = %s", rec.Status)
	}
}

func TestExecute_HardBypassesQuiet(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	seedDue(t, repo, dao.OutreachKindReminder, now.Add(-time.Minute))
	if err := repo.TouchInbound(context.Background(), "bot-1", "user:1", "web", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("expected ProcessSync once, got %d", runner.calls)
	}
	if lastRecord(t, repo).Status != StatusSent {
		t.Fatalf("status = %s", lastRecord(t, repo).Status)
	}
}

func TestExecute_SoftQuietWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	seedDue(t, repo, dao.OutreachKindWatch, now.Add(-time.Minute))
	if err := repo.TouchInbound(context.Background(), "bot-1", "user:1", "web", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatalf("soft should not call LLM, got %d", runner.calls)
	}
	if lastRecord(t, repo).Status != StatusSkippedQuiet {
		t.Fatalf("status = %s", lastRecord(t, repo).Status)
	}
}

func TestExecute_SoftDailyCap(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	seedDue(t, repo, dao.OutreachKindWatch, now.Add(-time.Minute))
	_ = repo.InsertRecord(context.Background(), &dao.OutreachRecord{
		BotID: "bot-1", IdentityKey: "user:1", ChannelType: "web",
		Status: StatusSent, CreatedAt: now.Add(-time.Hour), Trigger: "watch",
	})
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatalf("capped soft should not call LLM, got %d", runner.calls)
	}
	if lastRecord(t, repo).Status != StatusSkippedQuota {
		t.Fatalf("status = %s", lastRecord(t, repo).Status)
	}
}

func TestExecute_GateOpen_ProcessSync(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	c := seedDue(t, repo, dao.OutreachKindReminder, now.Add(-time.Minute))
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("calls = %d", runner.calls)
	}
	if runner.lastEnv == nil || !runner.lastEnv.IsOutreach() {
		t.Fatal("envelope must be outreach force-send")
	}
	if runner.lastEnv.Message.UserID != "1" {
		t.Fatalf("user = %q", runner.lastEnv.Message.UserID)
	}
	if runner.lastEnv.Message.Source != "web-bot-1" {
		t.Fatalf("source = %q want real channel", runner.lastEnv.Message.Source)
	}
	if runner.lastEnv.Message.Text != "" {
		t.Fatal("Text must be empty")
	}
	if runner.lastEnv.Message.InjectContext == "" {
		t.Fatal("InjectContext required")
	}
	rec := lastRecord(t, repo)
	if rec.Status != StatusSent || rec.Content == "" {
		t.Fatalf("record %+v", rec)
	}
	got, _ := repo.GetCommitment(context.Background(), "bot-1", c.ID)
	if got.Status != dao.OutreachDelivered {
		t.Fatalf("commitment status = %s", got.Status)
	}
}

func TestExecute_EmptySoft_NoSend(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	runner.reply = "   "
	seedDue(t, repo, dao.OutreachKindWatch, now.Add(-time.Minute))
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if lastRecord(t, repo).Status != StatusError {
		t.Fatalf("status = %s", lastRecord(t, repo).Status)
	}
}

func TestExecute_EmptyHard_TemplateFallback(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	runner.reply = "  "
	var sent string
	exec.fallback = func(_ context.Context, c dao.OutreachCommitment, content string) error {
		sent = content
		return nil
	}
	seedDue(t, repo, dao.OutreachKindReminder, now.Add(-time.Minute))
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "练灯光题") {
		t.Fatalf("fallback = %q", sent)
	}
	if lastRecord(t, repo).Status != StatusSent {
		t.Fatalf("status = %s", lastRecord(t, repo).Status)
	}
}

func TestExecute_SpeakModeBlocks(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	exec.allowPost = func(string) bool { return false }
	seedDue(t, repo, dao.OutreachKindReminder, now.Add(-time.Minute))
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatal("speak mode must block LLM")
	}
	if lastRecord(t, repo).Status != StatusSkippedSpeakMode {
		t.Fatalf("status = %s", lastRecord(t, repo).Status)
	}
}

func TestExecute_PlatformDisabled(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	cfg, _ := exec.cfgStore.Load("bot-1")
	p := cfg.Platform("web")
	p.Enabled = false
	cfg.Platforms["web"] = p
	_ = exec.cfgStore.Save("bot-1", cfg)
	seedDue(t, repo, dao.OutreachKindReminder, now.Add(-time.Minute))
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatalf("disabled platform must not call LLM")
	}
	if lastRecord(t, repo).Status != StatusSkippedPlatformDisabled {
		t.Fatalf("status = %s", lastRecord(t, repo).Status)
	}
}

func TestExecute_WebCapDoesNotAffectTelegram(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	cfg, _ := exec.cfgStore.Load("bot-1")
	tg := cfg.Platform("telegram")
	tg.Enabled = true
	cfg.Platforms["telegram"] = tg
	_ = exec.cfgStore.Save("bot-1", cfg)

	_ = repo.InsertRecord(context.Background(), &dao.OutreachRecord{
		BotID: "bot-1", IdentityKey: "user:1", ChannelType: "web",
		Status: StatusSent, CreatedAt: now.Add(-time.Hour), Trigger: "watch",
	})
	c := &dao.OutreachCommitment{
		BotID: "bot-1", Kind: dao.OutreachKindWatch, IdentityKey: "user:1",
		UserID: "1", Channel: "tg-1", ChannelType: "telegram",
		DueAt: now.Add(-time.Minute), Topic: "天气", Status: dao.OutreachPending,
		CreatedAt: now.Add(-time.Hour),
	}
	_ = repo.CreateCommitment(context.Background(), c)
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("telegram should still send, calls=%d", runner.calls)
	}
}

func TestExecute_ExpireStale(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	old := &dao.OutreachCommitment{
		BotID: "bot-1", Kind: dao.OutreachKindReminder, IdentityKey: "user:1",
		UserID: "1", Channel: "web-bot-1", ChannelType: "web",
		DueAt: now.Add(-8 * 24 * time.Hour), Topic: "过期", Status: dao.OutreachPending,
		CreatedAt: now.Add(-9 * 24 * time.Hour),
	}
	_ = repo.CreateCommitment(context.Background(), old)
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatalf("expired must not send")
	}
	got, _ := repo.GetCommitment(context.Background(), "bot-1", old.ID)
	if got.Status != dao.OutreachExpired {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestExecute_NotifyInboundThenSoftAllowed(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	exec, repo, runner := testHarness(t, now)
	seedDue(t, repo, dao.OutreachKindWatch, now.Add(-time.Minute))
	exec.NotifyInbound(context.Background(), "1", "web")
	// last inbound is `now` → still in 6h window
	if _, err := exec.Execute(context.Background(), &cron.Job{}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatal("just inbound, soft must skip")
	}
}

func TestNormalizeChannelType(t *testing.T) {
	if got := NormalizeChannelType("web", "web-bot-1"); got != "web" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeChannelType("", "web-abc"); got != "web" {
		t.Fatalf("from source got %q", got)
	}
	if got := NormalizeChannelType("", "telegram-cs"); got != "telegram" {
		t.Fatalf("telegram instance got %q", got)
	}
}

func TestApplyPlatformPatch_PreservesOtherPlatforms(t *testing.T) {
	cfg := DefaultConfig()
	web := cfg.Platform("web")
	web.QuietHours = 3
	cfg.Platforms["web"] = web
	ApplyPlatformPatch(&cfg, map[string]PlatformConfig{
		"telegram": {Enabled: true, MaxPerUserPerDay: 2, QuietHours: 8, HardBypassQuiet: true, HardBypassDailyCap: true},
	})
	if cfg.Platform("web").QuietHours != 3 {
		t.Fatalf("web quiet overwritten: %v", cfg.Platform("web").QuietHours)
	}
	if !cfg.Platform("telegram").Enabled || cfg.Platform("telegram").MaxPerUserPerDay != 2 {
		t.Fatalf("telegram patch not applied: %+v", cfg.Platform("telegram"))
	}
}

func TestParseWhen(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, loc)
	t.Run("relative", func(t *testing.T) {
		got, err := ParseWhen("2h", loc, now)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(now.Add(2 * time.Hour).UTC()) {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("tomorrow", func(t *testing.T) {
		got, err := ParseWhen("明天 9:00", loc, now)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 9, 14, 9, 0, 0, 0, loc).UTC()
		if !got.Equal(want) {
			t.Fatalf("got %s want %s", got, want)
		}
	})
	t.Run("TOMORROW", func(t *testing.T) {
		got, err := ParseWhen("TOMORROW 09:00", loc, now)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 9, 14, 9, 0, 0, 0, loc).UTC()
		if !got.Equal(want) {
			t.Fatalf("got %s want %s", got, want)
		}
	})
	t.Run("iso", func(t *testing.T) {
		got, err := ParseWhen("2026-09-14T10:30", loc, now)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 9, 14, 10, 30, 0, 0, loc).UTC()
		if !got.Equal(want) {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("bad", func(t *testing.T) {
		if _, err := ParseWhen("sometime", loc, now); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRemindTool_CreateListCancel(t *testing.T) {
	db := testDB(t)
	repo := NewRepo(db)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	def := remindToolDef(ToolConfig{Repo: repo, BotID: "bot-1", Location: time.UTC, Now: func() time.Time { return now }})
	ctx := llmExecCtx(agenttoolsMeta("1", "web-bot-1", "web", "sess-1"))
	out, err := def.Execute(ctx, map[string]any{"action": "create", "when": "2h", "text": "练灯光题"})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["success"] != true {
		t.Fatalf("%v", m)
	}
	id := m["id"].(string)

	listed, err := def.Execute(ctx, map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if listed.(map[string]any)["total"].(int) != 1 {
		t.Fatalf("list = %v", listed)
	}

	if _, err := def.Execute(ctx, map[string]any{"action": "cancel", "id": id}); err != nil {
		t.Fatal(err)
	}
	listed, _ = def.Execute(ctx, map[string]any{"action": "list"})
	if listed.(map[string]any)["total"].(int) != 0 {
		t.Fatal("cancel failed")
	}
}

func TestRemindTool_InvalidWhen(t *testing.T) {
	db := testDB(t)
	def := remindToolDef(ToolConfig{Repo: NewRepo(db), BotID: "bot-1", Location: time.UTC})
	ctx := llmExecCtx(agenttoolsMeta("1", "web-bot-1", "web", "sess-1"))
	out, err := def.Execute(ctx, map[string]any{"action": "create", "when": "nope", "text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["success"] != false {
		t.Fatalf("%v", out)
	}
}

func TestRemindTool_AntiNest(t *testing.T) {
	db := testDB(t)
	def := remindToolDef(ToolConfig{Repo: NewRepo(db), BotID: "bot-1"})
	base := agenttoolsMeta("1", "web-bot-1", "web", "sess-1")
	ctx := &llm.ToolExecContext{Context: WithOutreachSession(base)}
	out, err := def.Execute(ctx, map[string]any{"action": "create", "when": "2h", "text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["success"] != false {
		t.Fatal("expected anti-nest")
	}
}

func agenttoolsMeta(userID, chatID, channelType, sessionID string) context.Context {
	ctx := context.Background()
	ctx = agenttools.ContextWithMessageMeta(ctx, agenttools.MessageMeta{
		BotID: "bot-1", UserID: userID, Source: chatID, ChatID: chatID, ChannelType: channelType,
	})
	ctx = agenttools.ContextWithCallOrigin(ctx, agenttools.CallOrigin{BotID: "bot-1", SessionID: sessionID})
	return ctx
}

func llmExecCtx(ctx context.Context) *llm.ToolExecContext {
	return &llm.ToolExecContext{Context: ctx}
}
