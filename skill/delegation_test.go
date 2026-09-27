package skill

// delegation_test.go 覆盖技能分级与委托执行提示：
//   - front matter delegation 字段解析（applyFrontMatterField / parseFrontMatter）
//   - 体积自动分级边界值（3000 字节上下：2999/3000 为 light，3001 为 heavy）
//   - 显式 delegation: preferred 声明优先于体积阈值（小体积也 heavy）
//   - 显式非 preferred 取值（none）不升级为 heavy
//   - SearchHit 携带 light/heavy 标注（SearchSkills 与 skill_search 工具返回）
//   - use_skill 重型技能返回委托提示（引用现有 spawn 工具名），轻型不提示
//
// 注意：复用 skill_test.go 的 contains / containsSlice 辅助函数（同 package 可见），
// 不修改 skill_test.go 本身。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// delegation 字段解析
// ============================================================================

// TestApplyFrontMatterField_Delegation 单行 key: value 解析的边界情况。
func TestApplyFrontMatterField_Delegation(t *testing.T) {
	cases := []struct {
		val  string
		want string
	}{
		{"preferred", "preferred"},
		{"  preferred  ", "preferred"}, // 值两侧空白
		{`"preferred"`, "preferred"},   // 双引号包裹
		{"'preferred'", "preferred"},   // 单引号包裹
		{"", ""},                       // 空值（未声明）
		{"none", "none"},               // 其他取值原样保存（分级时按未声明处理）
		{"PREFERRED", "PREFERRED"},     // 大小写敏感原样保存（IsHeavy 只认小写 preferred）
	}
	for _, c := range cases {
		var meta SkillMeta
		applyFrontMatterField("delegation", c.val, &meta)
		if meta.Delegation != c.want {
			t.Errorf("applyFrontMatterField(delegation, %q) = %q, want %q", c.val, meta.Delegation, c.want)
		}
	}

	// 未写 delegation 字段 → 保持零值（向后兼容：旧 SKILL.md 不受影响）
	var meta SkillMeta
	applyFrontMatterField("name", "pdf", &meta)
	if meta.Delegation != "" {
		t.Errorf("delegation should stay empty when field absent, got %q", meta.Delegation)
	}
}

// TestParseFrontMatter_Delegation 完整 front matter 管线（块标量路径不受影响）。
func TestParseFrontMatter_Delegation(t *testing.T) {
	meta, _ := parseFrontMatter("---\n" +
		"name: pdf\ndescription: 处理 PDF 文件。\ndelegation: preferred\n" +
		"---\n# PDF 技能\n")
	if meta.Name != "pdf" || meta.Description != "处理 PDF 文件。" {
		t.Fatalf("adjacent fields should parse as before, got %+v", meta)
	}
	if meta.Delegation != "preferred" {
		t.Errorf("delegation should parse to 'preferred', got %q", meta.Delegation)
	}
}

// TestLoader_LoadSkill_Delegation 从文件系统加载：字段透传到 Skill。
func TestLoader_LoadSkill_Delegation(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "pdf")
	_ = os.MkdirAll(skillDir, 0755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\n"+
		"name: pdf\ndescription: 处理 PDF 文件。\ndelegation: preferred\n"+
		"---\n\n# PDF 技能\n"), 0644)

	s, err := NewLoader(tmpDir, nil).LoadSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if s.Delegation != DelegationPreferred {
		t.Errorf("loader should pass delegation through, got %q", s.Delegation)
	}
	// 小体积 + 显式声明 → heavy（声明优先于阈值）
	if !s.IsHeavy() {
		t.Error("delegation: preferred should make a small skill heavy")
	}
}

// ============================================================================
// 体积自动分级（边界值 3000 字节）
// ============================================================================

// contentOfLen 生成恰好 n 字节的 ASCII 正文（字节数即字符数，边界精确可控）。
func contentOfLen(n int) string {
	return strings.Repeat("a", n)
}

// TestSkillIsHeavy_ContentThreshold 体积阈值边界：
// 未声明 delegation 时，<= 3000 字节为 light，> 3000 字节为 heavy。
func TestSkillIsHeavy_ContentThreshold(t *testing.T) {
	cases := []struct {
		contentLen int
		heavy      bool
	}{
		{0, false}, // 空正文
		{1, false},
		{SkillHeavyContentBytes - 1, false}, // 2999
		{SkillHeavyContentBytes, false},     // 3000：恰好等于阈值仍为 light
		{SkillHeavyContentBytes + 1, true},  // 3001：超过阈值
		{SkillHeavyContentBytes * 10, true}, // 远超
	}
	for _, c := range cases {
		s := &Skill{Name: "x", Description: "d", Content: contentOfLen(c.contentLen), Enabled: true}
		if got := s.IsHeavy(); got != c.heavy {
			t.Errorf("content len %d: IsHeavy() = %v, want %v", c.contentLen, got, c.heavy)
		}
		want := SkillLevelLight
		if c.heavy {
			want = SkillLevelHeavy
		}
		if got := s.Level(); got != want {
			t.Errorf("content len %d: Level() = %q, want %q", c.contentLen, got, want)
		}
	}
}

