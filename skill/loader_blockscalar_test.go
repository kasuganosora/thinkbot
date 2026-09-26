package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// 块标量（> 与 | 系列）解析测试
// ============================================================================

// TestParseFrontMatterBlockScalar 覆盖各块标量风格的精确语义（期望值经
// yaml.v3 差分基准核对，与真实 YAML 解析器逐字节一致）。
func TestParseFrontMatterBlockScalar(t *testing.T) {
	tests := []struct {
		name string
		fm   string
		desc string // 期望的 meta.Description
	}{
		{
			name: "folded-strip",
			fm: "name: pdf\n" +
				"description: >-\n" +
				"  Line one\n" +
				"  Line two\n" +
				"  Line three",
			desc: "Line one Line two Line three",
		},
		{
			name: "folded-clip",
			fm: "name: pdf\n" +
				"description: >\n" +
				"  Line one\n" +
				"  Line two",
			desc: "Line one Line two\n",
		},
		{
			name: "literal-clip",
			fm: "name: pdf\n" +
				"description: |\n" +
				"  Line one\n" +
				"  Line two",
			desc: "Line one\nLine two\n",
		},
		{
			name: "literal-strip",
			fm: "name: pdf\n" +
				"description: |-\n" +
				"  Line one\n" +
				"  Line two",
			desc: "Line one\nLine two",
		},
		{
			name: "folded-keep",
			// front matter 末尾的空行属于块（keep 保留）：内容尾换行 + 空行换行
			fm: "name: pdf\n" +
				"description: >+\n" +
				"  Line one\n" +
				"  Line two\n" +
				"\n",
			desc: "Line one Line two\n\n\n",
		},
		{
			name: "literal-keep",
			fm: "name: pdf\n" +
				"description: |+\n" +
				"  Line one\n" +
				"\n",
			desc: "Line one\n\n\n",
		},
		{
			name: "folded-blank-line-becomes-newline",
			// 单个空行 → 段落间恰好一个换行
			fm: "description: >\n" +
				"  para one\n" +
				"\n" +
				"  para two",
			desc: "para one\npara two\n",
		},
		{
			name: "folded-two-blank-lines-become-two-newlines",
			// 两个连续空行 → 两个换行
			fm: "description: >\n" +
				"  para one\n" +
				"\n" +
				"\n" +
				"  para two",
			desc: "para one\n\npara two\n",
		},
		{
			name: "folded-blank-then-more-indented-keeps-breaks",
			// 更缩进行保留原始换行（折叠规则不作用于额外缩进）
			fm: "description: >\n" +
				"  a\n" +
				"\n" +
				"    b\n" +
				"  c",
			desc: "a\n\n  b\nc\n",
		},
		{
			name: "literal-preserves-extra-indent",
			// 字面标量按块体最小缩进去掉公共前导空格，保留额外缩进
			fm: "description: |\n" +
				"  para one\n" +
				"    more indented\n" +
				"  para two",
			desc: "para one\n  more indented\npara two\n",
		},
		{
			name: "header-with-comment",
			fm: "description: > # trailing comment\n" +
				"  Line one\n" +
				"  Line two",
			desc: "Line one Line two\n",
		},
		{
			name: "explicit-indent-indicator",
			// |2：块缩进为 2；第三行有 4 个前导空格，去掉 2 个后保留 2 个
			fm: "description: |2\n" +
				"  base\n" +
				"    deeper",
			desc: "base\n  deeper\n",
		},
		{
			name: "leading-blank-line-preserved",
			// 头部行之后的前导空行原样保留（每行一个换行）
			fm: "description: >\n" +
				"\n" +
				"  first\n" +
				"  second",
			desc: "\nfirst second\n",
		},
		{
			name: "empty-block-clip-yields-empty-string",
			// 全空块 + clip：无内容行则无尾换行，结果为空串
			fm: "description: >\n" +
				"\n" +
				"\n" +
				"name: pdf",
			desc: "",
		},
		{
			name: "empty-block-keep-yields-newlines",
			// 全空块 + keep：保留全部空行换行
			fm: "description: >+\n" +
				"\n" +
				"\n" +
				"name: pdf",
			desc: "\n\n",
		},
		{
			name: "plain-value-not-mistaken-for-block",
			// 普通单行值以 > 开头但后面有残余文本，不是块标量
			fm: "description: >-not-a-block-scalar\n" +
				"name: pdf",
			desc: ">-not-a-block-scalar",
		},
		{
			name: "unicode-content",
			fm: "description: >-\n" +
				"  第一行\n" +
				"  第二行",
			desc: "第一行 第二行",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, body := parseFrontMatter("---\n" + tt.fm + "\n---\n# Body\n")
			if meta.Description != tt.desc {
				t.Errorf("Description = %q, want %q", meta.Description, tt.desc)
			}
			if body != "# Body\n" {
				t.Errorf("body = %q, want %q", body, "# Body\n")
			}
		})
	}
}

// TestParseFrontMatterBlockScalarNextKeyNotSwallowed 验证块标量结束后的
// 后续 key 不会被吞进块内容。
func TestParseFrontMatterBlockScalarNextKeyNotSwallowed(t *testing.T) {
	content := "---\n" +
		"name: pdf\n" +
		"description: >\n" +
		"  Line1\n" +
		"  Line2\n" +
		"compatibility: [pdf_read_tool, pdf_write_tool]\n" +
		"enabled: false\n" +
		"license: MIT\n" +
		"---\n" +
		"# 正文\n"

	meta, body := parseFrontMatter(content)

	if want := "Line1 Line2\n"; meta.Description != want {
		t.Errorf("Description = %q, want %q", meta.Description, want)
	}
	if meta.Name != "pdf" {
		t.Errorf("Name = %q, want %q", meta.Name, "pdf")
	}
	if len(meta.Compatibility) != 2 || meta.Compatibility[0] != "pdf_read_tool" || meta.Compatibility[1] != "pdf_write_tool" {
		t.Errorf("Compatibility = %v, want [pdf_read_tool pdf_write_tool]", meta.Compatibility)
	}
	if meta.Enabled == nil || *meta.Enabled != false {
		t.Errorf("Enabled = %v, want false", meta.Enabled)
	}
	if body != "# 正文\n" {
		t.Errorf("body = %q, want %q", body, "# 正文\n")
	}
}

