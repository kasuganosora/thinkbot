package skill

import (
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 技能卸载（unload）测试 — 加载 → 卸载 → 重新加载全链路
//
// 覆盖点：
//   - UseSkill 成功后登记 loaded 集合（IsLoaded / LoadedNames）
//   - UnloadSkill 卸载：从 Registry 移除 Section、loaded 集合清空、返回标记消息
//   - 卸载未加载技能：返回 ok=false（非 error），工具层给友好提示
//   - 卸载后再次 use_skill 同名技能可重新加载全文（不因曾卸载而拒绝）
//   - use_skill 工具的 "unload:<skill>" 前缀解析与结构化返回
// ============================================================================

// mockRegistry 记录 Section 注册/注销历史的测试 Registry（并发安全）。
type mockRegistry struct {
	mu           sync.Mutex
	sections     map[string]string // name -> 当前 content（注销即删除）
	unregistered []string          // 被注销过的 Section 名称（按顺序，可重复）
}

func newMockRegistry() *mockRegistry {
	return &mockRegistry{sections: make(map[string]string)}
}

func (r *mockRegistry) RegisterSection(name string, order int, content string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sections[name] = content
}

func (r *mockRegistry) UnregisterSection(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sections, name)
	r.unregistered = append(r.unregistered, name)
}

// has 断言辅助：Section 是否存在。
func (r *mockRegistry) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sections[name]
	return ok
}

// unregisterCount 返回指定 Section 被注销的次数。
func (r *mockRegistry) unregisterCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.unregistered {
		if s == name {
			n++
		}
	}
	return n
}

// newUnloadTestManager 构造带 mock Registry 的 SkillManager，并注册一个 pdf 技能。
func newUnloadTestManager(t *testing.T) (*SkillManager, *mockRegistry) {
	t.Helper()
	reg := newMockRegistry()
	mgr := NewSkillManager(reg, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "处理 PDF 文件。",
		Content:     "# PDF 技能\n\n使用 pdf_read 工具读取 PDF 内容。",
		Enabled:     true,
		Source:      "fs",
	})
	return mgr, reg
}

// TestUnloadSkill_LoadUnloadReload 全链路：加载 → 卸载 → 重新加载。
func TestUnloadSkill_LoadUnloadReload(t *testing.T) {
	mgr, reg := newUnloadTestManager(t)

	// 初始未加载
	if mgr.IsLoaded("pdf") {
		t.Fatal("skill should not be loaded initially")
	}

	// 1. 加载：登记 loaded，且 prompt Section 存在
	s, err := mgr.UseSkill("pdf")
	if err != nil {
		t.Fatalf("UseSkill('pdf'): %v", err)
	}
	if s.Content == "" {
		t.Fatal("UseSkill should return full content")
	}
	if !mgr.IsLoaded("pdf") {
		t.Fatal("skill should be loaded after UseSkill")
	}
	if !reg.has("skill_pdf") {
		t.Fatal("skill_pdf section should be registered after UseSkill")
	}

	// 2. 卸载：Section 移除、loaded 清空、返回标记消息
	note, ok := mgr.UnloadSkill("pdf")
	if !ok {
		t.Fatal("UnloadSkill on a loaded skill should return ok=true")
	}
	if !contains(note, "[skill unloaded: pdf]") {
		t.Errorf("unload note should contain marker, got %q", note)
	}
	if !contains(note, "重新 use_skill 加载") {
		t.Errorf("unload note should mention reloading, got %q", note)
	}
	if mgr.IsLoaded("pdf") {
		t.Fatal("skill should not be loaded after UnloadSkill")
	}
	if reg.has("skill_pdf") {
		t.Fatal("skill_pdf section should be removed from registry after unload")
	}
	if got := reg.unregisterCount("skill_pdf"); got != 1 {
		t.Errorf("expected exactly 1 unregister of skill_pdf, got %d", got)
	}
	// 卸载不影响技能的注册与启用（可再次加载的前提）
	if !mgr.IsEnabled("pdf") {
		t.Fatal("unload must not change Enabled state")
	}
	if _, ok := mgr.GetInfo("pdf"); !ok {
		t.Fatal("unload must not unregister the skill itself")
	}

	// 3. 重新加载：必须能重新加载全文（不因曾卸载而拒绝）
	s2, err := mgr.UseSkill("pdf")
	if err != nil {
		t.Fatalf("UseSkill after unload should succeed, got: %v", err)
	}
	if s2.Content != s.Content {
		t.Errorf("reloaded content should equal original, got %q vs %q", s2.Content, s.Content)
	}
	if !mgr.IsLoaded("pdf") {
		t.Fatal("skill should be loaded again after re-UseSkill")
	}
	if !reg.has("skill_pdf") {
		t.Fatal("skill_pdf section should be re-registered after reload")
	}
}

