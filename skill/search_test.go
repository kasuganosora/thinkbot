package skill

// search_test.go 覆盖 skill_search 粗检索（L1）的边界条件：
// 多关键词全命中、部分命中淘汰、name 命中排名高于 description 命中、
// 大小写不敏感、空查询、无命中、limit 边界（0/负数/1/20/21/100）、
// 描述超 200 rune 截断、BuildSkillSearchTool 返回结构、BuildSkillListPrompt 截断。
//
// 注意：复用 skill_test.go 中的 NewSkillRegistryForTest 与 contains 辅助函数
// （同 package 可见），不修改 skill_test.go 本身。

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 测试夹具
// ============================================================================

// newSearchManager 构建带固定样本技能的 SkillManager。
// 样本设计（检索用，全部 Enabled）：
//   - pdf：name 含 "pdf"，description 含 "extract"
//   - pdf-extract：两个关键词都命中 name（用于验证 name 命中排名更高）
//   - xlsx：只在 description 中含 "excel"（对照组）
//   - disabled-one：含 "pdf" 但已禁用（验证只检索已启用技能）
func newSearchManager(t *testing.T) *SkillManager {
	t.Helper()
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "pdf",
		Description: "PDF processing skill, extract text and tables",
		Content:     "# PDF Skill",
		Enabled:     true,
	})
	mgr.Register(&Skill{
		Name:        "pdf-extract",
		Description: "Extract pages from documents",
		Content:     "# PDF Extract",
		Enabled:     true,
	})
	mgr.Register(&Skill{
		Name:        "xlsx",
		Description: "Excel processing skill",
		Content:     "# XLSX Skill",
		Enabled:     true,
	})
	mgr.Register(&Skill{
		Name:        "disabled-one",
		Description: "a pdf skill that is disabled",
		Content:     "# Disabled",
		Enabled:     false,
	})
	return mgr
}

// longDescription 生成 n 个 rune 的描述（用中文字符验证多字节安全）。
func longDescription(n int) string {
	return strings.Repeat("长", n)
}

// hitNames 提取命中列表的技能名，便于断言排序。
func hitNames(hits []SearchHit) []string {
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.Name)
	}
	return names
}

// ============================================================================
// 匹配语义
// ============================================================================

func TestSearchSkills_MultiKeywordAllMatch(t *testing.T) {
	mgr := newSearchManager(t)

	// 两个关键词全部命中（pdf 命中 pdf 的 name；extract 命中其 description）
	hits := mgr.SearchSkills("pdf extract", 10)
	if len(hits) == 0 {
		t.Fatal("expected hits for 'pdf extract'")
	}
	names := hitNames(hits)
	if !containsSlice(names, "pdf") || !containsSlice(names, "pdf-extract") {
		t.Errorf("expected pdf and pdf-extract, got %v", names)
	}
	// 禁用技能不得出现
	if containsSlice(names, "disabled-one") {
		t.Errorf("disabled skill should not be searchable, got %v", names)
	}
	// xlsx 两个关键词都不含，不应出现
	if containsSlice(names, "xlsx") {
		t.Errorf("xlsx should not match 'pdf extract', got %v", names)
	}
}

func TestSearchSkills_PartialMatchExcluded(t *testing.T) {
	mgr := newSearchManager(t)

	// AND 语义：xlsx 命中 "excel" 但不含 "pdf" → 淘汰；无技能同时含两词 → 空结果
	hits := mgr.SearchSkills("excel pdf", 10)
	if len(hits) != 0 {
		t.Errorf("partial match should return empty, got %v", hitNames(hits))
	}
	// 单关键词部分命中同理：没有任何技能含 "excel" 于 name/description 之外的场景
	hits = mgr.SearchSkills("excel", 10)
	if len(hits) != 1 || hits[0].Name != "xlsx" {
		t.Errorf("expected only xlsx for 'excel', got %v", hitNames(hits))
	}
}

func TestSearchSkills_CaseInsensitive(t *testing.T) {
	mgr := newSearchManager(t)

	upper := mgr.SearchSkills("PDF EXTRACT", 10)
	if len(upper) == 0 {
		t.Fatal("uppercase query should match (case-insensitive)")
	}
	lower := mgr.SearchSkills("pdf extract", 10)
	if len(upper) != len(lower) {
		t.Errorf("case should not affect result count: upper=%d lower=%d", len(upper), len(lower))
	}
	for i := range upper {
		if upper[i].Name != lower[i].Name {
			t.Errorf("case changed ordering: upper=%v lower=%v", hitNames(upper), hitNames(lower))
			break
		}
	}
}

