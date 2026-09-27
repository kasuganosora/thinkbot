package skill

import (
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
//	delegation: preferred
//	---
//
//	# PDF 处理技能
//
//	## 指令
//	当用户请求处理 PDF 时...
//
//	delegation 可选：取值 "preferred" 显式声明为重型技能（建议委托子代理执行）；
//	未声明时正文超过 3000 字节自动判为重型，见 Skill.IsHeavy。
// ============================================================================

// Loader 从文件系统目录加载 Skill。
type Loader struct {
	// Dir 是包含各 Skill 子目录的根目录（如 "skills/"）。
	Dir string

	// Source 写入 Skill.Source（bundled / managed / fs）。空则 "fs"。
	Source string

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

	src := l.Source
	if src == "" {
		src = "fs"
	}
	skill, err := newSkillFromContent(string(data), src, skillDir)
	if err != nil {
		return nil, err
	}

	// 扫描附加资源
	skill.Resources = scanResources(skillDir)

	l.Logger.Debugw("skill loaded from file",
		"name", skill.Name,
		"dir", skillDir,
		"hasContent", skill.Content != "",
		"hasScripts", len(skill.Resources.Scripts) > 0,
		"level", skill.Level(), // 分级标注：light / heavy（delegation 声明 + 体积阈值）
	)

	return skill, nil
}

// newSkillFromContent 由 SKILL.md 原文构建 Skill（不含资源扫描），校验必填字段。
// 文件系统 Loader 与沙箱工作空间来源（WorkspaceSource）共用。
func newSkillFromContent(content, source, dir string) (*Skill, error) {
	meta, body := parseFrontMatter(content)
	skill := &Skill{
		Name:          meta.Name,
		Description:   meta.Description,
		Compatibility: meta.Compatibility,
		Content:       strings.TrimSpace(body),
		Enabled:       meta.Enabled == nil || *meta.Enabled, // nil 或 true → 默认启用
		Source:        source,
		Dir:           dir,
		// 委托执行声明透传；分级（heavy/light）由 Skill.IsHeavy 按声明 + 体积动态判定
		Delegation: meta.Delegation,
	}
	if skill.Name == "" {
		return nil, fmt.Errorf("SKILL.md: missing required field `name`")
	}
	if skill.Description == "" {
		return nil, fmt.Errorf("SKILL.md: missing required field `description`")
	}
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

	// 逐行解析（缩进感知）：支持单行 key: value，也支持块标量（>、|- 等续行写法）
	// 详见下方 parseFrontMatterLines。
	parseFrontMatterLines(frontMatter, &meta)

	return meta, body
}

// parseFrontMatterLines 逐行解析 front matter（缩进感知）。
// 在旧的「单行 key: value」解析之上，支持 YAML 块标量（>、|- 等续行写法），
// 使 description 等字段可以写成多行文本。
// 行为基线：真实 YAML 解析器（libyaml 语义）在相同输入上的解析结果。
func parseFrontMatterLines(fm string, meta *SkillMeta) {
	// front matter 的最后一个换行属于 "\n---" 分隔符，被 parseFrontMatter
	// 切掉了；这里补上一个虚拟换行，使块标量扫描器看到的字节流与真实
	// YAML 文档完全一致（尾随空行、chomping 均以此为基准）。
	// strings.Split 恒在末尾产生一个空元素，恰好充当 EOF 哨兵行。
	lines := strings.Split(fm+"\n", "\n")

	sc := &blockScalarScanner{lines: lines}
	for i := 0; i < len(lines)-1; i++ { // 最后一项是 EOF 哨兵，不参与
		line := lines[i]
		// 与旧的逐行解析一致：不含冒号的行（含空行）直接跳过
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])

		if ok, literal, chomping, increment := parseBlockScalarHeader(val); ok {
			// 块标量：从头部行的下一行开始收集；
			// 块结束行（下一个 key 或更浅的行）交回外层继续处理
			val = sc.scanBlockScalar(i+1, literal, chomping, increment)
			i = sc.idx - 1 // for 的 i++ 会再前进一行
		}
		applyFrontMatterField(key, val, meta)
	}
}

