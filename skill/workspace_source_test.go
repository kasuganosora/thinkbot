package skill

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// localWorkspaceFS runs the real list script with sh in a temp dir, reporting
// a fake WorkDir (as the docker sandbox reports /data).
type localWorkspaceFS struct {
	root    string
	workDir string
	execs   int
}

func (f *localWorkspaceFS) WorkDir() string { return f.workDir }
func (f *localWorkspaceFS) Exec(ctx context.Context, command string) (string, int, error) {
	f.execs++
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = f.root
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), 0, err
}
func (f *localWorkspaceFS) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.root, p))
}

func writeSkill(t *testing.T, root, dir, name, desc string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(root, "skills", dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\nDo things."
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSource_DiscoverUpdateRemove(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	writeSkill(t, root, "bangumi", "bangumi", "Bangumi 番组计划 anime tracking", t0)
	writeSkill(t, root, "ctrip-wendao", "ctrip-wendao", "携程问道 travel search", t0)
	writeSkill(t, root, "pdf-mine", "pdf", "my own pdf", t0) // clashes with bundled
	writeSkill(t, root, "broken", "", "no name", t0)
	if err := os.MkdirAll(filepath.Join(root, "skills", "bangumi", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "bangumi", "scripts", "run.sh"), []byte("echo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "skills", "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{Name: "pdf", Description: "bundled pdf", Content: "x", Enabled: true, Source: "bundled", Dir: "skills/pdf"})
	fs := &localWorkspaceFS{root: root, workDir: "/data"}
	src := NewWorkspaceSource(fs, mgr, nil)
	src.Attach()
	src.Refresh(context.Background())

	bg, ok := mgr.Get("bangumi")
	if !ok || bg.Source != SourceWorkspace || bg.Dir != "/data/skills/bangumi" {
		t.Fatalf("bangumi not registered from workspace: %+v", bg)
	}
	if len(bg.Resources.Scripts) != 1 || bg.Resources.Scripts[0] != "/data/skills/bangumi/scripts/run.sh" {
		t.Errorf("scripts = %v", bg.Resources.Scripts)
	}
	if _, ok := mgr.Get("ctrip-wendao"); !ok {
		t.Error("ctrip-wendao missing")
	}
	if p, _ := mgr.Get("pdf"); p.Source != "bundled" {
		t.Error("bundled skill must win a name clash")
	}
	if hits := mgr.SearchSkills("携程", 5); len(hits) != 1 || hits[0].Name != "ctrip-wendao" {
		t.Errorf("search 携程 = %v", hits)
	}
	if !strings.Contains(mgr.BuildTriggerPrompt(), "/data/skills/<skill-name>/SKILL.md") {
		t.Error("trigger prompt should teach the install location")
	}

	// Throttled: MaybeRefresh within the interval does not exec again.
	n := fs.execs
	src.MaybeRefresh(context.Background())
	if fs.execs != n {
		t.Error("MaybeRefresh should be throttled")
	}

	// Update (mtime change) + removal + new skill.
	writeSkill(t, root, "bangumi", "bangumi", "updated description", t0.Add(time.Minute))
	if err := os.RemoveAll(filepath.Join(root, "skills", "ctrip-wendao")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "amap", "amap-lbs-skill", "高德地图", t0)
	src.now = func() time.Time { return time.Now().Add(time.Hour) }
	src.MaybeRefresh(context.Background())
	if bg, _ := mgr.Get("bangumi"); bg.Description != "updated description" {
		t.Errorf("bangumi not updated: %q", bg.Description)
	}
	if _, ok := mgr.Get("ctrip-wendao"); ok {
		t.Error("removed workspace skill must be unregistered")
	}
	if _, ok := mgr.Get("amap-lbs-skill"); !ok {
		t.Error("new workspace skill not discovered")
	}
	if _, ok := mgr.Get("pdf"); !ok {
		t.Error("bundled pdf must survive")
	}
}

func TestWorkspaceSource_LazyRefreshFromTools(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "bangumi", "bangumi", "anime tracking", time.Now())
	mgr := NewSkillManager(nil, nil, nil)
	src := NewWorkspaceSource(&localWorkspaceFS{root: root, workDir: "/data"}, mgr, nil)
	src.Attach() // no initial Refresh: tools must trigger it

	res, err := mgr.BuildSkillSearchTool().Execute(&llm.ToolExecContext{Context: context.Background()}, SkillSearchInput{Query: "bangumi"})
	if err != nil {
		t.Fatal(err)
	}
	hits := res.(map[string]any)["skills"].([]SearchHit)
	if len(hits) != 1 || hits[0].Name != "bangumi" {
		t.Fatalf("skill_search should see the workspace skill after lazy refresh, got %v", hits)
	}
}

func TestParseListing(t *testing.T) {
	out := "S\tskills/b\t100.20\nR\tskills/b/scripts/x.sh\nR\tskills/other/y\nS\tskills/a\t5.6\n"
	got := parseListing(out)
	if len(got) != 2 || got[0].dir != "skills/a" || got[1].dir != "skills/b" {
		t.Fatalf("%+v", got)
	}
	if len(got[1].resources) != 1 {
		t.Errorf("foreign resource must be dropped: %v", got[1].resources)
	}
	if sigSize("100.20") != 20 || sigSize("") != 0 {
		t.Error("sigSize")
	}
}
