package searchproviders

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	utilhttp "github.com/kasuganosora/thinkbot/util/http"
)

const (
	defaultUserAgent = "ThinkbotBot/1.0"
	ddgUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
	maxResultsCap    = 20
	maxBodySize      = 1 << 20
)

// Search 使用指定提供方执行真实网页搜索。
// 空结果、鉴权失败、超时一律返回 error，绝不伪造 duckduckgo.com/?q= 命中。
func Search(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if count <= 0 {
		count = 5
	}
	if count > maxResultsCap {
		count = maxResultsCap
	}

	kind := strings.ToLower(strings.TrimSpace(p.Type))
	var (
		results []Result
		err     error
	)
	switch kind {
	case TypeBrave:
		results, err = callBrave(ctx, p, query, count)
	case TypeBing:
		results, err = callBing(ctx, p, query, count)
	case TypeGoogle:
		results, err = callGoogle(ctx, p, query, count)
	case TypeTavily:
		results, err = callTavily(ctx, p, query, count)
	case TypeSogou:
		results, err = callSogou(ctx, p, query, count)
	case TypeSerper:
		results, err = callSerper(ctx, p, query, count)
	case TypeSearXNG:
		results, err = callSearXNG(ctx, p, query, count)
	case TypeJina:
		results, err = callJina(ctx, p, query, count)
	case TypeExa:
		results, err = callExa(ctx, p, query, count)
	case TypeBocha:
		results, err = callBocha(ctx, p, query, count)
	case TypeDuckDuckGo:
		results, err = callDuckDuckGo(ctx, p, query, count)
	case TypeYandex:
		results, err = callYandex(ctx, p, query, count)
	default:
		if kind == "" {
			return nil, fmt.Errorf("search provider type is empty")
		}
		return nil, fmt.Errorf("unsupported search provider %q", p.Type)
	}
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no search results for %q", query)
	}
	return results, nil
}

func newSearchClient(timeout time.Duration, userAgent string) *utilhttp.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	return utilhttp.New(
		utilhttp.WithTimeout(timeout),
		utilhttp.WithHeader("User-Agent", userAgent),
		utilhttp.WithMaxBodySize(maxBodySize),
	)
}

func (p Provider) httpClient() *utilhttp.Client {
	return newSearchClient(p.timeoutOrDefault(), defaultUserAgent)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func requireAPIKey(p Provider, name string) (string, error) {
	key := strings.TrimSpace(p.APIKey)
	if key == "" {
		return "", fmt.Errorf("%s API key is required", name)
	}
	return key, nil
}

func httpStatusError(status int, body []byte) error {
	detail := extractJSONErrorMessage(body)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if len(detail) > 200 {
		detail = detail[:200] + "..."
	}
	if detail != "" {
		return fmt.Errorf("search request failed (HTTP %d): %s", status, detail)
	}
	return fmt.Errorf("search request failed (HTTP %d)", status)
}

func extractJSONErrorMessage(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	for _, key := range []string{"error", "message", "detail", "error_message"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			return val
		case map[string]any:
			if msg, ok := val["message"].(string); ok {
				return msg
			}
		}
	}
	return ""
}

func do(ctx context.Context, req *utilhttp.Request) ([]byte, error) {
	resp, err := req.SetContext(ctx).Do()
	if err != nil {
		if resp != nil && !resp.IsSuccess() {
			return nil, httpStatusError(resp.StatusCode, resp.Body)
		}
		return nil, err
	}
	return resp.Body, nil
}

func capCount(count, max int) int {
	if count > max {
		return max
	}
	return count
}
