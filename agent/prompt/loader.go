package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ============================================================================
// FileLoader — 从 Markdown 文件加载 Section
// ============================================================================

// FileLoader 从指定目录加载 Markdown 模板文件，自动注册为 Section。
//
// 文件命名约定：
//   - 文件名格式：{order}_{name}.md
//     例如：000_identity.md, 100_rules.md, 200_context.md
//   - order: 3 位数字，决定 Section 的排序权重
//   - name: Section 名称标识（下划线分隔）
//
// 文件内容约定：
//   - 第一行如果以 "# " 开头，作为 Section 的显示名称（可选）
//   - 支持 {{.VarName}} 格式的变量占位符
//   - 支持 YAML front matter（可选）用于声明 Variables 和 Conditional
//
// 简化模式（推荐）：
//   - 文件内容即 Section.Content，直接渲染
//   - 变量通过 {{.VarName}} 自动发现，来源默认为 SourceEnvelopeKV
//   - 不使用 front matter 时，所有变量都是可选的、来源为 envelope KV
type FileLoader struct {
	dir      string
	registry *Registry
}

// NewFileLoader 创建文件加载器。
func NewFileLoader(dir string, registry *Registry) *FileLoader {
	return &FileLoader{
		dir:      dir,
		registry: registry,
	}
}

// LoadAll 扫描目录下所有 .md 文件并注册为 Section。
// 返回加载的 Section 数量和可能的错误。
func (l *FileLoader) LoadAll() (int, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 目录不存在时静默跳过
		}
		return 0, fmt.Errorf("prompt file_loader: read dir %q: %w", l.dir, err)
	}

	var mdFiles []os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".md") {
			mdFiles = append(mdFiles, entry)
		}
	}

	// 按文件名排序（确保 order 确定性）
	sort.Slice(mdFiles, func(i, j int) bool {
		return mdFiles[i].Name() < mdFiles[j].Name()
	})

	loaded := 0
	for _, f := range mdFiles {
		section, err := l.parseFile(f.Name())
		if err != nil {
			return loaded, fmt.Errorf("prompt file_loader: parse %q: %w", f.Name(), err)
		}
		l.registry.Register(*section)
		loaded++
	}

	return loaded, nil
}

// LoadFile 加载单个文件并注册。
func (l *FileLoader) LoadFile(filename string) error {
	section, err := l.parseFile(filename)
	if err != nil {
		return err
	}
	l.registry.Register(*section)
	return nil
}

// parseFile 解析一个 markdown 文件为 Section。
func (l *FileLoader) parseFile(filename string) (*Section, error) {
	path := filepath.Join(l.dir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	content := string(data)

	// 解析文件名 → order + name
	order, name, err := parseFileName(filename)
	if err != nil {
		return nil, err
	}

	// 解析 front matter（如果有）
	content, meta := parseFrontMatter(content)

	// 自动发现 {{.VarName}} 变量
	variables := discoverVariables(content, meta)

	// 处理条件开关
	enabled := true
	if v, ok := meta["enabled"]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			enabled = b
		}
	}

	return &Section{
		Name:      name,
		Order:     order,
		Content:   strings.TrimSpace(content),
		Enabled:   enabled,
		Variables: variables,
	}, nil
}

// parseFileName 解析文件名中的 order 和 name。
// 格式：{order}_{name}.md 或 {name}.md（无 order 默认 500）。
func parseFileName(filename string) (int, string, error) {
	// 移除扩展名
	base := strings.TrimSuffix(filename, ".md")

	// 尝试分割 order_name
	parts := strings.SplitN(base, "_", 2)
	if len(parts) == 2 {
		order, err := strconv.Atoi(parts[0])
		if err == nil {
			// 有效的 order_name 格式
			return order, parts[1], nil
		}
	}

	// 无 order 前缀，整个 base 作为 name，默认 order=500
	return 500, base, nil
}

// parseFrontMatter 解析简单的 YAML-like front matter。
// 支持 "---" 分隔的 key: value 格式。
func parseFrontMatter(content string) (string, map[string]string) {
	meta := make(map[string]string)

	if !strings.HasPrefix(content, "---\n") {
		return content, meta
	}

	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return content, meta
	}

	frontMatter := content[4 : 4+end]
	remaining := content[4+end+4:] // 跳过 "\n---"

	// 解析 key: value
	for _, line := range strings.Split(frontMatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			meta[key] = value
		}
	}

	return remaining, meta
}

// discoverVariables 从模板内容中自动发现 {{.VarName}} 占位符。
// 默认来源为 SourceEnvelopeKV，key 为变量名本身。
func discoverVariables(content string, meta map[string]string) []Variable {
	var variables []Variable
	seen := make(map[string]bool)

	// 查找所有 {{.VarName}} 模式
	for {
		start := strings.Index(content, "{{.")
		if start < 0 {
			break
		}
		end := strings.Index(content[start:], "}}")
		if end < 0 {
			break
		}

		varName := content[start+3 : start+end]
		content = content[start+end+2:]

		if seen[varName] {
			continue
		}
		seen[varName] = true

		// 检查 meta 中是否有变量来源声明
		// 格式：var_{name}_source: static|env|func
		// 格式：var_{name}_key: envelope_key_name
		// 格式：var_{name}_default: default_value
		source := SourceEnvelopeKV
		envelopeKey := varName
		defaultVal := ""
		required := false

		if s, ok := meta["var_"+varName+"_source"]; ok {
			switch s {
			case "static":
				source = SourceStatic
			case "env", "envelope":
				source = SourceEnvelopeKV
			}
		}
		if k, ok := meta["var_"+varName+"_key"]; ok {
			envelopeKey = k
		}
		if d, ok := meta["var_"+varName+"_default"]; ok {
			defaultVal = d
		}
		if r, ok := meta["var_"+varName+"_required"]; ok {
			required = r == "true"
		}

		v := Variable{
			Name:        varName,
			Source:      source,
			EnvelopeKey: envelopeKey,
			Default:     defaultVal,
			Required:    required,
		}

		if source == SourceStatic {
			if sv, ok := meta["var_"+varName+"_value"]; ok {
				v.StaticValue = sv
			}
		}

		variables = append(variables, v)
	}

	return variables
}