// TestSkillIsHeavy_DeclarationOverridesSize 显式声明优先于体积：
// preferred 让小体积升级为 heavy；none 不能让超大体积降级为 light。
func TestSkillIsHeavy_DeclarationOverridesSize(t *testing.T) {
	// preferred + 小体积 → heavy
	small := &Skill{Name: "x", Description: "d", Content: "tiny", Delegation: DelegationPreferred}
	if !small.IsHeavy() || small.Level() != SkillLevelHeavy {
		t.Error("delegation: preferred should force heavy regardless of size")
	}
	// none + 超大体积 → 仍 heavy（体积阈值兜底，none 只是不显式升级）
	hugeNone := &Skill{
		Name: "y", Description: "d",
		Content:    contentOfLen(SkillHeavyContentBytes * 4),
		Delegation: "none",
	}
	if !hugeNone.IsHeavy() {
		t.Error("oversized content should stay heavy even with delegation: none")
	}
	// none + 小体积 → light
	smallNone := &Skill{Name: "z", Description: "d", Content: "tiny", Delegation: "none"}
	if smallNone.IsHeavy() || smallNone.Level() != SkillLevelLight {
		t.Error("delegation: none with small content should be light")
	}
}

// ============================================================================
// SearchHit 的 light/heavy 标注
// ============================================================================

// newDelegationManager 构造三个分级样本技能（light / 声明 heavy / 体积 heavy）。
func newDelegationManager(t *testing.T) *SkillManager {
	t.Helper()
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name: "light-skill", Description: "shared doc keyword, small",
		Content: "# light", Enabled: true,
	})
	mgr.Register(&Skill{
		Name: "declared-skill", Description: "shared doc keyword, declared",
		Content: "# declared", Enabled: true, Delegation: DelegationPreferred,
	})
	mgr.Register(&Skill{
		Name: "bulky-skill", Description: "shared doc keyword, bulky",
		Content: contentOfLen(SkillHeavyContentBytes + 100), Enabled: true,
	})
	return mgr
}

// TestSearchSkills_HitLevelAnnotation 命中摘要必须携带 light/heavy 标注。
func TestSearchSkills_HitLevelAnnotation(t *testing.T) {
	hits := newDelegationManager(t).SearchSkills("doc", 10)
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d: %v", len(hits), hitNames(hits))
	}
	want := map[string]string{
		"light-skill":    SkillLevelLight,
		"declared-skill": SkillLevelHeavy,
		"bulky-skill":    SkillLevelHeavy,
	}
	for _, h := range hits {
		got, ok := want[h.Name]
		if !ok {
			t.Fatalf("unexpected hit %q", h.Name)
		}
		if h.Level != got {
			t.Errorf("hit %q level = %q, want %q", h.Name, h.Level, got)
		}
	}
}

// TestSkillSearchTool_ExecuteLevelAnnotation skill_search 工具返回同样带标注。
func TestSkillSearchTool_ExecuteLevelAnnotation(t *testing.T) {
	tool := newDelegationManager(t).BuildSkillSearchTool()
	result, err := tool.Execute(&llm.ToolExecContext{}, SkillSearchInput{Query: "doc"})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result should be map[string]any, got %T", result)
	}
	hits, _ := m["skills"].([]SearchHit)
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(hits))
	}
	for _, h := range hits {
		if h.Level != SkillLevelLight && h.Level != SkillLevelHeavy {
			t.Errorf("hit %q should carry level tag, got %q", h.Name, h.Level)
		}
	}
	if h, _ := m["hint"].(string); !contains(h, "spawn") {
		t.Errorf("hint should mention spawn delegation for heavy hits, got %q", m["hint"])
	}
}

// TestBuildSkillListPrompt_LevelAnnotation use_skill "list" 清单带 [light]/[heavy] 标注。
func TestBuildSkillListPrompt_LevelAnnotation(t *testing.T) {
	got := newDelegationManager(t).BuildSkillListPrompt()
	if !contains(got, "- light-skill [light] — ") {
		t.Errorf("light entry should be tagged [light], got:\n%s", got)
	}
	if !contains(got, "- declared-skill [heavy] — ") {
		t.Errorf("declared entry should be tagged [heavy], got:\n%s", got)
	}
	if !contains(got, "- bulky-skill [heavy] — ") {
		t.Errorf("bulky entry should be tagged [heavy], got:\n%s", got)
	}
}

