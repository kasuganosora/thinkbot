package bot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/prompt"
)

func writeSkill(t *testing.T, dir, name, desc string) {
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

func TestSetupSkills_BundledThenManagedOverride(t *testing.T) {
	root := t.TempDir()
	bundled := filepath.Join(root, "bundled")
	managed := filepath.Join(root, "managed")
	writeSkill(t, bundled, "pdf", "bundled pdf skill")
	writeSkill(t, bundled, "xlsx", "spreadsheet skill")
	writeSkill(t, managed, "pdf", "managed pdf override")

	reg := prompt.NewRegistry()
	mgr, err := SetupSkills(SkillWireConfig{
		BundledDir: bundled,
		ManagedDir: managed,
		Prompt:     reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(list))
	}
	pdf, ok := mgr.Get("pdf")
	if !ok {
		t.Fatal("pdf missing")
	}
	if pdf.Source != "managed" {
		t.Errorf("pdf source = %q, want managed", pdf.Source)
	}
	if pdf.Description != "managed pdf override" {
		t.Errorf("pdf desc = %q", pdf.Description)
	}
	xlsx, ok := mgr.Get("xlsx")
	if !ok || xlsx.Source != "bundled" {
		t.Fatalf("xlsx source = %v %q", ok, xlsx.Source)
	}
	if _, ok := reg.Get("skill_trigger"); !ok {
		t.Fatal("skill_trigger section missing")
	}
}
