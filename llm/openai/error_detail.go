package openai

import "github.com/kasuganosora/thinkbot/llm"

// parseProviderError reads structured error identifiers from standard OpenAI
// envelopes and known alternate provider shapes via the shared safe parser.
func parseProviderError(body []byte) (APIError, string) {
	fields, detail := llm.ParseProviderError(body)
	return APIError{
		Type:  fields.Type,
		Code:  fields.Code,
		Param: fields.Param,
	}, detail
}

func formatSafeErrorDetail(errType, code, param string) string {
	return llm.FormatSafeErrorDetail(errType, code, param)
}

func sanitizeErrorToken(s string) string {
	return llm.SanitizeErrorToken(s)
}