// ============================================================================
// use_skill 重型技能的委托提示
// ============================================================================

// TestDelegationNote 委托提示文案引用现有 spawn 工具名（不新造能力），
// 且只在长任务时建议委托、并让子代理自己用 use_skill 加载技能。
func TestDelegationNote(t *testing.T) {
	note := DelegationNote()
	for _, want := range []string{"spawn", "use_skill", "long", "short task", "do not spawn"} {
		if !contains(note, want) {
			t.Errorf("delegation note should mention %q, got %q", want, note)
		}
	}
}

// TestUseSkillTool_HeavySkillDelegationNote 重型技能：返回 level=heavy + 委托提示；
// 轻型技能：level=light 且不附带提示（避免噪音）。
func TestUseSkillTool_HeavySkillDelegationNote(t *testing.T) {
	tool := newDelegationManager(t).BuildUseSkillTool()

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

	// 轻型：不提示
	light := exec(t, "light-skill")
	if light["status"] != "loaded" {
		t.Errorf("expected status 'loaded', got %v", light["status"])
	}
	if light["level"] != SkillLevelLight {
		t.Errorf("light skill level = %v, want %q", light["level"], SkillLevelLight)
	}
	if light["note"] != nil {
		t.Errorf("light skill should NOT carry delegation note, got %v", light["note"])
	}

	// 声明重型（delegation: preferred）：提示委托执行
	declared := exec(t, "declared-skill")
	if declared["level"] != SkillLevelHeavy {
		t.Errorf("declared skill level = %v, want %q", declared["level"], SkillLevelHeavy)
	}
	note, ok := declared["note"].(string)
	if !ok || !contains(note, "spawn") {
		t.Errorf("heavy skill should carry delegation note mentioning spawn, got %v", declared["note"])
	}

	// 体积重型（> 3000 字节）：同样提示
	bulky := exec(t, "bulky-skill")
	if bulky["level"] != SkillLevelHeavy {
		t.Errorf("bulky skill level = %v, want %q", bulky["level"], SkillLevelHeavy)
	}
	if note, _ := bulky["note"].(string); !contains(note, "spawn") {
		t.Errorf("bulky skill should carry delegation note mentioning spawn, got %v", bulky["note"])
	}
}

// TestUseSkillTool_DelegationNoteAtResultEnd 委托提示在返回结果的「末尾」以
// 独立 note 字段呈现（content 保持原样，提示不混入正文）。
func TestUseSkillTool_DelegationNoteAtResultEnd(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name: "bulky", Description: "bulky skill",
		Content: contentOfLen(SkillHeavyContentBytes + 1), Enabled: true,
	})
	tool := mgr.BuildUseSkillTool()

	result, err := tool.Execute(&llm.ToolExecContext{}, UseSkillInput{Command: "bulky"})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	m := result.(map[string]any)
	if c, _ := m["content"].(string); c != contentOfLen(SkillHeavyContentBytes+1) {
		t.Error("content must stay intact (note lives in its own field)")
	}
	if _, hasNote := m["note"]; !hasNote {
		t.Error("heavy skill result should carry a note field")
	}
}

// ============================================================================
// 提示词文案（BuildTriggerPrompt / use_skill description）补充分级说明
// ============================================================================

// TestBuildTriggerPrompt_MentionsDelegation 触发指引提及 light/heavy 分级与 spawn。
func TestBuildTriggerPrompt_MentionsDelegation(t *testing.T) {
	prompt := newDelegationManager(t).BuildTriggerPrompt()
	if !contains(prompt, "heavy") || !contains(prompt, "light") {
		t.Error("trigger prompt should explain light/heavy grading")
	}
	if !contains(prompt, "spawn") {
		t.Error("trigger prompt should mention the existing spawn tool for heavy skills")
	}
	// 自启发性质保持：不内联具体技能名
	for _, forbidden := range []string{"bulky-skill", "declared-skill"} {
		if contains(prompt, forbidden) {
			t.Errorf("trigger prompt should NOT inline skill name %q", forbidden)
		}
	}
}

// TestBuildUseSkillTool_DescriptionMentionsDelegation use_skill 描述含分级说明。
func TestBuildUseSkillTool_DescriptionMentionsDelegation(t *testing.T) {
	desc := newDelegationManager(t).BuildUseSkillTool().Description
	for _, want := range []string{"heavy", "light", "spawn"} {
		if !contains(desc, want) {
			t.Errorf("use_skill description should mention %q for delegation guidance", want)
		}
	}
}