// applyFrontMatterField 把单个 key/value 赋给 SkillMeta。
// （原 parseFrontMatterLine 的赋值逻辑，key/value 均已 TrimSpace。）
func applyFrontMatterField(key, val string, meta *SkillMeta) {
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
	case "delegation":
		// 委托执行声明（可选字段）：值原样 TrimSpace 后保存（如 "preferred"），
		// 分级判定（Skill.IsHeavy）在读取侧完成，解析层不猜语义。
		// 注意不要实现为 trim 后回写 quoted 字符串的变体，保持与 name/description 一致的单行语义。
		meta.Delegation = strings.TrimSpace(strings.Trim(val, `"'`))
	}
}

// ============================================================================
// 块标量（> 与 | 系列）
// ============================================================================

// parseBlockScalarHeader 识别块标量头部（key: 之后、已 TrimSpace 的值）。
// 形如 >、>-、|+、|2、>2- 等，允许后跟空白与 # 注释。
// 返回是否为合法块标量及其参数；不合法时调用方按普通单行值处理。
func parseBlockScalarHeader(val string) (ok, literal bool, chomping, increment int) {
	if val == "" || (val[0] != '>' && val[0] != '|') {
		return false, false, 0, 0
	}
	literal = val[0] == '|'
	rest := val[1:]

	// chomping 指示符（+/-）与缩进指示符（1-9）各最多一个，顺序任意；
	// 缩进指示符不允许为 0（libyaml 限制），遇 '0' 按非法头部处理。
	seenChomp, seenDigit := false, false
indicators:
	for len(rest) > 0 {
		c := rest[0]
		switch {
		case (c == '+' || c == '-') && !seenChomp:
			seenChomp = true
			if c == '+' {
				chomping = 1
			} else {
				chomping = -1
			}
		case c >= '1' && c <= '9' && !seenDigit:
			seenDigit = true
			increment = int(c - '0')
		default:
			break indicators
		}
		rest = rest[1:]
	}
	// 吃掉空白与注释性尾随文本
	rest = strings.TrimLeft(rest, " \t")
	if strings.HasPrefix(rest, "#") {
		rest = ""
	}
	if rest != "" {
		// 头部之后还有残余文本，不是合法块标量
		return false, false, 0, 0
	}
	return true, literal, chomping, increment
}

// blockScalarScanner 是 libyaml yaml_parser_scan_block_scalar 的逐行移植。
type blockScalarScanner struct {
	lines []string // 全部行；最后一项恒为 ""（EOF 哨兵）
	idx   int      // 当前行下标；等于 len(lines)-1 表示已到流结尾
	col   int      // 当前行内的列（0 起，等价于 libyaml 的 mark.column）
}

// scanBreaks 对应 yaml_parser_scan_block_scalar_breaks：反复吃掉「缩进空格 +
// 换行」（即空行），直到遇到非空行或流结尾。每吃掉一个空行就向 breaks 追加
// 一个 "\n"（read_line 在真实实现中把 \r\n 归一化为 \n）。
// 返回本次调用中到达过的最大列（自动缩进判定用）以及是否遇到制表符错误。
//
// 两处与 libyaml 一致的细节：
//   - indent == 0 表示自动缩进模式，该行全部前导空格都会被吃掉；
//   - indent > 0 时空格只吃到列 == indent 为止，更深处的空格留给内容，
//     因此「缩进超过块缩进的纯空格行」会被视作内容行而非空行。
func (sc *blockScalarScanner) scanBreaks(indent int, breaks *string) (maxIndent int, tabErr bool) {
	for {
		line := sc.lines[sc.idx]
		// EOF 哨兵：流结尾既非空行也非内容，直接停住
		if sc.idx == len(sc.lines)-1 {
			return maxIndent, false
		}
		// 吃缩进空格（is_space 只认空格，不认制表符）
		for (indent == 0 || sc.col < indent) && sc.col < len(line) && line[sc.col] == ' ' {
			sc.col++
		}
		if sc.col > maxIndent {
			maxIndent = sc.col
		}
		// 缩进位置出现制表符：libyaml 视为扫描错误
		if (indent == 0 || sc.col < indent) && sc.col < len(line) && line[sc.col] == '\t' {
			return maxIndent, true
		}
		// 行尾（含结尾 "\r"）→ 空行：吃掉换行，进入下一行
		if sc.col >= len(line) || line[sc.col] == '\r' {
			*breaks += "\n"
			sc.idx++
			sc.col = 0
			continue
		}
		// 非空行：停住，不消费该行任何字符
		return maxIndent, false
	}
}

