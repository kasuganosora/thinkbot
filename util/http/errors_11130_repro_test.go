package http

import (
	"errors"
	"testing"
)

func TestMatchesQuotaText_DoesNotTreat11130As1113(t *testing.T) {
	body := `transient note code=11130 not a billing code`
	if matchesQuotaText(body) {
		t.Fatalf("matchesQuotaText matched 11130 via 1113 substring: %q", body)
	}
	jsonBody := `{"error":{"code":11130,"message":"DO_NOT_LEAK"}}`
	if matchesQuotaText(jsonBody) {
		t.Fatalf("matchesQuotaText matched JSON code 11130 via \"code\":1113 prefix: %q", jsonBody)
	}
	if hasNonRetryable429Code([]byte(jsonBody)) {
		t.Fatalf("hasNonRetryable429Code treated 11130 as 1113")
	}

	falsePositives := []string{
		`{"error":{"code":11130,"message":"DO_NOT_LEAK"}}`,
		`{"error":{"code": 11139,"message":"DO_NOT_LEAK"}}`,
		`{"error":{"code":"11130","message":"DO_NOT_LEAK"}}`,
		`{"error":{"code":13080,"message":"DO_NOT_LEAK"}}`,
		`{"error":{"code":"13080","message":"DO_NOT_LEAK"}}`,
		`HTTP 429: upstream transient (code=11130)`,
		`HTTP 429: upstream transient (code=01113)`,
		`HTTP 429: upstream transient (code=1113a)`,
		`HTTP 429: upstream transient (subcode=1113)`,
		`HTTP 429: upstream transient (code=13080)`,
	}
	for _, raw := range falsePositives {
		if matchesQuotaText(raw) {
			t.Fatalf("matchesQuotaText falsely matched %q", raw)
		}
		if IsQuotaExhausted(&StreamHTTPError{StatusCode: 429, Body: []byte(raw)}) {
			t.Fatalf("IsQuotaExhausted falsely matched %q", raw)
		}
		if IsQuotaExhaustedLoose(errors.New("HTTP 429 " + raw)) {
			t.Fatalf("IsQuotaExhaustedLoose falsely matched %q", raw)
		}
	}

	truePositives := []string{
		`{"error":{"code":1113,"message":"DO_NOT_LEAK"}}`,
		`{"error":{"code": 1113,"message":"DO_NOT_LEAK"}}`,
		`{"error":{"code":"1113","message":"DO_NOT_LEAK"}}`,
		`{"error":{"code":1308,"message":"DO_NOT_LEAK"}}`,
		`{"error":{"code":"1308","message":"DO_NOT_LEAK"}}`,
		`openai: quota_exceeded: provider account has insufficient balance (code=1113)`,
		`truncated {"code":1113, "msg":"DO_NOT_LEAK`,
		`truncated {"code": 1308}`,
	}
	for _, raw := range truePositives {
		if !matchesQuotaText(raw) {
			t.Fatalf("matchesQuotaText missed genuine quota code %q", raw)
		}
		if !IsQuotaExhausted(&StreamHTTPError{StatusCode: 429, Body: []byte(raw)}) {
			t.Fatalf("IsQuotaExhausted missed genuine quota code %q", raw)
		}
		if !IsQuotaExhaustedLoose(errors.New("HTTP 429 " + raw)) {
			t.Fatalf("IsQuotaExhaustedLoose missed genuine quota code %q", raw)
		}
	}
}

func TestHasNonRetryable429Code_Rejects11130JSON(t *testing.T) {
	jsonBody := []byte(`{"error":{"code":11130,"message":"DO_NOT_LEAK"}}`)
	if hasNonRetryable429Code(jsonBody) {
		t.Fatalf("hasNonRetryable429Code treated structured 11130 as 1113")
	}
	ok := []byte(`{"error":{"code":1113,"message":"DO_NOT_LEAK"}}`)
	if !hasNonRetryable429Code(ok) {
		t.Fatalf("hasNonRetryable429Code missed structured 1113")
	}
}
