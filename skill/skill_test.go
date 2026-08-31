package skill

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// SkillManager 基础测试
// ============================================================================

func TestNewSkillManager(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	if mgr == nil {
		t.Fatal("NewSkillManager returned nil")
	}
	if mgr.List() == nil {
		t.Error("List() should return empty slice, not nil")
	}
}

func TestSkillManager_RegisterAndList(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)

	skill := &Skill{
		Name:        "pdf",
		Description: "处理 PDF 文件。当用户提到 PDF、需要提取 PDF 内容时使用。",
		Content:     "# PDF 技能\n\n使用 pdf_read 工具读取 PDF 内容。",
		Enabled:     true,
		Source:      "fs",
	}
	mgr.Register(skill)

	list := mgr.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(list))
	}
	if list[0].Name != "pdf" {
		t.Errorf("expected name 'pdf', got %q", list[0].Name)
	}
	if !list[0].Enabled {
		t.Error("expected enabled=true")
	}
	if !list[0].HasContent {
		t.Error("expected HasContent=true")
	}
}

func TestSkillManager_EnableDisable(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)

	skill := &Skill{
		Name:        "pdf",
		Description: "处理 PDF 文件。",
		Content:     "# PDF 技能",
		Enabled:     false, // 初始禁用
		Source:      "fs",
	}
	mgr.Register(skill)

	if mgr.IsEnabled("pdf") {
		t.Error("should be disabled initially")
	}

	// Enable
	if err := mgr.Enable("pdf"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !mgr.IsEnabled("pdf") {
		t.Error("should be enabled after Enable")
	}

	// Disable
	if err := mgr.Disable("pdf"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if mgr.IsEnabled("pdf") {
		t.Error("should be disabled after Disable")
	}

	// 幂等
	if err := mgr.Disable("pdf"); err != nil {
		t.Error("Disable on already disabled should be idempotent")
	}
	if err := mgr.Enable("pdf"); err != nil {
		t.Error("Enable on already enabled should be idempotent")
	}

	// 不存在的 skill
	if err := mgr.Enable("nonexistent"); err == nil {
		t.Error("Enable nonexistent should error")
	}
}

func TestSkillManager_GetInfo(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "xlsx",
		Description: "处理 Excel 表格。",
		Content:     "# XLSX 技能",
		Enabled:     true,
		Source:      "fs",
	})

	info, ok := mgr.GetInfo("xlsx")
	if !ok {
		t.Fatal("GetInfo should return true for existing skill")
	}
	if info.Name != "xlsx" {
		t.Errorf("expected name 'xlsx', got %q", info.Name)
	}

	_, ok = mgr.GetInfo("nonexistent")
	if ok {
		t.Error("GetInfo should return false for nonexistent skill")
	}
}

func TestSkillManager_EnabledNames(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{Name: "a", Description: "test", Enabled: true, Source: "fs"})
	mgr.Register(&Skill{Name: "b", Description: "test", Enabled: false, Source: "fs"})
	mgr.Register(&Skill{Name: "c", Description: "test", Enabled: true, Source: "fs"})

	names := mgr.EnabledNames()
	if len(names) != 2 {
		t.Fatalf("expected 2 enabled, got %d", len(names))
	}
	if names[0] != "a" || names[1] != "c" {
		t.Errorf("expected [a c], got %v", names)
	}
}

// ============================================================================
// BuildTriggerPrompt 测试
// ============================================================================

