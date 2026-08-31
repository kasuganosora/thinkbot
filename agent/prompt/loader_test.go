package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileLoader_LoadAll(t *testing.T) {
	dir := t.TempDir()

	// 000_identity.md
	_ = os.WriteFile(filepath.Join(dir, "000_identity.md"), []byte("You are ThinkBot, a helpful AI assistant."), 0644)

	// 100_rules.md
	_ = os.WriteFile(filepath.Join(dir, "100_rules.md"), []byte("---\nenabled: true\n---\nAlways be polite and helpful."), 0644)

	// 200_context.md with variable
	_ = os.WriteFile(filepath.Join(dir, "200_context.md"), []byte("Here is what you remember:\n{{.memory_context}}"), 0644)

	// 999_disabled.md
	_ = os.WriteFile(filepath.Join(dir, "999_disabled.md"), []byte("---\nenabled: false\n---\nThis should be disabled."), 0644)

	registry := NewRegistry()
	loader := NewFileLoader(dir, registry)

	count, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if count != 4 {
		t.Errorf("loaded %d files, want 4", count)
	}
	if registry.Len() != 4 {
		t.Errorf("registry has %d sections, want 4", registry.Len())
	}

	// 验证排序
	sections := registry.List()
	if sections[0].Name != "identity" || sections[0].Order != 0 {
		t.Errorf("first section: name=%q order=%d, want identity/0", sections[0].Name, sections[0].Order)
	}
	if sections[1].Name != "rules" || sections[1].Order != 100 {
		t.Errorf("second section: name=%q order=%d, want rules/100", sections[1].Name, sections[1].Order)
	}

	// 验证禁用
	disabled, _ := registry.Get("disabled")
	if disabled.Enabled {
		t.Error("disabled section should have Enabled=false")
	}

	// 验证 Assemble 跳过 disabled
	ctx := &AssemblyContext{
		Values: map[string]any{
			"memory_context": "User likes cats",
		},
	}
	assembler := NewAssembler(registry, DefaultAssemblerConfig())
	result, err := assembler.Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	for _, name := range result.SectionsUsed {
		if name == "disabled" {
			t.Error("disabled section should not be in SectionsUsed")
		}
	}

	if !strings.Contains(result.Prompt, "User likes cats") {
		t.Error("expected variable to be rendered in prompt")
	}
}

func TestFileLoader_NonExistentDir(t *testing.T) {
	registry := NewRegistry()
	loader := NewFileLoader("/non/existent/path", registry)

	count, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll on non-existent dir should not error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestFileLoader_VariableMeta(t *testing.T) {
	dir := t.TempDir()

	_ = os.WriteFile(filepath.Join(dir, "050_greeting.md"), []byte(`---
var_bot_name_source: static
var_bot_name_value: ThinkBot
var_user_name_key: user.display_name
var_user_name_default: Friend
---
Hello {{.user_name}}! I am {{.bot_name}}.`), 0644)

	registry := NewRegistry()
	loader := NewFileLoader(dir, registry)
	_, _ = loader.LoadAll()

	sec, ok := registry.Get("greeting")
	if !ok {
		t.Fatal("expected greeting section")
	}
	if len(sec.Variables) != 2 {
		t.Fatalf("expected 2 variables, got %d", len(sec.Variables))
	}

	// Assemble with partial context (no user.display_name)
	ctx := &AssemblyContext{
		Values: map[string]any{},
	}
	assembler := NewAssembler(registry, DefaultAssemblerConfig())
	result, err := assembler.Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	if !strings.Contains(result.Prompt, "ThinkBot") {
		t.Error("expected static variable 'ThinkBot' in prompt")
	}
	if !strings.Contains(result.Prompt, "Friend") {
		t.Error("expected default 'Friend' in prompt")
	}
}

func TestFileLoader_NoOrderPrefix(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "custom.md"), []byte("No order prefix content"), 0644)

	registry := NewRegistry()
	loader := NewFileLoader(dir, registry)
	_, _ = loader.LoadAll()

	sec, ok := registry.Get("custom")
	if !ok {
		t.Fatal("expected custom section")
	}
	if sec.Order != 500 {
		t.Errorf("expected default order 500, got %d", sec.Order)
	}
}

func TestParseFileName(t *testing.T) {
	tests := []struct {
		filename string
		order    int
		name     string
	}{
		{"000_identity.md", 0, "identity"},
		{"100_rules.md", 100, "rules"},
		{"200_memory_context.md", 200, "memory_context"},
		{"custom.md", 500, "custom"},
		{"abc_test.md", 500, "abc_test"}, // "abc" is not a number
	}

	for _, tt := range tests {
		order, name, err := parseFileName(tt.filename)
		if err != nil {
			t.Errorf("parseFileName(%q): %v", tt.filename, err)
			continue
		}
		if order != tt.order || name != tt.name {
			t.Errorf("parseFileName(%q) = (%d, %q), want (%d, %q)",
				tt.filename, order, name, tt.order, tt.name)
		}
	}
}
