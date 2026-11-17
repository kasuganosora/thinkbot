package openai

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
	ulog "github.com/kasuganosora/thinkbot/util/log"
)

func TestSanitizeErrorToken_DropsIdentifierShapedSecrets(t *testing.T) {
	const secret = "sk-test-LEAK-9999-secret"
	if got := sanitizeErrorToken(secret); got != "" {
		t.Fatalf("sanitizeErrorToken(%q) = %q, want empty (identifier-shaped secret must not enter SafeDetail)", secret, got)
	}
	detail := formatSafeErrorDetail("invalid_request_error", secret, "temperature")
	if strings.Contains(detail, secret) || strings.Contains(detail, "LEAK") {
		t.Fatalf("SafeDetail construction accepted secret identifier: %q", detail)
	}
	if detail != "param=temperature type=invalid_request_error" {
		t.Fatalf("formatSafeErrorDetail = %q, want %q", detail, "param=temperature type=invalid_request_error")
	}

	const registeredVal = "synthetic_registered_vault_secret"
	ulog.RegisterSecret(registeredVal)
	defer ulog.UnregisterSecret(registeredVal)

	secrets := []string{
		secret,
		"sk-proj-SYNTH1234567890secret",
		"sk-synthetic-lowercase-only",
		"xai-SYNTH1234567890secret",
		"ghp_SYNTH1234567890abcdef",
		"AIzaSySyntheticKey1234567890",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig",
		"tok-9999-LEAK-synthetic",
		"aB3dE6fG9hJ2kL5mN8pQ1rS4",
		"0123456789abcdef0123456789abcdef",
		registeredVal,
	}
	for _, candidate := range secrets {
		if got := sanitizeErrorToken(candidate); got != "" {
			t.Fatalf("sanitizeErrorToken(%q) = %q, want empty", candidate, got)
		}
	}

	body := []byte(`{"error":{"type":"` + secret + `","code":"` + secret + `","param":"temperature","message":"DO_NOT_LEAK ` + secret + `"}}`)
	err := classifyStatusBody(http.StatusBadRequest, body, nil)
	le, ok := llm.AsLLMError(err)
	if !ok {
		t.Fatalf("expected *llm.LLMError, got %T", err)
	}
	for _, surface := range []string{le.SafeDetail, le.Message, le.Error(), llm.SafeErrorText(le), llm.UserFailureText(le)} {
		if strings.Contains(surface, secret) || strings.Contains(surface, "LEAK") || strings.Contains(surface, "DO_NOT_LEAK") {
			t.Fatalf("error surface %q leaked secret material", surface)
		}
	}
	if le.SafeDetail != "param=temperature" {
		t.Fatalf("SafeDetail = %q, want %q", le.SafeDetail, "param=temperature")
	}
}

func TestSanitizeErrorToken_PreservesLegitimateIdentifiers(t *testing.T) {
	allowed := []string{
		"invalid_request_error",
		"invalid_api_key",
		"unsupported_parameter",
		"max_completion_tokens",
		"temperature",
		"top_p",
		"b64_json",
		"UNSUPPORTED_PARAMETER",
		"INVALID_ARGUMENT",
		"maxTokens",
		"messages.0.role",
		"1113",
		"1302",
		"1305",
		"400",
		"rate-limit-exceeded",
	}
	for _, tok := range allowed {
		if got := sanitizeErrorToken(tok); got != tok {
			t.Fatalf("sanitizeErrorToken(%q) = %q, want %q", tok, got, tok)
		}
	}
}