// scanBlockScalar 对应 yaml_parser_scan_block_scalar 的主体（头部指示符已由
// parseBlockScalarHeader 解析完毕）。front matter 的 key 全部顶格，即父节点
// （根级 block mapping）缩进恒为 0，因此：
//   - 显式缩进指示 n → 块缩进 = n；
//   - 自动缩进 = max(空行们与首个内容行中到达的最大列, 1)。
//
// 返回标量值。块结束时 sc.idx 指向第一个不属于本块的行（下一个 key、
// 更浅的非空行或 EOF 哨兵），由调用方接管。
//
// 核心语义（与 libyaml 逐分支对应）：
//   - 头部行的换行被 skip_line 吃掉，不计入前导换行；其后到首个内容行
//     之间的空行原样保留（折叠标量保留为换行，每空行一个 "\n"）；
//   - 字面标量（|）：每个内容行原样保留并以换行结尾，不做任何折叠；
//   - 折叠标量（>）：上一内容行与当前行均不以空白开头且中间无空行时，
//     折成单个空格；中间有 N 个连续空行时折成 N 个换行（前导换行被丢弃，
//     只保留空行的换行）；任一行以空白开头（额外缩进）则保留原始换行；
//   - 尾部 chomping：strip（-）去掉全部尾换行，clip（默认）保留恰好一个
//     尾换行（即最后内容行的换行，尾随空行被丢弃），keep（+）再保留全部
//     尾随空行。全空块：strip 为空串，clip 为空串，keep 为各空行的换行。
func (sc *blockScalarScanner) scanBlockScalar(startIdx int, literal bool, chomping, increment int) string {
	sc.idx, sc.col = startIdx, 0

	indent := 0
	if increment > 0 {
		indent = increment
	}

	var s, leadingBreak, trailingBreaks string
	maxIndent, tabErr := sc.scanBreaks(indent, &trailingBreaks)
	if tabErr {
		// 真实 YAML 在此直接报错；这里降级为返回已收集内容，不中断整体解析
		return s
	}
	if indent == 0 {
		indent = maxIndent
		if indent < 1 {
			indent = 1
		}
	}

	leadingBlank := false
	for {
		line := sc.lines[sc.idx]
		// 流结尾，或当前列不等于块缩进（更浅的行）→ 块结束
		if sc.idx == len(sc.lines)-1 || sc.col != indent {
			break
		}

		// 当前行是否以空白开头（libyaml 的 is_blank：空格或制表符），
		// 即「额外缩进」标记，同时充当上一行的 trailing_blank
		trailingBlank := line[sc.col] == ' ' || line[sc.col] == '\t'

		// 折叠判定：折叠标量 + 上一行不以空白开头 + 当前行不以空白开头 +
		// 存在前导换行 → 中间无空行折成空格、有 N 个空行折成 N 个换行
		// （前导换行本身被丢弃）；其余情况保留前导换行原样
		if !literal && !leadingBlank && !trailingBlank && leadingBreak != "" {
			if trailingBreaks == "" {
				s += " "
			}
		} else {
			s += leadingBreak
		}
		leadingBreak = ""
		s += trailingBreaks
		trailingBreaks = ""

		leadingBlank = trailingBlank

		// 消费行内容（结尾 "\r" 属于换行的一部分，不计入内容）
		end := len(line)
		if r := strings.IndexByte(line[sc.col:], '\r'); r >= 0 {
			end = sc.col + r
		}
		s += line[sc.col:end]
		leadingBreak = "\n" // read_line：消费换行（进循环体的行必为真实行）
		sc.idx++
		sc.col = 0

		// 吃掉后续缩进与空行
		if _, tabErr = sc.scanBreaks(indent, &trailingBreaks); tabErr {
			return s
		}
	}

	// 尾部 chomping
	if chomping != -1 {
		s += leadingBreak
	}
	if chomping == 1 {
		s += trailingBreaks
	}
	return s
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
