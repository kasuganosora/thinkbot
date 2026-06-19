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
// 尝试顺序：
//  1. 直接解析整个字符串
//  2. 提取第一个 '{' 到最后一个 '}' 之间的子串再解析
//
// 解析成功返回 nil error；均失败则返回原始 json.Unmarshal 错误。
// 适用于 LLM 返回的 JSON（可能被 markdown 代码块或说明文字包裹）。
func ExtractJSON(raw string, v any) error {
	raw = strings.TrimSpace(raw)

	// 直接解析
	if err := json.Unmarshal([]byte(raw), v); err == nil {
		return nil
	}

	// 提取 JSON 块
	if idx := strings.Index(raw, "{"); idx >= 0 {
		if end := strings.LastIndex(raw, "}"); end > idx {
			return json.Unmarshal([]byte(raw[idx:end+1]), v)
		}
	}

	return json.Unmarshal([]byte(raw), v) // 返回原始错误
}