func TestBuildTriggerPrompt(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "处理 PDF 文件（提取文本、合并、拆分等）。当用户提到 PDF、需要提取 PDF 内容时使用。",
		Enabled:     true,
		Source:      "fs",
	})
	mgr.Register(&Skill{
		Name:        "xlsx",
		Description: "处理 Excel 表格。当用户提到表格、xlsx、数据处理时使用。",
		Enabled:     true,
		Source:      "fs",
	})
	mgr.Register(&Skill{
		Name:        "disabled-skill",
		Description: "此技能已禁用。",
		Enabled:     false,
		Source:      "fs",
	})

	prompt := mgr.BuildTriggerPrompt()
	if prompt == "" {
		t.Fatal("BuildTriggerPrompt should not return empty")
	}

	// 应该包含已启用的 skill
	if !contains(prompt, "pdf") {
		t.Error("trigger prompt should contain 'pdf'")
	}
	if !contains(prompt, "xlsx") {
		t.Error("trigger prompt should contain 'xlsx'")
	}
	// 不应包含已禁用的 skill
	if contains(prompt, "disabled-skill") {
		t.Error("trigger prompt should not contain disabled skill")
	}
	// 应该包含触发指令（use_skill 工具调用方式）
	if !contains(prompt, "use_skill") {
		t.Error("trigger prompt should contain 'use_skill' instruction")
	}
}

// ============================================================================
// TriggerIfNeeded 测试
// ============================================================================

func TestTriggerIfNeeded(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)

	// 正常匹配
	output := "好的，我来帮你处理 PDF。\n<use_skill: pdf>\n\n这是处理后的结果..."
	name := mgr.TriggerIfNeeded(output)
	if name != "pdf" {
		t.Errorf("expected 'pdf', got %q", name)
	}

	// 无触发标签
	output2 := "你好！我是助手，有什么可以帮你？"
	name2 := mgr.TriggerIfNeeded(output2)
	if name2 != "" {
		t.Errorf("expected empty string, got %q", name2)
	}

	// 带空格
	output3 := "<use_skill:  xlsx  >"
	name3 := mgr.TriggerIfNeeded(output3)
	if name3 != "xlsx" {
		t.Errorf("expected 'xlsx', got %q", name3)
	}
}

// ============================================================================
// UseSkill / BuildUseSkillTool 测试（对齐 CodeBuddy use_skill 工具）
// ============================================================================

func TestUseSkill(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "PDF processing",
		Content:     "# PDF Skill\nProcess PDF files.",
		Enabled:     true,
	})
	mgr.Register(&Skill{
		Name:        "disabled-one",
		Description: "Disabled skill",
		Content:     "should not be accessible",
		Enabled:     false,
	})

	// 正常调用
	s, err := mgr.UseSkill("pdf")
	if err != nil {
		t.Fatalf("UseSkill('pdf'): %v", err)
	}
	if s.Name != "pdf" {
		t.Errorf("expected skill name 'pdf', got %q", s.Name)
	}
	if s.Content == "" {
		t.Error("returned skill should have content")
	}

	// 不存在的技能
	_, err = mgr.UseSkill("nonexistent")
	if err == nil {
		t.Error("UseSkill('nonexistent') should return error")
	}

	// 禁用的技能
	_, err = mgr.UseSkill("disabled-one")
	if err == nil {
		t.Error("UseSkill('disabled-one') should return error")
	}
}

func TestBuildUseSkillTool(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "PDF processing",
		Content:     "# PDF Skill\nProcess PDF files.",
		Enabled:     true,
	})

	tool := mgr.BuildUseSkillTool()
	if tool.Name != "use_skill" {
		t.Errorf("expected tool name 'use_skill', got %q", tool.Name)
	}
	if tool.Description == "" {
		t.Error("tool should have description")
	}
	if tool.Execute == nil {
		t.Error("tool should have Execute function")
	}
	if tool.Parameters == nil {
		t.Error("tool should have Parameters schema")
	}
}

func TestUseSkillTool_Execute(t *testing.T) {
	mgr := NewSkillRegistryForTest(t) // helper
	tool := mgr.BuildUseSkillTool()

	// 模拟 LLM 调用 use_skill
	result, err := tool.Execute(
		&llm.ToolExecContext{},
		UseSkillInput{Command: "pdf"},
	)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result should be map[string]any, got %T", result)
	}
	if m["status"] != "loaded" {
		t.Errorf("expected status 'loaded', got %v", m["status"])
	}
	if m["skill"] != "pdf" {
		t.Errorf("expected skill 'pdf', got %v", m["skill"])
	}
	content, _ := m["content"].(string)
	if content == "" {
		t.Error("result should contain skill content")
	}
}

