package prompt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// helper: 创建临时 SOUL.md 文件
func writeSoul(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "SOUL.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSoulLoader_AutoCreateWithSubDir(t *testing.T) {
	dir := t.TempDir()
	// Path with a subdirectory that doesn't exist yet
	path := filepath.Join(dir, "bot-001", "SOUL.md")

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{Path: path}, reg)

	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// File and parent dir should be created
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file auto-created with parent dir: %v", err)
	}
	if !loader.Loaded() {
		t.Error("expected Loaded()=true")
	}
}

func TestSoulLoader_LoadBasic(t *testing.T) {
	dir := t.TempDir()
	path := writeSoul(t, dir, "You are ThinkBot, a helpful assistant.")

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{Path: path}, reg)

	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !loader.Loaded() {
		t.Fatal("expected Loaded()=true after successful Load")
	}

	// Registry should now have an "identity" section
	sec, ok := reg.Get("identity")
	if !ok {
		t.Fatal("expected identity section to be registered")
	}
	if sec.Order != 0 {
		t.Errorf("expected Order=0, got %d", sec.Order)
	}
	if !sec.Enabled {
		t.Error("expected Enabled=true")
	}
	if sec.Content != "You are ThinkBot, a helpful assistant." {
		t.Errorf("unexpected content: %q", sec.Content)
	}
}

func TestSoulLoader_FileNotFound(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "nonexistent.md")

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{
		Path:          missingPath,
		SectionName:   "identity",
	}, reg)

	// Should not return error — should auto-create default file
	if err := loader.Load(); err != nil {
		t.Fatalf("expected nil error for missing file (auto-create), got: %v", err)
	}
	if !loader.Loaded() {
		t.Error("expected Loaded()=true after auto-create")
	}
	// File should now exist on disk
	if _, err := os.Stat(missingPath); err != nil {
		t.Errorf("expected file to be auto-created: %v", err)
	}
	// Registry should have identity section
	sec, ok := reg.Get("identity")
	if !ok {
		t.Fatal("expected identity section after auto-create")
	}
	if sec.Content == "" {
		t.Error("expected non-empty content from default template")
	}
}

func TestSoulLoader_FrontMatter(t *testing.T) {
	dir := t.TempDir()
	content := `---
enabled: false
---
You should be disabled.`
	path := writeSoul(t, dir, content)

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{Path: path}, reg)

	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	sec, ok := reg.Get("identity")
	if !ok {
		t.Fatal("expected identity section")
	}
	if sec.Enabled {
		t.Error("expected Enabled=false from front matter")
	}
	if sec.Content != "You should be disabled." {
		t.Errorf("unexpected content: %q", sec.Content)
	}
}

func TestSoulLoader_Variables(t *testing.T) {
	dir := t.TempDir()
	content := `You are {{.BotName}}, operating in {{.Channel}}.
Today is {{.Date}}.`
	path := writeSoul(t, dir, content)

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{Path: path}, reg)

	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	vars := loader.Variables()
	if len(vars) != 3 {
		t.Fatalf("expected 3 variables, got %d", len(vars))
	}

	// Verify variables are registered in the section
	sec, _ := reg.Get("identity")
	if len(sec.Variables) != 3 {
		t.Errorf("expected 3 section variables, got %d", len(sec.Variables))
	}
}

func TestSoulLoader_Assemble(t *testing.T) {
	dir := t.TempDir()
	content := "You are ThinkBot. Your bot ID is {{.BotID}}."
	path := writeSoul(t, dir, content)

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{Path: path}, reg)
	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	assembler := NewAssembler(reg, DefaultAssemblerConfig())
	ctx := &AssemblyContext{
		Values: map[string]any{
			"BotID": "bot-123",
		},
		BotID: "bot-123",
	}

	result, err := assembler.Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}

	expected := "You are ThinkBot. Your bot ID is bot-123."
	if result.Prompt != expected {
		t.Errorf("expected %q, got %q", expected, result.Prompt)
	}
	if len(result.SectionsUsed) != 1 || result.SectionsUsed[0] != "identity" {
		t.Errorf("unexpected sections used: %v", result.SectionsUsed)
	}
}

func TestSoulLoader_HotReload(t *testing.T) {
	dir := t.TempDir()
	path := writeSoul(t, dir, "v1")

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{
		Path:           path,
		ReloadInterval: 50 * time.Millisecond,
	}, reg)

	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Start watcher
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loader.StartWatcher(ctx)
	defer loader.Stop()

	// Initial content check
	if c := loader.Content(); c != "v1" {
		t.Fatalf("expected 'v1', got %q", c)
	}

	// Modify file with a newer mtime
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("v2"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for reload
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if loader.Content() == "v2" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if c := loader.Content(); c != "v2" {
		t.Errorf("expected 'v2' after reload, got %q", c)
	}
}