// TestUnloadSkill_NotLoaded 卸载未加载技能：返回友好提示（非 error）。
func TestUnloadSkill_NotLoaded(t *testing.T) {
	mgr, reg := newUnloadTestManager(t)

	// 场景 1：从未加载过
	_, ok := mgr.UnloadSkill("pdf")
	if ok {
		t.Fatal("UnloadSkill on a never-loaded skill should return ok=false")
	}
	if reg.unregisterCount("skill_pdf") != 0 {
		t.Errorf("nothing to unregister, got %d calls", reg.unregisterCount("skill_pdf"))
	}

	// 场景 2：不存在的技能名（同样按未加载处理，非 error）
	if _, ok := mgr.UnloadSkill("nonexistent"); ok {
		t.Fatal("UnloadSkill on nonexistent skill should return ok=false")
	}

	// 场景 3：已卸载后再次卸载 → 仍为未加载
	if _, err := mgr.UseSkill("pdf"); err != nil {
		t.Fatalf("UseSkill: %v", err)
	}
	if _, ok := mgr.UnloadSkill("pdf"); !ok {
		t.Fatal("first unload should succeed")
	}
	if _, ok := mgr.UnloadSkill("pdf"); ok {
		t.Fatal("second unload should return ok=false (already unloaded)")
	}

	// 场景 4：禁用后 loaded 同步失效
	if _, err := mgr.UseSkill("pdf"); err != nil {
		t.Fatalf("UseSkill: %v", err)
	}
	if err := mgr.Disable("pdf"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if mgr.IsLoaded("pdf") {
		t.Fatal("Disable should clear loaded state")
	}
	if _, ok := mgr.UnloadSkill("pdf"); ok {
		t.Fatal("unload after disable should return ok=false")
	}
}

// TestLoadedNames loaded 集合的多技能查询。
func TestLoadedNames(t *testing.T) {
	reg := newMockRegistry()
	mgr := NewSkillManager(reg, nil, nil)
	mgr.Register(&Skill{Name: "b", Description: "B", Content: "# b", Enabled: true})
	mgr.Register(&Skill{Name: "a", Description: "A", Content: "# a", Enabled: true})

	if len(mgr.LoadedNames()) != 0 {
		t.Fatalf("expected no loaded skills, got %v", mgr.LoadedNames())
	}

	// 加载两个，卸载一个 → 剩余按名称排序
	if _, err := mgr.UseSkill("b"); err != nil {
		t.Fatalf("UseSkill('b'): %v", err)
	}
	if _, err := mgr.UseSkill("a"); err != nil {
		t.Fatalf("UseSkill('a'): %v", err)
	}
	if _, ok := mgr.UnloadSkill("b"); !ok {
		t.Fatal("UnloadSkill('b') should succeed")
	}

	names := mgr.LoadedNames()
	if len(names) != 1 || names[0] != "a" {
		t.Fatalf("expected [a], got %v", names)
	}
}

// TestParseUnloadCommand "unload:" 前缀解析的边界情况。
func TestParseUnloadCommand(t *testing.T) {
	cases := []struct {
		command string
		want    string
		ok      bool
	}{
		{"unload:pdf", "pdf", true},
		{"unload:  pdf  ", "pdf", true}, // 允许冒号后空白
		{"pdf", "", false},              // 普通技能名
		{"list", "", false},             // list 子命令
		{"unload:", "", false},          // 空技能名 → 按普通名处理
		{"unload", "", false},           // 缺冒号 → 按普通名处理
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := parseUnloadCommand(c.command)
		if got != c.want || ok != c.ok {
			t.Errorf("parseUnloadCommand(%q) = (%q, %v), want (%q, %v)",
				c.command, got, ok, c.want, c.ok)
		}
	}
}

// TestUseSkillTool_ExecuteUnload use_skill 工具的卸载子命令端到端。
func TestUseSkillTool_ExecuteUnload(t *testing.T) {
	mgr, reg := newUnloadTestManager(t)
	tool := mgr.BuildUseSkillTool()

	exec := func(t *testing.T, command string) map[string]any {
		t.Helper()
		result, err := tool.Execute(&llm.ToolExecContext{}, UseSkillInput{Command: command})
		if err != nil {
			t.Fatalf("Execute(%q) failed: %v", command, err)
		}
		m, ok := result.(map[string]any)
		if !ok {
			t.Fatalf("result should be map[string]any, got %T", result)
		}
		return m
	}

	// 未加载直接卸载 → 友好提示，不算错误
	m := exec(t, "unload:pdf")
	if m["status"] != "not_loaded" {
		t.Errorf("expected status 'not_loaded', got %v", m["status"])
	}
	if note, _ := m["note"].(string); !contains(note, "nothing to unload") {
		t.Errorf("note should be friendly hint, got %v", m["note"])
	}

	// 加载 → 卸载 → 结构化 unloaded 结果
	_ = exec(t, "pdf")
	if !mgr.IsLoaded("pdf") {
		t.Fatal("should be loaded after use_skill")
	}
	m = exec(t, "unload:pdf")
	if m["status"] != "unloaded" {
		t.Errorf("expected status 'unloaded', got %v", m["status"])
	}
	if m["skill"] != "pdf" {
		t.Errorf("expected skill 'pdf', got %v", m["skill"])
	}
	if note, _ := m["note"].(string); !contains(note, "[skill unloaded: pdf]") {
		t.Errorf("note should contain unload marker, got %v", m["note"])
	}
	if reg.has("skill_pdf") {
		t.Error("skill_pdf section should be removed after unload")
	}

	// 卸载后可重新加载全文
	m = exec(t, "pdf")
	if m["status"] != "loaded" {
		t.Errorf("reload should have status 'loaded', got %v", m["status"])
	}
	if c, _ := m["content"].(string); c == "" {
		t.Error("reload should return full content again")
	}
	if !mgr.IsLoaded("pdf") {
		t.Error("should be loaded again after reload")
	}
}

// TestUnloadSkill_UnregisterClearsLoaded Unregister 同步清理 loaded 集合。
func TestUnloadSkill_UnregisterClearsLoaded(t *testing.T) {
	reg := newMockRegistry()
	mgr := NewSkillManager(reg, nil, nil)
	mgr.Register(&Skill{Name: "pdf", Description: "PDF", Content: "# pdf", Enabled: true})

	if _, err := mgr.UseSkill("pdf"); err != nil {
		t.Fatalf("UseSkill: %v", err)
	}
	mgr.Unregister("pdf")
	if mgr.IsLoaded("pdf") {
		t.Fatal("Unregister should clear loaded state")
	}
	if _, ok := mgr.UnloadSkill("pdf"); ok {
		t.Fatal("unload after unregister should return ok=false")
	}
}
