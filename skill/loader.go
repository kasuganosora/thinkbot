package skill

import (
	"bufio"
	"fmt"
	"github.com/kasuganosora/thinkbot/util/errs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ============================================================================
// Loader — 从文件系统加载 Skill
//
// 目录结构（遵循 Anthropic Skills 规范）：
//
//	skills/
//	  pdf/                  # Skill 目录（目录名任意，以 SKILL.md 的 name 为准）
//	    SKILL.md            # 核心文件（YAML front matter + Markdown 正文）
//	    scripts/            # 可选：可执行脚本
//	    references/         # 可选：参考文档
//	    assets/             # 可选：资产文件
//	  xlsx/
//	    SKILL.md
//	    references/
//	    ...
//
// SKILL.md 格式：
//
//	---
//	name: pdf
//	description: 处理 PDF 文件（提取文本、合并、拆分等）。当用户提到 PDF、需要提取 PDF 内容时使用。
//	compatibility: [pdf_read_tool]
//	enabled: true
//	---
//
//	# PDF 处理技能
//
//	## 指令
//	当用户请求处理 PDF 时...
//
// ============================================================================

// Loader 从文件系统目录加载 Skill。
type Loader struct {
	// Dir 是包含各 Skill 子目录的根目录（如 "skills/"）。
	Dir string

	// Logger 日志记录器（可选）。
	Logger Logger
}

// NewLoader 创建 Loader。
func NewLoader(dir string, logger Logger) *Loader {
	if logger == nil {
		logger = noopLogger{}
	}
	return &Loader{
		Dir:    dir,
		Logger: logger,
	}
}

// LoadAll 扫描 Dir 下所有子目录，加载包含 SKILL.md 的 Skill，
// 并通过 registerFn 回调注册到 SkillManager。
// 返回加载成功的 Skill 数量。
func (l *Loader) LoadAll(registerFn func(*Skill)) (int, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 目录不存在时静默跳过
		}
		return 0, errs.Wrapf(err, "skill loader: read dir %q", l.Dir)
	}

	// 按目录名排序
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Strings(dirs)

	loaded := 0
	for _, dir := range dirs {
		skillDir := filepath.Join(l.Dir, dir)
		skill, err := l.LoadSkill(skillDir)
		if err != nil {
			l.Logger.Warnw("skip skill",
				"dir", skillDir,
				"error", err)
			continue
		}
		if registerFn != nil {
			registerFn(skill)
		}
		loaded++
	}
	return loaded, nil
}

// LoadSkill 从指定目录加载单个 Skill（读取 SKILL.md）。
// 同时扫描附加资源目录（scripts/、references/、assets/）。
// 返回加载后的 Skill 对象，由调用方决定何时注册到 SkillManager。
func (l *Loader) LoadSkill(skillDir string) (*Skill, error) {
	skillMdPath := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(skillMdPath)
	if err != nil {
		return nil, errs.Wrap(err, "read SKILL.md")
	}

	content := string(data)
	meta, body := parseFrontMatter(content)

	skill := &Skill{
		Name:          meta.Name,
		Description:   meta.Description,
		Compatibility: meta.Compatibility,
		Content:       strings.TrimSpace(body),
		Enabled:       meta.Enabled == nil || *meta.Enabled, // nil 或 true → 默认启用
		Source:        "fs",
		Dir:           skillDir,
	}

	// 校验必填字段
	if skill.Name == "" {
		return nil, fmt.Errorf("SKILL.md: missing required field `name`")
	}
	if skill.Description == "" {
		return nil, fmt.Errorf("SKILL.md: missing required field `description`")
	}

	// 扫描附加资源
	skill.Resources = scanResources(skillDir)

	l.Logger.Debugw("skill loaded from file",
		"name", skill.Name,
		"dir", skillDir,
		"hasContent", skill.Content != "",
		"hasScripts", len(skill.Resources.Scripts) > 0,
	)

	return skill, nil
}

