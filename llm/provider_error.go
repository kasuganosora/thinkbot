package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	httputil "github.com/kasuganosora/thinkbot/util/http"
	ulog "github.com/kasuganosora/thinkbot/util/log"
)

const (
	errorTokenMaxLen = 64

	// MsgProviderCredentialsRejected is the fixed safe message for HTTP 401 / invalid_api_key.
	MsgProviderCredentialsRejected = "provider rejected the credentials"
	// MsgProviderAccessDenied is the fixed safe message for HTTP 403.
	MsgProviderAccessDenied = "provider denied access"
)

// ProviderErrorFields holds sanitized structured identifiers extracted from an
// upstream error payload. Free-text message/description fields are never copied.
type ProviderErrorFields struct {
	Type  string
	Code  string
	Param string
}

type providerErrorFieldViolation struct {
	Field string `json:"field"`
}

type providerErrorDetailItem struct {
	Reason          string                        `json:"reason"`
	Field           string                        `json:"field"`
	Param           string                        `json:"param"`
	FieldViolations []providerErrorFieldViolation `json:"fieldViolations"`
	SnakeViolations []providerErrorFieldViolation `json:"field_violations"`
}

type providerStructuredErrorFields struct {
	Type    string                    `json:"type"`
	Status  json.RawMessage           `json:"status"`
	Code    json.RawMessage           `json:"code"`
	Param   json.RawMessage           `json:"param"`
	Details []providerErrorDetailItem `json:"details"`
}

