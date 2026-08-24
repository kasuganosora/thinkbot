package http

import (
	"net/url"
	"regexp"
	"strings"
)

// redactQueryNames 是需在日志/错误中抹除的敏感查询参数名。
var redactQueryNames = map[string]bool{
	"i":             true, // Misskey token（?i=<token>）
	"token":         true,
	"access_token":  true,
	"secret":        true,
	"api_key":       true,
	"apikey":        true,
	"password":      true,
	"authorization": true,
}

// botTokenPathRE 匹配 Telegram 风格的 /bot<token> 路径段（token 含字母数字:_-）。
var botTokenPathRE = regexp.MustCompile(`(/bot)([A-Za-z0-9:_-]+)`)

// SanitizeURL 返回脱敏后的 URL 字符串，用于日志与错误信息，避免 token 经
// 查询参数（?i= / ?token= / ?access_token= 等）或 /bot<token> 路径段泄露。
//
// 仅做展示层脱敏：实际请求仍使用原始 URL，不影响功能。
func SanitizeURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// 解析失败：兜底做启发式替换，至少抹掉明显的 token 片段。
		return redactFallback(raw)
	}
	if u.RawQuery != "" {
		q, _ := url.ParseQuery(u.RawQuery)
		changed := false
		for name := range q {
			if redactQueryNames[strings.ToLower(name)] {
				q.Set(name, "***")
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	out := u.String()
	out = botTokenPathRE.ReplaceAllString(out, `${1}***`)
	return out
}

// redactFallback 在 URL 无法解析时，用正则兜底抹除敏感片段。
func redactFallback(raw string) string {
	out := botTokenPathRE.ReplaceAllString(raw, `${1}***`)
	// 形如 ?i=xxxx 或 &token=xxxx 的片段
	out = regexp.MustCompile(`([?&](i|token|access_token|secret|api_key|apikey|password)=)[^&#]+`).
		ReplaceAllString(out, `${1}***`)
	return out
}
