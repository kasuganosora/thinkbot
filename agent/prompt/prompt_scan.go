package prompt

import (
	"regexp"
	"strings"
)

// ============================================================================
// PromptScan — 上下文文件安全扫描
//
// 扫描注入 system prompt 的上下文文件（如 SOUL.md），检测：
//   - 经典提示注入（"ignore previous instructions" 等）
//   - 角色劫持（"you are now a..."）
//   - C2 / promptware 模式（brainworm、beacon 等）
//   - 数据渗出（curl/wget + secrets）
//   - 不可见 Unicode 字符（零宽空格、RTL 覆盖等）
//
// 检测结果用于日志告警或阻止加载，防止恶意 SOUL.md 内容劫持 bot。
// ============================================================================

// ScanMode 控制扫描匹配后的行为。
type ScanMode int

const (
	// ScanModeOff 不扫描。
	ScanModeOff ScanMode = iota
	// ScanModeWarn 扫描并记录告警日志，但不阻止加载。
	ScanModeWarn
	// ScanModeBlock 扫描并阻止加载（返回 error）。
	ScanModeBlock
)

// ScanFinding 表示一个扫描发现。
type ScanFinding struct {
	// PatternID 威胁模式的标识符。
	PatternID string
	// Snippet 匹配内容的片段（截取前 80 字符）。
	Snippet string
}

// threatPattern 定义一个威胁检测规则。
type threatPattern struct {
	regex     *regexp.Regexp
	patternID string
}

// contextThreatPatterns 是上下文文件的威胁检测规则集。
// 适用于用户编辑的文件（SOUL.md 等），检测明确的注入和攻击模式。
var contextThreatPatterns = []threatPattern{
	// ── 经典提示注入 ──
	{
		regexp.MustCompile(`(?i)ignore\s+(?:\w+\s+)*(?:previous|all|above|prior)\s+(?:\w+\s+)*instructions`),
		"prompt_injection",
	},
	{
		regexp.MustCompile(`(?i)system\s+prompt\s+override`),
		"sys_prompt_override",
	},
	{
		regexp.MustCompile(`(?i)disregard\s+(?:\w+\s+)*(?:your|all|any)\s+(?:\w+\s+)*(?:instructions|rules|guidelines)`),
		"disregard_rules",
	},
	{
		regexp.MustCompile(`(?i)<!--[^>]*(?:ignore|override|system|secret|hidden)[^>]*-->`),
		"html_comment_injection",
	},
	{
		regexp.MustCompile(`(?i)<\s*div\s+style\s*=\s*["'][\s\S]*?display\s*:\s*none`),
		"hidden_div",
	},

	// ── C2 / Promptware ──
	{
		regexp.MustCompile(`(?i)register\s+(?:as\s+)?a?\s*node`),
		"c2_node_registration",
	},
	{
		regexp.MustCompile(`(?i)(?:heartbeat|beacon|check[\s\-]?in)\s+(?:to|with)\s+`),
		"c2_heartbeat",
	},
	{
		regexp.MustCompile(`(?i)\b(?:praxis|cobalt\s*strike|sliver|havoc|mythic|brainworm)\b`),
		"known_c2_framework",
	},
	{
		regexp.MustCompile(`(?i)\bcommand\s+and\s+control\b`),
		"c2_explicit",
	},
	{
		regexp.MustCompile(`(?i)unset\s+\w*(?:CLAUDE|CODEX|HERMES|AGENT|OPENAI|ANTHROPIC)\w*`),
		"env_var_unset_agent",
	},

	// ── 数据渗出 ──
	{
		regexp.MustCompile(`(?i)curl\s+[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)`),
		"exfil_curl",
	},
	{
		regexp.MustCompile(`(?i)wget\s+[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)`),
		"exfil_wget",
	},
	{
		regexp.MustCompile(`(?i)cat\s+[^\n]*(?:\.env|credentials|\.netrc|\.pgpass|\.npmrc)`),
		"read_secrets",
	},
}

// invisibleUnicodeChars 是用于注入攻击的不可见 Unicode 字符集合。
var invisibleUnicodeChars = map[rune]string{
	'\u200b': "U+200B", // zero-width space
	'\u200c': "U+200C", // zero-width non-joiner
	'\u200d': "U+200D", // zero-width joiner
	'\u2060': "U+2060", // word joiner
	'\u2062': "U+2062", // invisible times
	'\u2063': "U+2063", // invisible separator
	'\u2064': "U+2064", // invisible plus
	'\ufeff': "U+FEFF", // zero-width no-break space (BOM)
	'\u202a': "U+202A", // left-to-right embedding
	'\u202b': "U+202B", // right-to-left embedding
	'\u202c': "U+202C", // pop directional formatting
	'\u202d': "U+202D", // left-to-right override
	'\u202e': "U+202E", // right-to-left override
	'\u2066': "U+2066", // left-to-right isolate
	'\u2067': "U+2067", // right-to-left isolate
	'\u2068': "U+2068", // first strong isolate
	'\u2069': "U+2069", // pop directional isolate
}

// ScanForThreats 扫描内容中的威胁模式。
// 返回所有匹配的 ScanFinding 列表（空列表表示安全）。
func ScanForThreats(content string) []ScanFinding {
	if content == "" {
		return nil
	}

	var findings []ScanFinding

	// 检测不可见 Unicode 字符
	for _, ch := range content {
		if name, ok := invisibleUnicodeChars[ch]; ok {
			findings = append(findings, ScanFinding{
				PatternID: "invisible_unicode_" + name,
				Snippet:   string(ch),
			})
		}
	}

	// 检测威胁模式
	for _, tp := range contextThreatPatterns {
		loc := tp.regex.FindStringIndex(content)
		if loc == nil {
			continue
		}
		snippet := content[loc[0]:loc[1]]
		if len(snippet) > 80 {
			snippet = snippet[:80]
		}
		findings = append(findings, ScanFinding{
			PatternID: tp.patternID,
			Snippet:   snippet,
		})
	}

	return findings
}

// HasThreats 是 ScanForThreats 的便捷封装，返回是否有任何威胁。
func HasThreats(content string) bool {
	return len(ScanForThreats(content)) > 0
}

// truncateContent 将内容截断到 maxBytes 以内。
// 保留头部 70% 和尾部 20%，中间用省略标记替代。
// maxBytes <= 0 表示不截断。
func truncateContent(content string, maxBytes int) string {
	if maxBytes <= 0 || len(content) <= maxBytes {
		return content
	}

	headSize := int(float64(maxBytes) * 0.7)
	tailSize := int(float64(maxBytes) * 0.2)

	head := content[:headSize]
	tail := content[len(content)-tailSize:]

	omitted := len(content) - headSize - tailSize
	return head + "\n\n[... truncated " + itoa(omitted) + " bytes ...]\n\n" + tail
}

// itoa 简单的 int → string 转换，避免引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// FindingsSummary 将扫描发现格式化为可读的摘要字符串。
func FindingsSummary(findings []ScanFinding) string {
	if len(findings) == 0 {
		return ""
	}
	seen := make(map[string]bool)
	var parts []string
	for _, f := range findings {
		if seen[f.PatternID] {
			continue
		}
		seen[f.PatternID] = true
		snippet := strings.TrimSpace(f.Snippet)
		if len(snippet) > 60 {
			snippet = snippet[:60] + "..."
		}
		parts = append(parts, f.PatternID+"("+snippet+")")
	}
	return strings.Join(parts, ", ")
}
