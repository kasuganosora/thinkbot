package searchproviders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

// Attempt 记录一次失败或空结果的尝试，供 LLM 查看回退路径。
type Attempt struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

// EnabledResult 是按 UI 优先级串行尝试后的成功结果。
type EnabledResult struct {
	Results   []Result
	Provider  Provider
	Fallback  bool
	Attempted []Attempt
}

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

// SearchEnabled 按 providers.json 文件顺序串行尝试所有已启用提供方。
// 失败或空结果立即回退到下一个；401/403 熔断 10 分钟，429 熔断 Retry-After（默认 60 秒）。
// 超时/5xx/网络错误立即回退，不加长熔断。不并行，以免打爆配额。
func SearchEnabled(ctx context.Context, store *Store, query string, count int) (*EnabledResult, error) {
	if store == nil {
		return nil, fmt.Errorf("search provider store is required")
	}
	list, err := store.EnabledList()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no search provider is enabled; enable one in Settings → Search Providers")
	}

	var attempted []Attempt
	for _, p := range list {
		if reason, skipped := circuitSkipReason(p.ID); skipped {
			attempted = append(attempted, Attempt{ID: p.ID, Type: p.Type, Name: p.Name, Error: reason})
			continue
		}
		results, err := Search(ctx, p, query, count)
		if err == nil && len(results) > 0 {
			out := &EnabledResult{
				Results:  results,
				Provider: p,
				Fallback: len(attempted) > 0,
			}
			if len(attempted) > 0 {
				out.Attempted = attempted
			}
			return out, nil
		}
		errMsg := "no search results"
		if err != nil {
			errMsg = err.Error()
			tripCircuitFromError(p.ID, err)
		}
		attempted = append(attempted, Attempt{ID: p.ID, Type: p.Type, Name: p.Name, Error: errMsg})
	}

	// 已配置的启用项全部失败后，再无密钥试一次 ddgs 风格 HTML DDG。
	// 若启用列表里已经有 duckduckgo，不再重复。零启用项不会走到这里。
	if !enabledListHasType(list, TypeDuckDuckGo) {
		p := lastResortDDGProvider()
		results, err := Search(ctx, p, query, count)
		if err == nil && len(results) > 0 {
			return &EnabledResult{
				Results:   results,
				Provider:  p,
				Fallback:  true,
				Attempted: attempted,
			}, nil
		}
		errMsg := "no search results"
		if err != nil {
			errMsg = err.Error()
		}
		attempted = append(attempted, Attempt{ID: p.ID, Type: p.Type, Name: p.Name, Error: errMsg})
	}

	parts := make([]string, 0, len(attempted))
	for _, a := range attempted {
		name := a.Name
		if name == "" {
			name = a.ID
		}
		parts = append(parts, fmt.Sprintf("%s/%s: %s", a.Type, name, a.Error))
	}
	return nil, fmt.Errorf("all search providers failed: %s", strings.Join(parts, "; "))
}

func lastResortDDGProvider() Provider {
	return Provider{
		Type:    TypeDuckDuckGo,
		Name:    "DuckDuckGo (ddgs html)",
		Timeout: 15,
	}
}

func enabledListHasType(list []Provider, typ string) bool {
	for _, p := range list {
		if strings.EqualFold(strings.TrimSpace(p.Type), typ) {
			return true
		}
	}
	return false
}

func tripCircuitFromError(id string, err error) {
	auth, rate, isRate := classifySearchError(err)
	if auth {
		tripAuthCircuit(id)
		return
	}
	if isRate {
		tripRateCircuit(id, rate)
	}
}

// httpSearchError 保留 Search() 原有错误文案，同时带上状态码和 Retry-After 供熔断分类。
type httpSearchError struct {
	status     int
	retryAfter time.Duration
	msg        string
}

func (e *httpSearchError) Error() string { return e.msg }

func classifySearchError(err error) (auth bool, rate time.Duration, isRate bool) {
	if err == nil {
		return false, 0, false
	}
	var he *httpSearchError
	if errors.As(err, &he) {
		switch he.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true, 0, false
		case http.StatusTooManyRequests:
			d := he.retryAfter
			if d <= 0 {
				d = rateCooldown
			}
			return false, d, true
		}
		// Concrete HTTP status that is not 401/403/429: do not guess from body text.
		return false, 0, false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "http 401") || strings.Contains(msg, "http 403") {
		return true, 0, false
	}
	if strings.Contains(msg, "http 429") {
		return false, rateCooldown, true
	}
	return false, 0, false
}

func newSearchClient(timeout time.Duration, userAgent string) *utilhttp.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	hc := &http.Client{
		Timeout:       timeout,
		CheckRedirect: refuseSearchRedirect,
	}
	return utilhttp.New(
		utilhttp.WithHTTPClient(hc),
		utilhttp.WithHeader("User-Agent", userAgent),
		utilhttp.WithMaxBodySize(maxBodySize),
	)
}

// refuseSearchRedirect blocks following redirects into Instant Answer or y.js ads.
// A custom CheckRedirect replaces net/http's default 10-hop limit, so we keep it.
func refuseSearchRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if req == nil || req.URL == nil {
		return nil
	}
	host := strings.ToLower(req.URL.Hostname())
	if host == InstantAnswerHost || strings.HasSuffix(host, "."+InstantAnswerHost) {
		return fmt.Errorf("refusing Instant Answer host %s", InstantAnswerHost)
	}
	if isDDGAdURL(req.URL.String()) || strings.Contains(strings.ToLower(req.URL.Path), "y.js") {
		return fmt.Errorf("refusing duckduckgo y.js redirect")
	}
	return nil
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
	return newHTTPSearchError(status, body, 0)
}

func newHTTPSearchError(status int, body []byte, retryAfter time.Duration) error {
	detail := extractJSONErrorMessage(body)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if len(detail) > 200 {
		detail = detail[:200] + "..."
	}
	var msg string
	if detail != "" {
		msg = fmt.Sprintf("search request failed (HTTP %d): %s", status, detail)
	} else {
		msg = fmt.Sprintf("search request failed (HTTP %d)", status)
	}
	return &httpSearchError{status: status, retryAfter: retryAfter, msg: msg}
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
			return nil, newHTTPSearchError(resp.StatusCode, resp.Body, parseRetryAfterHeader(resp.Headers))
		}
		return nil, err
	}
	return resp.Body, nil
}

func parseRetryAfterHeader(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	val := strings.TrimSpace(h.Get("Retry-After"))
	if val == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(val); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(val); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func capCount(count, max int) int {
	if count > max {
		return max
	}
	return count
}
