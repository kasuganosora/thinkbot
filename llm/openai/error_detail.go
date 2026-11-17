package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const errorTokenMaxLen = 64

type errorDetailFieldViolation struct {
	Field string `json:"field"`
}

type errorDetailItem struct {
	Reason          string                      `json:"reason"`
	Field           string                      `json:"field"`
	Param           string                      `json:"param"`
	FieldViolations []errorDetailFieldViolation `json:"fieldViolations"`
	SnakeViolations []errorDetailFieldViolation `json:"field_violations"`
}

type structuredErrorFields struct {
	Type    string            `json:"type"`
	Status  json.RawMessage   `json:"status"`
	Code    json.RawMessage   `json:"code"`
	Param   json.RawMessage   `json:"param"`
	Details []errorDetailItem `json:"details"`
}

// parseProviderError reads structured error identifiers from standard OpenAI
// envelopes and known alternate provider shapes (such as top-level code with a
// string error field, or gRPC status/details). Free-text message, description,
// and string error fields are never copied into the result.
func parseProviderError(body []byte) (APIError, string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return APIError{}, ""
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil || len(arr) == 0 {
			return APIError{}, ""
		}
		trimmed = bytes.TrimSpace(arr[0])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return APIError{}, ""
	}

	var top struct {
		Type    string            `json:"type"`
		Status  json.RawMessage   `json:"status"`
		Code    json.RawMessage   `json:"code"`
		Param   json.RawMessage   `json:"param"`
		Details []errorDetailItem `json:"details"`
		Error   json.RawMessage   `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return APIError{}, ""
	}

	var nested structuredErrorFields
	if errRaw := bytes.TrimSpace(top.Error); len(errRaw) > 0 && errRaw[0] == '{' {
		_ = json.Unmarshal(errRaw, &nested)
	}

	errType := firstSafeToken(
		nested.Type,
		rawJSONStringOnly(nested.Status),
		top.Type,
		rawJSONStringOnly(top.Status),
	)

	nestedReason, nestedField := extractDetailIdentifiers(nested.Details)
	topReason, topField := extractDetailIdentifiers(top.Details)

	nestedCode := normalizeSafeCodeToken(rawJSONScalar(nested.Code))
	if nestedReason != "" && (nestedCode == "" || isHTTPStatusNumber(nestedCode)) {
		nestedCode = nestedReason
	}
	topCode := normalizeSafeCodeToken(rawJSONScalar(top.Code))
	if topReason != "" && (topCode == "" || isHTTPStatusNumber(topCode)) {
		topCode = topReason
	}
	code := nestedCode
	if code == "" {
		code = topCode
	}

	param := firstSafeToken(
		rawJSONScalar(nested.Param),
		nestedField,
		rawJSONScalar(top.Param),
		topField,
	)

	apiErr := APIError{
		Type:  errType,
		Code:  code,
		Param: param,
	}
	return apiErr, formatSafeErrorDetail(apiErr.Type, apiErr.Code, apiErr.Param)
}

func extractDetailIdentifiers(details []errorDetailItem) (reason, field string) {
	for _, d := range details {
		if reason == "" {
			reason = sanitizeErrorToken(d.Reason)
		}
		if field == "" {
			field = firstSafeToken(d.Param, d.Field)
		}
		if field == "" {
			for _, fv := range d.FieldViolations {
				if v := sanitizeErrorToken(fv.Field); v != "" {
					field = v
					break
				}
			}
		}
		if field == "" {
			for _, fv := range d.SnakeViolations {
				if v := sanitizeErrorToken(fv.Field); v != "" {
					field = v
					break
				}
			}
		}
	}
	return reason, field
}

func firstSafeToken(candidates ...string) string {
	for _, c := range candidates {
		if v := sanitizeErrorToken(c); v != "" {
			return v
		}
	}
	return ""
}

// normalizeSafeCodeToken accepts identifier-shaped codes and maps exact canonical
// gRPC status phrases (used in some xAI responses) to fixed safe tokens without
// copying any input text. Unknown free text returns "".
func normalizeSafeCodeToken(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "client specified an invalid argument":
		return "invalid_argument"
	case "some requested entity was not found":
		return "not_found"
	case "the caller does not have permission":
		return "permission_denied"
	case "request does not have valid authentication credentials":
		return "unauthenticated"
	case "resource has been exhausted":
		return "resource_exhausted"
	case "failed precondition":
		return "failed_precondition"
	case "the service is currently unavailable":
		return "unavailable"
	case "deadline expired before operation could complete":
		return "deadline_exceeded"
	case "internal errors", "internal error":
		return "internal"
	}
	return sanitizeErrorToken(raw)
}

func isHTTPStatusNumber(s string) bool {
	return len(s) == 3 && (s[0] == '4' || s[0] == '5') && s[1] >= '0' && s[1] <= '9' && s[2] >= '0' && s[2] <= '9'
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

func rawJSONStringOnly(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
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
