// Package strutil 提供字符串处理工具函数。
package strutil

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Truncate 将字符串截断到最多 maxRunes 个 rune（按 Unicode 码点计数），
// 超出时追加 "..."。用于日志和错误消息中的安全截断。
func Truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}

// ExtractJSON 从可能包含额外文本的字符串中提取并解析 JSON。
//
// 处理步骤：
//  1. 去除 markdown 代码块标记（```json ... ```）
//  2. 直接解析整个字符串
//  3. 提取第一个 '{' 到最后一个 '}' 之间的子串再解析（JSON 对象）
//  4. 提取第一个 '[' 到最后一个 ']' 之间的子串再解析（JSON 数组）
//
// 解析成功返回 nil error；均失败则返回原始 json.Unmarshal 错误。
// 适用于 LLM 返回的 JSON（可能被 markdown 代码块或说明文字包裹）。
// ExtractJSON 从可能包含额外文本的字符串中提取并解析 JSON。
//
// 处理步骤（任一成功即返回）：
//  1. 去除 markdown 代码块标记（```json ... ```）
//  2. 直接解析整个字符串
//  3. 用括号配平扫描定位最外层 JSON 对象 / 数组并解析（比“第一个 {
//     到最后一个 }”更稳健，能正确处理被说明文字包裹、以及字符串值内
//     出现括号的情况）
//  4. 容错清理（字符串内裸控制字符转义、尾随逗号删除）后重试上述解析
//
// 解析成功返回 nil error；均失败则返回原始 json.Unmarshal 错误。
// 适用于 LLM 返回的 JSON（常被 markdown 代码块、说明文字包裹，或在字符串
// 值里直接输出换行导致非法 JSON——日志中表现为 analyzer/dreaming 解析失败）。
func ExtractJSON(raw string, v any) error {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "\ufeff") // 去 BOM

	// 去除 markdown 代码块标记
	raw = stripMarkdownCodeBlock(raw)

	// 1) 直接解析（绝大多数情况）
	if err := json.Unmarshal([]byte(raw), v); err == nil {
		return nil
	}

	// 2) 括号配平扫描定位最外层对象 / 数组
	if obj, ok := extractBalanced(raw, '{', '}'); ok {
		if err := json.Unmarshal([]byte(obj), v); err == nil {
			return nil
		}
	}
	if arr, ok := extractBalanced(raw, '[', ']'); ok {
		if err := json.Unmarshal([]byte(arr), v); err == nil {
			return nil
		}
	}

	// 3) 容错：清理常见 LLM 退化输出后再试
	sanitized := sanitizeJSON(raw)
	if err := json.Unmarshal([]byte(sanitized), v); err == nil {
		return nil
	}
	if obj, ok := extractBalanced(sanitized, '{', '}'); ok {
		if err := json.Unmarshal([]byte(obj), v); err == nil {
			return nil
		}
	}
	if arr, ok := extractBalanced(sanitized, '[', ']'); ok {
		if err := json.Unmarshal([]byte(arr), v); err == nil {
			return nil
		}
	}

	return json.Unmarshal([]byte(raw), v) // 返回原始错误
}

// extractBalanced 从 s 中第一个 open 括号起，按括号深度扫描回到配平的 close，
// 返回该最外层片段。字符串字面量内的括号会被忽略，避免误判（例如值里含 "{"）。
func extractBalanced(s string, open, close byte) (string, bool) {
	start := strings.IndexByte(s, open)
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// sanitizeJSON 修复 LLM 常见的非法 JSON：字符串字面量内的裸控制字符转义，
// 以及对象 / 数组尾随逗号。对已经是合法 JSON 的输入是无操作，不改变语义。
func sanitizeJSON(s string) string {
	s = sanitizeJSONStrings(s)
	// 删除尾随逗号： ",}" / ",]"
	re := regexp.MustCompile(`,(\s*[}\]])`)
	s = re.ReplaceAllString(s, "$1")
	return s
}

// sanitizeJSONStrings 将字符串字面量内部的裸控制字符（换行、回车、制表等）
// 转义为合法转义序列；其他不可见控制字符直接丢弃。非字符串区域不受影响。
func sanitizeJSONStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				b.WriteByte(c)
				esc = false
				continue
			}
			if c == '\\' {
				b.WriteByte(c)
				esc = true
				continue
			}
			if c == '"' {
				b.WriteByte(c)
				inStr = false
				continue
			}
			switch c {
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					continue // 其他控制字符丢弃
				}
				b.WriteByte(c)
			}
			continue
		}
		if c == '"' {
			inStr = true
		}
		b.WriteByte(c)
	}
	return b.String()
}

// stripMarkdownCodeBlock 去除 ```json ... ``` 或 ``` ... ``` 包裹。
func stripMarkdownCodeBlock(raw string) string {
	s := raw
	// 去除前缀的代码块起始标记
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSpace(s)
	// 去除后缀的代码块结束标记
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// MapKeys 返回 map 的键列表（无序）。
// 用于审计日志等场景记录修改了哪些字段。
func MapKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