// ParseProviderError extracts sanitized structured identifiers from standard
// OpenAI-compatible envelopes and known alternate provider shapes (such as xAI
// top-level code + string error or gRPC status/details). Free-text message,
// description, and string error fields are never copied into the result.
func ParseProviderError(body []byte) (ProviderErrorFields, string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ProviderErrorFields{}, ""
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil || len(arr) == 0 {
			return ProviderErrorFields{}, ""
		}
		trimmed = bytes.TrimSpace(arr[0])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ProviderErrorFields{}, ""
	}

	var top struct {
		Type    string                    `json:"type"`
		Status  json.RawMessage           `json:"status"`
		Code    json.RawMessage           `json:"code"`
		Param   json.RawMessage           `json:"param"`
		Details []providerErrorDetailItem `json:"details"`
		Error   json.RawMessage           `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return ProviderErrorFields{}, ""
	}

	var nested providerStructuredErrorFields
	if errRaw := bytes.TrimSpace(top.Error); len(errRaw) > 0 && errRaw[0] == '{' {
		_ = json.Unmarshal(errRaw, &nested)
	}

	errType := firstSafeErrorToken(
		nested.Type,
		rawJSONStringScalar(nested.Status),
		top.Type,
		rawJSONStringScalar(top.Status),
	)

	nestedReason, nestedField := extractProviderDetailIdentifiers(nested.Details)
	topReason, topField := extractProviderDetailIdentifiers(top.Details)

	nestedCode := normalizeSafeCodeToken(rawJSONScalarToken(nested.Code))
	if nestedReason != "" && (nestedCode == "" || isHTTPStatusNumber(nestedCode)) {
		nestedCode = nestedReason
	}
	topCode := normalizeSafeCodeToken(rawJSONScalarToken(top.Code))
	if topReason != "" && (topCode == "" || isHTTPStatusNumber(topCode)) {
		topCode = topReason
	}
	code := nestedCode
	if code == "" {
		code = topCode
	}

	param := firstSafeErrorToken(
		rawJSONScalarToken(nested.Param),
		nestedField,
		rawJSONScalarToken(top.Param),
		topField,
	)

	fields := ProviderErrorFields{
		Type:  errType,
		Code:  code,
		Param: param,
	}
	return fields, FormatSafeErrorDetail(fields.Type, fields.Code, fields.Param)
}

func extractProviderDetailIdentifiers(details []providerErrorDetailItem) (reason, field string) {
	for _, d := range details {
		if reason == "" {
			reason = SanitizeErrorToken(d.Reason)
		}
		if field == "" {
			field = firstSafeErrorToken(d.Param, d.Field)
		}
		if field == "" {
			for _, fv := range d.FieldViolations {
				if v := SanitizeErrorToken(fv.Field); v != "" {
					field = v
					break
				}
			}
		}
		if field == "" {
			for _, fv := range d.SnakeViolations {
				if v := SanitizeErrorToken(fv.Field); v != "" {
					field = v
					break
				}
			}
		}
	}
	return reason, field
}

func firstSafeErrorToken(candidates ...string) string {
	for _, c := range candidates {
		if v := SanitizeErrorToken(c); v != "" {
			return v
		}
	}
	return ""
}

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
	return SanitizeErrorToken(raw)
}

func isHTTPStatusNumber(s string) bool {
	return len(s) == 3 && (s[0] == '4' || s[0] == '5') && s[1] >= '0' && s[1] <= '9' && s[2] >= '0' && s[2] <= '9'
}

// FormatSafeErrorDetail formats sanitized upstream type/code/param tokens for SafeDetail.
func FormatSafeErrorDetail(errType, code, param string) string {
	var parts []string
	if v := SanitizeErrorToken(code); v != "" {
		parts = append(parts, "code="+v)
	}
	if v := SanitizeErrorToken(param); v != "" {
		parts = append(parts, "param="+v)
	}
	if v := SanitizeErrorToken(errType); v != "" {
		parts = append(parts, "type="+v)
	}
	return strings.Join(parts, " ")
}

var credentialPrefixes = []string{
	"sk-", "sk_",
	"pk-", "pk_",
	"rk-", "rk_",
	"ak-", "ak_",
	"xai-", "xai_",
	"gsk_",
	"ant-",
	"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_",
	"glpat-",
	"xoxb-", "xoxp-", "xoxa-", "xoxr-", "xoxs-",
	"hf_",
	"r8_",
	"whsec_",
}

func hasCredentialPrefix(s string) bool {
	if strings.HasPrefix(s, "AIza") || strings.HasPrefix(s, "eyJ") {
		return true
	}
	low := strings.ToLower(s)
	for _, p := range credentialPrefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

func isSignedIntegerCode(s string) bool {
	if len(s) == 0 || len(s) > 12 {
		return false
	}
	start := 0
	if s[0] == '-' {
		if len(s) == 1 {
			return false
		}
		start = 1
	}
	for i := start; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SanitizeErrorToken keeps short identifier-shaped error classifiers while
// dropping free text and identifier-shaped secrets/tokens.
func SanitizeErrorToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > errorTokenMaxLen {
		return ""
	}
	if ulog.RedactSecrets(s) != s {
		return ""
	}
	if isSignedIntegerCode(s) {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if first == '.' || first == '_' || first == '-' || last == '.' || last == '_' || last == '-' {
		return ""
	}
	if strings.Contains(s, "..") || hasCredentialPrefix(s) {
		return ""
	}

	var hasLower, hasUpper, hasDigit, hasHyphen bool
	maxAlnumRun := 0
	curAlnumRun := 0
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
			curAlnumRun++
		case r >= 'A' && r <= 'Z':
			hasUpper = true
			curAlnumRun++
		case r >= '0' && r <= '9':
			hasDigit = true
			curAlnumRun++
		case r == '-':
			hasHyphen = true
			curAlnumRun = 0
		case r == '_' || r == '.':
			curAlnumRun = 0
		default:
			return ""
		}
		if curAlnumRun > maxAlnumRun {
			maxAlnumRun = curAlnumRun
		}
	}

	if hasHyphen && (hasUpper || hasDigit) {
		return ""
	}
	if hasLower && hasUpper && hasDigit {
		return ""
	}
	if hasDigit && maxAlnumRun >= 20 {
		return ""
	}
	return s
}

func rawJSONStringScalar(raw json.RawMessage) string {
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

func rawJSONScalarToken(raw json.RawMessage) string {
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

// ClassifyProviderErrorReason maps sanitized error type, business code, and HTTP
// status into a structured ErrorReason.
func ClassifyProviderErrorReason(errType, code string, statusCode int) ErrorReason {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_api_key", "unauthenticated":
		return ErrorReasonAuthentication
	case "permission_denied":
		return ErrorReasonAuthentication
	case "model_not_found", "not_found":
		return ErrorReasonNoRoute
	case "content_filter", "content_policy_violation", "1301":
		return ErrorReasonContentPolicy
	case "insufficient_quota", "1113":
		return ErrorReasonQuotaExceeded
	case "1302", "resource_exhausted":
		if statusCode == http.StatusTooManyRequests || statusCode == 0 {
			return ErrorReasonRateLimit
		}
	case "1305", "unavailable", "internal":
		return ErrorReasonProviderInternal
	}
	switch errType {
	case "invalid_request_error", "INVALID_ARGUMENT":
		return ErrorReasonInvalidRequest
	case "authentication_error", "UNAUTHENTICATED", "PERMISSION_DENIED":
		return ErrorReasonAuthentication
	case "rate_limit_exceeded", "tokens":
		return ErrorReasonRateLimit
	case "insufficient_quota", "billing":
		return ErrorReasonQuotaExceeded
	case "content_policy_violation":
		return ErrorReasonContentPolicy
	case "server_error", "api_error", "INTERNAL", "UNAVAILABLE":
		return ErrorReasonProviderInternal
	}
	return ClassifyProviderHTTPStatus(statusCode)
}

// ClassifyProviderHTTPStatus maps bare HTTP status codes to ErrorReason.
func ClassifyProviderHTTPStatus(statusCode int) ErrorReason {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return ErrorReasonRateLimit
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return ErrorReasonAuthentication
	case statusCode == http.StatusPaymentRequired:
		return ErrorReasonQuotaExceeded
	case statusCode >= 500:
		return ErrorReasonProviderInternal
	case statusCode >= 400:
		return ErrorReasonInvalidRequest
	default:
		return ErrorReasonTransport
	}
}

// SafeProviderMessage returns a bounded, static diagnostic message without any
// upstream free text, URLs, prompts, or secrets.
func SafeProviderMessage(reason ErrorReason, statusCode int, code string) string {
	trimmedCode := strings.TrimSpace(code)
	switch {
	case strings.EqualFold(trimmedCode, "invalid_api_key") || statusCode == http.StatusUnauthorized:
		return MsgProviderCredentialsRejected
	case statusCode == http.StatusForbidden:
		return MsgProviderAccessDenied
	case reason == ErrorReasonAuthentication:
		return MsgProviderCredentialsRejected
	case reason == ErrorReasonContentPolicy:
		return "provider rejected the content"
	case trimmedCode == "1113":
		return "provider account has insufficient balance"
	case trimmedCode == "1305":
		return "provider model is overloaded"
	case reason == ErrorReasonRateLimit:
		return "provider rate limited the request"
	case reason == ErrorReasonQuotaExceeded:
		return "provider quota exceeded"
	case reason == ErrorReasonNoRoute:
		return "provider model was not found"
	default:
		return fmt.Sprintf("provider request failed with HTTP %d", statusCode)
	}
}

// ParseRetryAfterHeader parses integer Retry-After seconds from response headers.
func ParseRetryAfterHeader(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	if raStr := strings.TrimSpace(headers.Get("Retry-After")); raStr != "" {
		if secs, err := strconv.Atoi(raStr); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}

// ClassifyProviderHTTPFailure classifies an HTTP error response in memory without
// copying raw body text or free-text error messages into the returned LLMError.
func ClassifyProviderHTTPFailure(provider string, statusCode int, body []byte, headers http.Header) *LLMError {
	fields, detail := ParseProviderError(body)
	reason := ClassifyProviderErrorReason(fields.Type, fields.Code, statusCode)
	var opts []LLMErrorOpt
	if delay := ParseRetryAfterHeader(headers); delay > 0 {
		opts = append(opts, WithRetryAfter(delay))
	}
	err := NewLLMError(reason, provider, SafeProviderMessage(reason, statusCode, fields.Code), opts...)
	err.SafeDetail = detail
	return err
}

// ParseProviderAPIError converts a synchronous HTTP response or transport failure
// into a sanitized *LLMError.
func ParseProviderAPIError(provider string, resp *httputil.Response, httpErr error) error {
	if resp != nil && resp.StatusCode >= 400 {
		return ClassifyProviderHTTPFailure(provider, resp.StatusCode, resp.Body, resp.Headers)
	}
	if httpErr != nil {
		var already *LLMError
		if errors.As(httpErr, &already) && already != nil {
			return already
		}
		return NewLLMError(ErrorReasonTransport, provider, "provider connection failed")
	}
	return nil
}

// ClassifyProviderStreamError converts a streaming error into a sanitized *LLMError
// without retaining raw upstream response bodies or URLs.
func ClassifyProviderStreamError(provider string, err error) error {
	if err == nil {
		return nil
	}
	var already *LLMError
	if errors.As(err, &already) && already != nil {
		return already
	}
	var streamErr *httputil.StreamHTTPError
	if errors.As(err, &streamErr) && streamErr != nil {
		return ClassifyProviderHTTPFailure(provider, streamErr.StatusCode, streamErr.Body, streamErr.Headers)
	}
	return NewLLMError(ErrorReasonTransport, provider, "provider connection failed")
}
