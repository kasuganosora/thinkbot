package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const errorTokenMaxLen = 64

// parseProviderError reads the structured error fields and returns a safe
// classifier. The free-text message is never copied into the result.
func parseProviderError(body []byte) (APIError, string) {
	var env struct {
		Error struct {
			Type  string          `json:"type"`
			Code  json.RawMessage `json:"code"`
			Param json.RawMessage `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return APIError{}, ""
	}
	apiErr := APIError{
		Type:  strings.TrimSpace(env.Error.Type),
		Code:  rawJSONScalar(env.Error.Code),
		Param: rawJSONScalar(env.Error.Param),
	}
	return apiErr, formatSafeErrorDetail(apiErr.Type, apiErr.Code, apiErr.Param)
}

func formatSafeErrorDetail(errType, code, param string) string {
	var parts []string
	if v := sanitizeErrorToken(code); v != "" {
		parts = append(parts, "code="+v)
	}
	if v := sanitizeErrorToken(param); v != "" {
		parts = append(parts, "param="+v)
	}
	if v := sanitizeErrorToken(errType); v != "" {
		parts = append(parts, "type="+v)
	}
	return strings.Join(parts, " ")
}

// sanitizeErrorToken keeps short identifier-shaped values. Anything else,
// including free text that could echo a prompt or a key, is dropped.
func sanitizeErrorToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > errorTokenMaxLen {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return ""
		}
	}
	return s
}

func rawJSONScalar(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	if raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9') {
		return string(raw)
	}
	return ""
}