func TestSearchSkills_EmptyQuery(t *testing.T) {
	mgr := newSearchManager(t)

	for _, q := range []string{"", "   ", "\t\n "} {
		hits := mgr.SearchSkills(q, 10)
		if len(hits) != 0 {
			t.Errorf("query %q should return empty, got %v", q, hitNames(hits))
		}
		if hits == nil {
			t.Errorf("query %q should return non-nil empty slice", q)
		}
	}

	// 空 manager 上同样返回空切片（非 nil）
	empty := NewSkillManager(nil, nil, nil).SearchSkills("pdf", 10)
	if len(empty) != 0 || empty == nil {
		t.Errorf("empty manager should return non-nil empty slice, got %#v", empty)
	}
}

func TestSearchSkills_NoMatch(t *testing.T) {
	mgr := newSearchManager(t)

	hits := mgr.SearchSkills("nonexistent", 10)
	if len(hits) != 0 {
		t.Errorf("expected empty result, got %v", hitNames(hits))
	}
	if hits == nil {
		t.Error("no-match should return non-nil empty slice")
	}
}

// ============================================================================
// 计分与排序
// ============================================================================

func TestSearchSkills_NameRankAboveDescription(t *testing.T) {
	mgr := newSearchManager(t)

	// "pdf"：pdf 与 pdf-extract 的 name 命中（各 +10）；
	// disabled-one 命中但已禁用，不参与。
	hits := mgr.SearchSkills("pdf", 10)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d: %v", len(hits), hitNames(hits))
	}
	// 同分（都仅 name 命中 +10）→ 按 name 字典序：pdf < pdf-extract
	if hits[0].Name != "pdf" || hits[1].Name != "pdf-extract" {
		t.Errorf("same score should sort by name asc, got %v", hitNames(hits))
	}
	if hits[0].Score != skillSearchNameWeight {
		t.Errorf("name hit should score %d, got %d", skillSearchNameWeight, hits[0].Score)
	}

	// name 命中 vs description 命中：构造对照
	mgr2 := NewSkillManager(nil, nil, nil)
	mgr2.Register(&Skill{Name: "aaa-doc", Description: "no keyword here", Enabled: true})
	mgr2.Register(&Skill{Name: "zzz", Description: "contains doc keyword", Enabled: true})
	got := mgr2.SearchSkills("doc", 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(got))
	}
	// aaa-doc：name 命中 +10；zzz：description 命中 +1 → 前者排前
	if got[0].Name != "aaa-doc" {
		t.Errorf("name hit should rank above description hit, got %v", hitNames(got))
	}
	if got[1].Name != "zzz" || got[1].Score != skillSearchDescWeight {
		t.Errorf("description hit should score %d, got %d (%s)",
			skillSearchDescWeight, got[1].Score, got[1].Name)
	}
}

func TestSearchSkills_TieBreakByName(t *testing.T) {
	// 三技能同分（均 description 单命中），按 name 字典序稳定输出
	mgr := NewSkillManager(nil, nil, nil)
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		mgr.Register(&Skill{Name: n, Description: "shared doc keyword", Enabled: true})
	}
	hits := mgr.SearchSkills("doc", 10)
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(hits))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if hits[i].Name != w {
			t.Errorf("tie should break by name asc: got %v want %v", hitNames(hits), want)
			break
		}
	}
}

// ============================================================================
// limit 边界
// ============================================================================

func TestSearchSkills_LimitBoundaries(t *testing.T) {
	// 构建 25 个均命中 "doc" 的技能
	mgr := NewSkillManager(nil, nil, nil)
	for i := 0; i < 25; i++ {
		mgr.Register(&Skill{
			Name:        "doc-skill-" + string(rune('a'+i)),
			Description: "shared doc keyword",
			Enabled:     true,
		})
	}

	cases := []struct {
		limit int
		want  int // 期望返回条数
	}{
		{0, 10},  // limit<=0 → 取默认 10
		{-1, 10}, // 负数 → 取默认 10
		{-100, 10},
		{1, 1}, // 正常截断
		{5, 5},
		{20, 20},  // 上限边界：20 合法
		{21, 10},  // 超 20 → 回落默认 10
		{100, 10}, // 远超 → 回落默认 10
	}
	for _, c := range cases {
		hits := mgr.SearchSkills("doc", c.limit)
		if len(hits) != c.want {
			t.Errorf("limit=%d: expected %d hits, got %d", c.limit, c.want, len(hits))
		}
	}
}

// ============================================================================
// 描述截断
// ============================================================================

