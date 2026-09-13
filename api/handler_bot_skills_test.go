package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
)

func newSkillAPITest(t *testing.T) (*Server, string) {
	t.Helper()
	tmp := t.TempDir()
	store := config.NewStore(nil)
	ctx := context.Background()
	if err := store.Set(ctx, config.KeyWorkspaceDir, filepath.Join(tmp, "workspaces")); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, config.KeySandboxBackend, "local"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, config.KeySandboxRequireDocker, "false"); err != nil {
		t.Fatal(err)
	}
	logger := zap.NewNop().Sugar()
	svc := &BotService{store: store, logger: logger}
	s := &Server{
		store:                    store,
		logger:                   logger,
		botSvc:                   svc,
		bundledSkillsDirOverride: filepath.Join(tmp, "bundled"),
	}
	return s, tmp
}

func writeTestSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectBotSkills_BundledAndManaged(t *testing.T) {
	s, tmp := newSkillAPITest(t)
	writeTestSkill(t, s.bundledSkillsDir(), "pdf", "bundled pdf")
	writeTestSkill(t, s.bundledSkillsDir(), "xlsx", "sheets")
	writeTestSkill(t, filepath.Join(tmp, "skills", "bot-a"), "pdf", "managed pdf")

	list := s.collectBotSkills("bot-a")
	byName := map[string]botSkillEntry{}
	for _, sk := range list {
		byName[sk.Name] = sk
	}
	if byName["pdf"].Source != "managed" {
		t.Fatalf("pdf source = %q, want managed; list=%v", byName["pdf"].Source, list)
	}
	if byName["xlsx"].Source != "bundled" {
		t.Fatalf("xlsx source = %q, want bundled", byName["xlsx"].Source)
	}
	if byName["pdf"].Editable != true || byName["xlsx"].Editable != false {
		t.Fatalf("editable flags pdf=%v xlsx=%v", byName["pdf"].Editable, byName["xlsx"].Editable)
	}
}

func TestCreateBotSkill_WritesManagedDir(t *testing.T) {
	s, tmp := newSkillAPITest(t)
	const botID = "bot-skill-create"
	content := "---\nname: demo\ndescription: a demo skill\n---\n\n# Demo\n"
	dir := s.botSkillsDir(botID)
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "demo", "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	list := s.collectBotSkills(botID)
	if len(list) != 1 || list[0].Name != "demo" || list[0].Source != "managed" {
		t.Fatalf("list = %+v", list)
	}
	if _, err := os.Stat(filepath.Join(tmp, "skills", botID, "demo", "SKILL.md")); err != nil {
		t.Fatalf("expected file on disk: %v", err)
	}
}

func TestSetBotSkillEnabled_PersistsWhenNotRunning(t *testing.T) {
	s, _ := newSkillAPITest(t)
	writeTestSkill(t, s.bundledSkillsDir(), "pdf", "bundled pdf")
	if err := s.store.Set(context.Background(), config.BotSkillEnabledKey("bot-z", "pdf"), "false"); err != nil {
		t.Fatal(err)
	}
	list := s.collectBotSkills("bot-z")
	if len(list) != 1 || list[0].Enabled {
		t.Fatalf("expected disabled pdf, got %+v", list)
	}
}