// LoadAndRegister 从目录加载所有 Skill 并注册到 SkillManager。
// 是 LoadAll + Register 的便捷方法。
func (l *Loader) LoadAndRegister(mgr *SkillManager) (int, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, errs.Wrapf(err, "skill loader: read dir %q", l.Dir)
	}

	// 按目录名排序，确保加载顺序确定
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Strings(dirs)

	loaded := 0
	for _, dir := range dirs {
		skillDir := filepath.Join(l.Dir, dir)
		skill, err := l.LoadSkill(skillDir)
		if err != nil {
			l.Logger.Warnw("skip skill",
				"dir", skillDir,
				"error", err)
			continue
		}
		mgr.Register(skill)
		loaded++
	}
	return loaded, nil
}

// ============================================================================
// Front Matter 解析
// ============================================================================

// parseFrontMatter 解析 SKILL.md 中的 YAML front matter。
// 格式：--- 开头，--- 结尾，中间是 key: value 行。
// 返回解析后的 SkillMeta 和剩余的 Markdown 正文。
func parseFrontMatter(content string) (SkillMeta, string) {
	var meta SkillMeta

	// 检查是否有 front matter
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		// 无 front matter，整个内容作为正文
		return meta, content
	}

	// 找到开头 --- 后的内容
	var rest string
	if strings.HasPrefix(content, "---\n") {
		rest = content[4:]
	} else {
		rest = content[5:] // "---\r\n" = 5 字符
	}

	// 找到结尾的 ---
	end := strings.Index(rest, "\n---")
	if end < 0 {
		// 无结尾 ---，整个内容作为正文
		return meta, content
	}

	frontMatter := rest[:end]
	body := rest[end+4:] // 跳过 "\n---"
	// 如果结尾是 "---\n" 后还有内容，body 已正确截取
	// 处理 "\r\n" 情况
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimPrefix(body, "\r\n")

	// 逐行解析 key: value
	scanner := bufio.NewScanner(strings.NewReader(frontMatter))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parseFrontMatterLine(line, &meta)
	}

	return meta, body
}

// parseFrontMatterLine 解析一行 front matter。
func parseFrontMatterLine(line string, meta *SkillMeta) {
	// 简单解析 "key: value" 格式
	idx := strings.Index(line, ":")
	if idx < 0 {
		return
	}

	key := strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+1:])

	switch key {
	case "name":
		meta.Name = val
	case "description":
		meta.Description = val
	case "compatibility":
		// 支持 YAML 列表格式：["a", "b"] 或 a, b
		meta.Compatibility = parseYAMLList(val)
	case "enabled":
		b, err := parseBool(val)
		meta.Enabled = &b
		if err != nil {
			// 解析失败，保持 nil（默认启用）
			meta.Enabled = nil
		}
	}
}

// parseYAMLList 解析简单的 YAML 列表值。
// 支持格式：["a", "b"] 或 a, b 或 [a, b]
func parseYAMLList(val string) []string {
	val = strings.TrimSpace(val)
	if val == "" {
		return nil
	}

	// 尝试 ["a", "b"] 格式
	if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
		inner := strings.TrimSpace(val[1 : len(val)-1])
		inner = strings.TrimSuffix(inner, ",")
		var result []string
		// 简单按逗号分割，去掉引号和空格
		parts := strings.Split(inner, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			p = strings.Trim(p, "\"'")
			if p != "" {
				result = append(result, p)
			}
		}
		return result
	}

	// 尝试 "a, b" 格式
	if strings.Contains(val, ",") {
		parts := strings.Split(val, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		return result
	}

	// 单个值
	return []string{val}
}

// parseBool 解析布尔值字符串。
func parseBool(s string) (bool, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "true", "1", "yes", "on", "enable", "enabled":
		return true, nil
	case "false", "0", "no", "off", "disable", "disabled":
		return false, nil
	}
	return false, fmt.Errorf("invalid bool: %q", s)
}

// ============================================================================
// 附加资源扫描
// ============================================================================

// scanResources 扫描 Skill 目录下的附加资源。
func scanResources(skillDir string) SkillResources {
	var r SkillResources

	// scripts/
	r.Scripts = scanDir(filepath.Join(skillDir, "scripts"))

	// references/
	r.References = scanDir(filepath.Join(skillDir, "references"))

	// assets/
	r.Assets = scanDir(filepath.Join(skillDir, "assets"))

	return r
}

// scanDir 扫描目录下的所有文件（递归），返回完整路径列表。
func scanDir(dir string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过错误
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files
}