func TestHasEnabledSkills(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	if mgr.HasEnabledSkills() {
		t.Error("empty manager should not have enabled skills")
	}

	mgr.Register(&Skill{
		Name:    "pdf",
		Content: "test",
		Enabled: true,
	})
	if !mgr.HasEnabledSkills() {
		t.Error("should have enabled skills after registration")
	}

	_ = mgr.Disable("pdf")
	if mgr.HasEnabledSkills() {
		t.Error("should not have enabled skills after disable")
	}
}

// NewSkillRegistryForTest creates a SkillManager with test data.
func NewSkillRegistryForTest(t *testing.T) *SkillManager {
	t.Helper()
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "PDF processing skill",
		Content:     "# PDF Skill\n\nProcess PDF files.",
		Enabled:     true,
	})
	mgr.Register(&Skill{
		Name:        "xlsx",
		Description: "Excel processing skill",
		Content:     "# XLSX Skill\n\nProcess Excel files.",
		Enabled:     true,
	})
	return mgr
}

// ============================================================================
// Loader 测试
// ============================================================================

func TestLoader_LoadSkill(t *testing.T) {
	// 创建临时 SKILL.md 文件
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "pdf")
	_ = os.MkdirAll(skillDir, 0755)

	content := `---
name: pdf
description: 处理 PDF 文件（提取文本、合并、拆分等）。当用户提到 PDF、需要提取 PDF 内容时使用。
compatibility: [pdf_read_tool, pdf_merge_tool]
enabled: true
---

# PDF 处理技能

## 指令

当用户请求处理 PDF 时，使用 pdf_read 工具读取内容。
`
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644)

	loader := NewLoader(tmpDir, nil)
	skill, err := loader.LoadSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}

	if skill.Name != "pdf" {
		t.Errorf("expected name 'pdf', got %q", skill.Name)
	}
	if skill.Description == "" {
		t.Error("Description should not be empty")
	}
	if skill.Content == "" {
		t.Error("Content should not be empty")
	}
	if !skill.Enabled {
		t.Error("Enabled should be true")
	}
	if len(skill.Compatibility) != 2 {
		t.Errorf("expected 2 compatibility items, got %d", len(skill.Compatibility))
	}
}

func TestLoader_LoadSkill_NoFrontMatter(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "simple")
	_ = os.MkdirAll(skillDir, 0755)

	content := `# 简单技能

这是一个没有 front matter 的技能。
`
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644)

	loader := NewLoader(tmpDir, nil)
	_, err := loader.LoadSkill(skillDir)
	// 无 name 字段时 LoadSkill 应返回 error
	if err == nil {
		t.Error("LoadSkill should error when name is missing (no front matter)")
	}
}

func TestLoader_LoadSkill_MissingRequiredField(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "bad")
	_ = os.MkdirAll(skillDir, 0755)

	content := `---
description: 缺少 name 字段。
---

# Bad Skill
`
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644)

	loader := NewLoader(tmpDir, nil)
	_, err := loader.LoadSkill(skillDir)
	if err == nil {
		t.Error("LoadSkill should error when name is missing")
	}
}

func TestLoader_LoadAndRegister(t *testing.T) {
	tmpDir := t.TempDir()

	// 创建 pdf skill
	pdfDir := filepath.Join(tmpDir, "pdf")
	_ = os.MkdirAll(pdfDir, 0755)
	_ = os.WriteFile(filepath.Join(pdfDir, "SKILL.md"), []byte(`---
name: pdf
description: 处理 PDF 文件。
---
# PDF 技能
`), 0644)

	// 创建 xlsx skill
	xlsxDir := filepath.Join(tmpDir, "xlsx")
	_ = os.MkdirAll(xlsxDir, 0755)
	_ = os.WriteFile(filepath.Join(xlsxDir, "SKILL.md"), []byte(`---
name: xlsx
description: 处理 Excel 表格。
---
# XLSX 技能
`), 0644)

	mgr := NewSkillManager(nil, nil, nil)
	loader := NewLoader(tmpDir, nil)

	loaded, err := loader.LoadAll(func(s *Skill) {
		mgr.Register(s)
	})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if loaded != 2 {
		t.Errorf("expected 2 loaded, got %d", loaded)
	}

	list := mgr.List()
	if len(list) != 2 {
		t.Errorf("expected 2 registered, got %d", len(list))
	}
}