func TestSoulLoader_FileRemoved(t *testing.T) {
	dir := t.TempDir()
	path := writeSoul(t, dir, "hello")

	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{
		Path:           path,
		ReloadInterval: 50 * time.Millisecond,
	}, reg)

	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if _, ok := reg.Get("identity"); !ok {
		t.Fatal("expected identity section before removal")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loader.StartWatcher(ctx)
	defer loader.Stop()

	// Remove the file — should be recreated with default content
	os.Remove(path)

	// Wait for recreate + reload
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := loader.Content()
		if c != "" && c != "hello" {
			// Content changed back to default
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// File should exist again
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file to be recreated after removal: %v", err)
	}
	// Loader should still be loaded
	if !loader.Loaded() {
		t.Error("expected Loaded()=true after recreate")
	}
	// Identity section should still exist
	if _, ok := reg.Get("identity"); !ok {
		t.Error("expected identity section to still exist after recreate")
	}
	// Content should be the default template, not "hello"
	c := loader.Content()
	if c == "hello" {
		t.Error("expected content to be recreated with default, not original")
	}
}

func TestSoulLoader_DoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	path := writeSoul(t, dir, "from soul.md")

	reg := NewRegistry()

	// Pre-register identity with higher order via FileLoader pattern
	reg.Register(Section{
		Name:    "identity",
		Order:   1,
		Content: "pre-existing identity",
		Enabled: true,
	})

	loader := NewSoulLoader(SoulLoaderConfig{Path: path}, reg)
	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// SoulLoader should have overridden the identity section
	sec, _ := reg.Get("identity")
	if sec.Content != "from soul.md" {
		t.Errorf("expected soul.md content to override, got %q", sec.Content)
	}
}

// testLogger is a simple SoulLogger for testing
type testLogger struct {
	infoCount  atomic.Int64
	warnCount  atomic.Int64
	errorCount atomic.Int64
}

func (l *testLogger) Infof(format string, args ...any)  { l.infoCount.Add(1) }
func (l *testLogger) Warnf(format string, args ...any)  { l.warnCount.Add(1) }
func (l *testLogger) Errorf(format string, args ...any) { l.errorCount.Add(1) }

func TestSoulLoader_WithLogger(t *testing.T) {
	dir := t.TempDir()
	path := writeSoul(t, dir, "content")

	reg := NewRegistry()
	logger := &testLogger{}
	loader := NewSoulLoader(SoulLoaderConfig{Path: path}, reg).WithLogger(logger)

	if err := loader.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if logger.infoCount.Load() == 0 {
		t.Error("expected at least one info log on successful load")
	}
}

func TestSoulLoader_DefaultConfig(t *testing.T) {
	cfg := DefaultSoulLoaderConfig()
	// Path is now empty by default — resolved at NewSoulLoader time
	if cfg.Path != "" {
		t.Errorf("expected default Path='', got %q", cfg.Path)
	}
	if cfg.SectionName != "identity" {
		t.Errorf("expected default SectionName='identity', got %q", cfg.SectionName)
	}
	if cfg.Order != 0 {
		t.Errorf("expected default Order=0, got %d", cfg.Order)
	}
}

func TestSoulLoader_AutoResolvePath(t *testing.T) {
	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{BotID: "bot-001"}, reg)

	// Path should be auto-resolved and contain botID
	p := loader.Path()
	if p == "" {
		t.Error("expected auto-resolved path to be non-empty")
	}
	if !strings.Contains(p, "bot-001") {
		t.Errorf("expected path to contain botID 'bot-001', got %q", p)
	}
}

func TestSoulLoader_AutoResolvePathNoBotID(t *testing.T) {
	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{}, reg)

	// Path should still resolve even without botID (single-bot compat)
	p := loader.Path()
	if p == "" {
		t.Error("expected auto-resolved path to be non-empty")
	}
	// Should end with SOUL.md (no botID dir)
	if !strings.HasSuffix(filepath.ToSlash(p), "SOUL.md") {
		t.Errorf("expected path to end with SOUL.md, got %q", p)
	}
}

func TestSoulLoader_ExplicitPath(t *testing.T) {
	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{
		Path: "/custom/path/SOUL.md",
	}, reg)

	if loader.Path() != "/custom/path/SOUL.md" {
		t.Errorf("expected explicit path, got %q", loader.Path())
	}
}

func TestDefaultSoulPath(t *testing.T) {
	// With botID
	p := DefaultSoulPath("bot-001")
	if p == "" {
		t.Error("expected non-empty default soul path")
	}
	if !strings.Contains(filepath.ToSlash(p), "bot-001") {
		t.Errorf("expected path to contain botID, got %q", p)
	}

	// Without botID (single-bot compat)
	p2 := DefaultSoulPath("")
	if p2 == "" {
		t.Error("expected non-empty default soul path for empty botID")
	}
}

func TestSoulLoader_Stop(t *testing.T) {
	reg := NewRegistry()
	loader := NewSoulLoader(SoulLoaderConfig{
		Path:           "SOUL.md",
		ReloadInterval: 100 * time.Millisecond,
	}, reg)

	ctx := context.Background()
	loader.StartWatcher(ctx)
	loader.Stop()

	// Calling Stop again should not panic
	loader.Stop()
}
