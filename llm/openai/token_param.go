package openai

import (
	"net/url"
	"strings"
)

// Token parameter styles for Chat Completions.
// auto uses max_completion_tokens when the base URL host is the official API
// host (parsed hostname equality with the default base URL), and max_tokens
// for every other host. Responses requests do not use this setting.
const (
	TokenParamAuto          TokenParamStyle = "auto"
	TokenParamMaxTokens     TokenParamStyle = "max_tokens"
	TokenParamMaxCompletion TokenParamStyle = "max_completion_tokens"
)

// TokenParamStyle selects which Chat Completions field carries the output cap.
type TokenParamStyle string

// NormalizeTokenParamStyle maps a config value to auto, max_tokens, or
// max_completion_tokens. Empty and unrecognized values are auto.
func NormalizeTokenParamStyle(style string) TokenParamStyle {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case string(TokenParamMaxTokens):
		return TokenParamMaxTokens
	case string(TokenParamMaxCompletion):
		return TokenParamMaxCompletion
	default:
		return TokenParamAuto
	}
}

func chatTokenField(style TokenParamStyle, baseURL string) string {
	switch NormalizeTokenParamStyle(string(style)) {
	case TokenParamMaxTokens:
		return "max_tokens"
	case TokenParamMaxCompletion:
		return "max_completion_tokens"
	default:
		if officialChatHost(baseURL) {
			return "max_completion_tokens"
		}
		return "max_tokens"
	}
}

func officialChatHost(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host != "" && host == officialAPIHostname()
}

func officialAPIHostname() string {
	u, err := url.Parse(DefaultBaseURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// applyChatTokenLimit sets exactly one output-cap field, or neither when the
// limit is unset or not positive.
func applyChatTokenLimit(req *ChatCompletionRequest, limit *int, style TokenParamStyle, baseURL string) {
	if req == nil {
		return
	}
	req.MaxTokens = nil
	req.MaxCompletionTokens = nil
	if limit == nil || *limit <= 0 {
		return
	}
	n := *limit
	if chatTokenField(style, baseURL) == "max_completion_tokens" {
		req.MaxCompletionTokens = &n
		return
	}
	req.MaxTokens = &n
}