// ============================================================================
// RegistryAdapter 集成测试（mock）
// ============================================================================

func TestSkillManager_InjectAndRemovePrompt(t *testing.T) {
	var registered []string
	var unregistered []string

	adapter := NewPromptRegistryAdapter(
		func(name string, order int, content string, enabled bool) {
			registered = append(registered, name)
		},
		func(name string) {
			unregistered = append(unregistered, name)
		},
	)

	mgr := NewSkillManager(adapter, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "处理 PDF 文件。",
		Content:     "# PDF 技能\n\n使用 pdf_read 工具。",
		Enabled:     true,
		Source:      "fs",
	})

	if len(registered) != 1 {
		t.Errorf("expected 1 registered section, got %d", len(registered))
	}
	if registered[0] != "skill_pdf" {
		t.Errorf("expected 'skill_pdf', got %q", registered[0])
	}

	// 禁用后应触发 unregister
	_ = mgr.Disable("pdf")
	if len(unregistered) != 1 {
		t.Errorf("expected 1 unregistered, got %d", len(unregistered))
	}
}

func TestBuildTriggerSection(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "处理 PDF 文件。",
		Enabled:     true,
		Source:      "fs",
	})

	section := mgr.BuildTriggerSection(150)
	if section.Name != "skill_trigger" {
		t.Errorf("expected section name 'skill_trigger', got %q", section.Name)
	}
	if section.Order != 150 {
		t.Errorf("expected order 150, got %d", section.Order)
	}
	if section.Content == "" {
		t.Error("section content should not be empty")
	}
	if !section.Enabled {
		t.Error("section should be enabled")
	}
}

// ============================================================================
// DirectInjector 测试
// ============================================================================

func TestDirectInjector_Inject(t *testing.T) {
	injector := NewDirectInjector()

	basePrompt := "你是一个有帮助的助手。"
	skills := []*Skill{
		{
			Name:    "pdf",
			Content: "使用 pdf_read 工具读取 PDF。",
			Enabled: true,
		},
		{
			Name:    "xlsx",
			Content: "使用 xlsx_read 工具读取表格。",
			Enabled: true,
		},
	}

	result := injector.Inject(basePrompt, skills...)

	if !contains(result, "你是一个有帮助的助手") {
		t.Error("result should contain base prompt")
	}
	if !contains(result, "## Skill: pdf") {
		t.Error("result should contain '## Skill: pdf'")
	}
	if !contains(result, "## Skill: xlsx") {
		t.Error("result should contain '## Skill: xlsx'")
	}
}

func TestDirectInjector_Remove(t *testing.T) {
	injector := NewDirectInjector()
	injector.Inject("base", &Skill{Name: "pdf", Content: "pdf content", Enabled: true})
	injector.Inject("base", &Skill{Name: "xlsx", Content: "xlsx content", Enabled: true})

	if len(injector.SkillNames()) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(injector.SkillNames()))
	}

	injector.Remove("pdf")
	if len(injector.SkillNames()) != 1 {
		t.Errorf("expected 1 skill after remove, got %d", len(injector.SkillNames()))
	}
	if injector.SkillNames()[0] != "xlsx" {
		t.Errorf("expected remaining skill 'xlsx', got %q", injector.SkillNames()[0])
	}
}

func TestDirectInjector_Clear(t *testing.T) {
	injector := NewDirectInjector()
	injector.Inject("base", &Skill{Name: "pdf", Content: "pdf content", Enabled: true})

	injector.Clear()
	if len(injector.SkillNames()) != 0 {
		t.Errorf("expected 0 skills after clear, got %d", len(injector.SkillNames()))
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstr(s, substr))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