func TestSearchSkills_DescriptionTruncation(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{
		Name:        "longdesc",
		Description: longDescription(500), // 500 个中文 rune，远超 200
		Enabled:     true,
	})
	mgr.Register(&Skill{
		Name:        "shortdesc",
		Description: longDescription(100), // 未超限，应原样返回
		Enabled:     true,
	})

	hits := mgr.SearchSkills("longdesc", 10)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	d := hits[0].Description
	// 截断后 = 200 rune + "…"（1 rune）
	if got := len([]rune(d)); got != maxDescriptionRunes+1 {
		t.Errorf("truncated description should be %d runes (200 + …), got %d", maxDescriptionRunes+1, got)
	}
	if !strings.HasSuffix(d, "…") {
		t.Errorf("truncated description should end with …, got %q", d)
	}

	hits = mgr.SearchSkills("shortdesc", 10)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Description != longDescription(100) {
		t.Error("description within limit should not be truncated")
	}
}

func TestSearchSkills_DescriptionTruncationBoundary(t *testing.T) {
	// 恰好 200 rune：不截断、无省略号；201 rune：截到 200 + …
	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(&Skill{Name: "exact", Description: longDescription(200), Enabled: true})
	mgr.Register(&Skill{Name: "over", Description: longDescription(201), Enabled: true})

	hits := mgr.SearchSkills("exact", 10)
	if len(hits) != 1 || hits[0].Description != longDescription(200) {
		t.Error("exactly 200 runes should not be truncated")
	}
	hits = mgr.SearchSkills("over", 10)
	if len(hits) != 1 {
		t.Fatal("expected 1 hit")
	}
	if d := hits[0].Description; len([]rune(d)) != 201 || !strings.HasSuffix(d, "…") {
		t.Errorf("201 runes should truncate to 200+…, got %d runes", len([]rune(d)))
	}
}

// ============================================================================
// buildSkillListLocked 截断（BuildSkillListPrompt 仍返回完整清单）
// ============================================================================

func TestBuildSkillListPrompt_DescriptionTruncation(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	long := longDescription(500)
	mgr.Register(&Skill{Name: "long", Description: long, Enabled: true})
	mgr.Register(&Skill{Name: "short", Description: "short one", Enabled: true})
	mgr.Register(&Skill{Name: "disabled", Description: long, Enabled: false})

	got := mgr.BuildSkillListPrompt()

	// 仍返回全部已启用条目（截断只作用于单条描述）
	if !contains(got, "- long — ") {
		t.Error("skill list should contain 'long' entry")
	}
	if !contains(got, "- short — short one") {
		t.Error("skill list should contain 'short' entry with full description")
	}
	if contains(got, "- disabled") {
		t.Error("skill list should not contain disabled skill")
	}
	// 超长描述被截断：原文 500 rune 不应完整出现
	if contains(got, long) {
		t.Error("long description should be truncated in skill list")
	}
	// 截断后的行应以 … 结尾
	if !contains(got, "…\n") {
		t.Error("truncated description line should end with …")
	}
	// 计数头不受影响
	if !contains(got, "2 skills available:") {
		t.Errorf("skill list header should say 2 skills, got %q", firstLine(got))
	}
}

// firstLine 取首行，用于失败信息。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ============================================================================
// skill_search 工具
// ============================================================================