// TestParseFrontMatterBlockScalarEndIndentation 验证块结束的缩进判定：
// 缩进不比块缩进深的非空行（无论是否顶格）都终止块，且该行交回外层处理。
func TestParseFrontMatterBlockScalarEndIndentation(t *testing.T) {
	// 更浅缩进的 key 行结束块
	meta, _ := parseFrontMatter("---\n" +
		"description: |-\n" +
		"  text\n" +
		" shallow: v\n" +
		"---\n")
	if want := "text"; meta.Description != want {
		t.Errorf("Description = %q, want %q", meta.Description, want)
	}

	// 更浅缩进但非顶格的无冒号行同样结束块（真实 YAML 会在该行报
	// "could not find expected ':'"）；外层照旧跳过该行
	meta, _ = parseFrontMatter("---\n" +
		"description: |-\n" +
		"  text\n" +
		" orphan\n" +
		"name: pdf\n" +
		"---\n")
	if want := "text"; meta.Description != want {
		t.Errorf("Description = %q, want %q", meta.Description, want)
	}
	if meta.Name != "pdf" {
		t.Errorf("Name = %q, want %q", meta.Name, "pdf")
	}
}

// TestParseFrontMatterSingleLineRegression 验证单行 key: value 行为不变。
func TestParseFrontMatterSingleLineRegression(t *testing.T) {
	content := "---\n" +
		"name: pdf\n" +
		"description: 处理 PDF 文件（提取文本、合并、拆分等）。\n" +
		"compatibility: pdf_read_tool, pdf_write_tool\n" +
		"enabled: true\n" +
		"---\n" +
		"# PDF 处理技能\n"

	meta, body := parseFrontMatter(content)

	if meta.Name != "pdf" {
		t.Errorf("Name = %q, want %q", meta.Name, "pdf")
	}
	if want := "处理 PDF 文件（提取文本、合并、拆分等）。"; meta.Description != want {
		t.Errorf("Description = %q, want %q", meta.Description, want)
	}
	if len(meta.Compatibility) != 2 || meta.Compatibility[0] != "pdf_read_tool" || meta.Compatibility[1] != "pdf_write_tool" {
		t.Errorf("Compatibility = %v, want [pdf_read_tool pdf_write_tool]", meta.Compatibility)
	}
	if meta.Enabled == nil || *meta.Enabled != true {
		t.Errorf("Enabled = %v, want true", meta.Enabled)
	}
	if body != "# PDF 处理技能\n" {
		t.Errorf("body = %q, want %q", body, "# PDF 处理技能\n")
	}
}

// TestParseFrontMatterMissingEndMarker 验证缺结尾 --- 时路径不变。
func TestParseFrontMatterMissingEndMarker(t *testing.T) {
	content := "---\nname: pdf\ndescription: no end marker"
	meta, body := parseFrontMatter(content)
	if meta.Name != "" || meta.Description != "" || meta.Compatibility != nil || meta.Enabled != nil {
		t.Errorf("meta = %+v, want zero value", meta)
	}
	if body != content {
		t.Errorf("body = %q, want original content", body)
	}
}

// TestParseFrontMatterNoFrontMatter 验证无 front matter 时路径不变。
func TestParseFrontMatterNoFrontMatter(t *testing.T) {
	content := "# 没有任何 front matter 的文档\n"
	meta, body := parseFrontMatter(content)
	if meta.Name != "" || meta.Description != "" || meta.Compatibility != nil || meta.Enabled != nil {
		t.Errorf("meta = %+v, want zero value", meta)
	}
	if body != content {
		t.Errorf("body = %q, want original content", body)
	}
}

// TestLoadSkillBlockScalarFullPipeline 用仿 skills/academy-guide/SKILL.md 的
// 临时 SKILL.md 走 LoadSkill 全链路，验证 Description 非空且不含 ">" 字面量。
func TestLoadSkillBlockScalarFullPipeline(t *testing.T) {
	dir := t.TempDir()
	skillMd := "---\n" +
		"name: academy-guide\n" +
		"description: >\n" +
		"  提供学院相关的使用指南与常见问题解答。\n" +
		"  当用户询问学院功能、课程安排或学分规则时使用。\n" +
		"license: MIT\n" +
		"---\n" +
		"# Academy Guide\n"

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMd), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	sk, err := NewLoader(dir, nil).LoadSkill(dir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}

	wantDesc := "提供学院相关的使用指南与常见问题解答。 当用户询问学院功能、课程安排或学分规则时使用。\n"
	if sk.Description != wantDesc {
		t.Errorf("Description = %q, want %q", sk.Description, wantDesc)
	}
	if strings.Contains(sk.Description, ">") {
		t.Errorf("Description 不应包含 \">\" 字面量: %q", sk.Description)
	}
	if sk.Name != "academy-guide" {
		t.Errorf("Name = %q, want %q", sk.Name, "academy-guide")
	}
	if !sk.Enabled {
		t.Errorf("Enabled = false, want true（enabled 缺省时默认启用）")
	}
	if sk.Content != "# Academy Guide" {
		t.Errorf("Content = %q, want %q", sk.Content, "# Academy Guide")
	}
}
