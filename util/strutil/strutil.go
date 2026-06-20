// Package strutil 提供字符串处理工具函数。
package strutil

import (
	"encoding/json"
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
func ExtractJSON(raw string, v any) error {
	raw = strings.TrimSpace(raw)

	// 去除 markdown 代码块标记
	raw = stripMarkdownCodeBlock(raw)

	// 直接解析
	if err := json.Unmarshal([]byte(raw), v); err == nil {
		return nil
	}

	// 尝试提取 JSON 对象 {...}
	if idx := strings.Index(raw, "{"); idx >= 0 {
		if end := strings.LastIndex(raw, "}"); end > idx {
			if err := json.Unmarshal([]byte(raw[idx:end+1]), v); err == nil {
				return nil
			}
		}
	}

	// 尝试提取 JSON 数组 [...]
	if idx := strings.Index(raw, "["); idx >= 0 {
		if end := strings.LastIndex(raw, "]"); end > idx {
			if err := json.Unmarshal([]byte(raw[idx:end+1]), v); err == nil {
				return nil
			}
		}
	}

	return json.Unmarshal([]byte(raw), v) // 返回原始错误
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