func TestBuildSkillSearchTool(t *testing.T) {
	mgr := NewSkillRegistryForTest(t) // 复用 skill_test.go 的夹具

	tool := mgr.BuildSkillSearchTool()
	if tool.Name != "skill_search" {
		t.Errorf("expected tool name 'skill_search', got %q", tool.Name)
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

func TestSkillSearchTool_Execute(t *testing.T) {
	mgr := NewSkillRegistryForTest(t)
	tool := mgr.BuildSkillSearchTool()

	result, err := tool.Execute(
		&llm.ToolExecContext{},
		SkillSearchInput{Query: "pdf"},
	)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result should be map[string]any, got %T", result)
	}
	if m["status"] != "search" {
		t.Errorf("expected status 'search', got %v", m["status"])
	}
	hits, ok := m["skills"].([]SearchHit)
	if !ok {
		t.Fatalf("skills should be []SearchHit, got %T", m["skills"])
	}
	if len(hits) != 1 || hits[0].Name != "pdf" {
		t.Errorf("expected pdf hit, got %v", hitNames(hits))
	}
	if h, _ := m["hint"].(string); !contains(h, "use_skill") {
		t.Errorf("hint should mention use_skill, got %q", m["hint"])
	}
}

func TestSkillSearchTool_ExecuteMultiKeyword(t *testing.T) {
	mgr := NewSkillRegistryForTest(t)
	tool := mgr.BuildSkillSearchTool()

	// NewSkillRegistryForTest 样本：pdf → "PDF processing skill"，xlsx → "Excel processing skill"
	// "pdf process" 两词均命中 pdf；xlsx 不含任一词
	result, err := tool.Execute(
		&llm.ToolExecContext{},
		SkillSearchInput{Query: "pdf process"},
	)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	m, _ := result.(map[string]any)
	hits, _ := m["skills"].([]SearchHit)
	if len(hits) != 1 || hits[0].Name != "pdf" {
		t.Errorf("expected only pdf, got %v", hitNames(hits))
	}
}

func TestSkillSearchTool_ExecuteEmptyQuery(t *testing.T) {
	mgr := NewSkillRegistryForTest(t)
	tool := mgr.BuildSkillSearchTool()

	// 空 query：不报错，返回空结果 + hint，让 LLM 自行纠正
	result, err := tool.Execute(
		&llm.ToolExecContext{},
		SkillSearchInput{Query: "   "},
	)
	if err != nil {
		t.Fatalf("empty query should not error, got %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result should be map[string]any, got %T", result)
	}
	if m["status"] != "search" {
		t.Errorf("expected status 'search', got %v", m["status"])
	}
	hits, _ := m["skills"].([]SearchHit)
	if len(hits) != 0 {
		t.Errorf("empty query should return empty hits, got %v", hitNames(hits))
	}
	if m["hint"] == nil {
		t.Error("empty query result should still carry hint")
	}
}

func TestSkillSearchTool_ExecuteNoMatch(t *testing.T) {
	mgr := NewSkillRegistryForTest(t)
	tool := mgr.BuildSkillSearchTool()

	result, err := tool.Execute(
		&llm.ToolExecContext{},
		SkillSearchInput{Query: "nothing-matches-this", Limit: 5},
	)
	if err != nil {
		t.Fatalf("no-match query should not error, got %v", err)
	}
	m, _ := result.(map[string]any)
	if m["status"] != "search" {
		t.Errorf("expected status 'search', got %v", m["status"])
	}
	hits, _ := m["skills"].([]SearchHit)
	if len(hits) != 0 {
		t.Errorf("no-match should return empty hits, got %v", hitNames(hits))
	}
}

// ============================================================================
// SkillToolProvider — 应同时提供 use_skill 与 skill_search
// ============================================================================

func TestSkillToolProvider_ReturnsBothTools(t *testing.T) {
	// 无已启用技能：nil, nil
	empty := &SkillToolProvider{Manager: NewSkillManager(nil, nil, nil)}
	tools, err := empty.Tools(nil, nil)
	if err != nil {
		t.Fatalf("Tools should not error: %v", err)
	}
	if tools != nil {
		t.Errorf("empty manager should return nil tools, got %d", len(tools))
	}

	// 有已启用技能：use_skill + skill_search 两个
	p := &SkillToolProvider{Manager: newSearchManager(t)}
	tools, err = p.Tools(nil, nil)
	if err != nil {
		t.Fatalf("Tools should not error: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	names := []string{tools[0].Name, tools[1].Name}
	if names[0] != "use_skill" || names[1] != "skill_search" {
		t.Errorf("expected [use_skill skill_search], got %v", names)
	}
}

// ============================================================================
// buildTriggerPromptLocked — 发现指引提及 skill_search
// ============================================================================

func TestBuildTriggerPrompt_MentionsSkillSearch(t *testing.T) {
	// 复用 NewSkillRegistryForTest，避免在本文件重复造样本
	prompt := NewSkillRegistryForTest(t).BuildTriggerPrompt()

	if !contains(prompt, "skill_search") {
		t.Error("trigger prompt should mention skill_search as cheap first step")
	}
	if !contains(prompt, "use_skill") {
		t.Error("trigger prompt should still mention use_skill")
	}
	if !contains(prompt, "list") {
		t.Error("trigger prompt should still mention use_skill \"list\"")
	}
	// 不内联技能名（skill_test.go 的 TestBuildTriggerPrompt 性质保持）
	for _, forbidden := range []string{"pdf", "xlsx"} {
		if contains(prompt, forbidden) {
			t.Errorf("trigger prompt should NOT inline skill name %q", forbidden)
		}
	}
}

// ============================================================================
// use_skill 工具 description 的 DISCOVERY 微调
// ============================================================================

func TestBuildUseSkillTool_DescriptionMentionsSearch(t *testing.T) {
	tool := NewSkillRegistryForTest(t).BuildUseSkillTool()
	if !contains(tool.Description, "skill_search") {
		t.Error("use_skill description should prefer skill_search for keyword discovery")
	}
}
