package searchproviders

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func callBrave(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "brave")
	if err != nil {
		return nil, err
	}
	endpoint := firstNonEmpty(p.BaseURL, "https://api.search.brave.com/res/v1/web/search")
	reqURL, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid search provider base_url")
	}
	params := reqURL.Query()
	params.Set("q", query)
	params.Set("count", strconv.Itoa(count))
	reqURL.RawQuery = params.Encode()

	req := p.httpClient().Get(reqURL.String()).
		SetHeader("Accept", "application/json").
		SetHeader("X-Subscription-Token", apiKey)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	out := make([]Result, 0, len(raw.Web.Results))
	for _, r := range raw.Web.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

func callBing(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "bing")
	if err != nil {
		return nil, err
	}
	endpoint := firstNonEmpty(p.BaseURL, "https://api.bing.microsoft.com/v7.0/search")
	reqURL, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid search provider base_url")
	}
	params := reqURL.Query()
	params.Set("q", query)
	params.Set("count", strconv.Itoa(count))
	reqURL.RawQuery = params.Encode()

	req := p.httpClient().Get(reqURL.String()).
		SetHeader("Accept", "application/json").
		SetHeader("Ocp-Apim-Subscription-Key", apiKey)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		WebPages struct {
			Value []struct {
				Name    string `json:"name"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
			} `json:"value"`
		} `json:"webPages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	out := make([]Result, 0, len(raw.WebPages.Value))
	for _, r := range raw.WebPages.Value {
		out = append(out, Result{Title: r.Name, URL: r.URL, Snippet: r.Snippet})
	}
	return out, nil
}

func callGoogle(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "google")
	if err != nil {
		return nil, err
	}
	cx := googleCX(p)
	if cx == "" {
		return nil, fmt.Errorf("google custom search requires cx (set Search Type to your Programmable Search Engine ID)")
	}
	count = capCount(count, 10)
	endpoint := firstNonEmpty(p.BaseURL, "https://customsearch.googleapis.com/customsearch/v1")
	reqURL, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid search provider base_url")
	}
	params := reqURL.Query()
	params.Set("q", query)
	params.Set("cx", cx)
	params.Set("num", strconv.Itoa(count))
	params.Set("key", apiKey)
	reqURL.RawQuery = params.Encode()

	req := p.httpClient().Get(reqURL.String()).SetHeader("Accept", "application/json")
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	out := make([]Result, 0, len(raw.Items))
	for _, r := range raw.Items {
		out = append(out, Result{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return out, nil
}

// googleCX 把 UI 的 Search Type 字段当作 Google CSE cx。
// 形如 SEARCH_TYPE_* 的值是 Yandex 占位，不能当 cx 用。
func googleCX(p Provider) string {
	st := strings.TrimSpace(p.SearchType)
	if st == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToUpper(st), "SEARCH_TYPE_") {
		return ""
	}
	return st
}

func callTavily(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "tavily")
	if err != nil {
		return nil, err
	}
	endpoint := firstNonEmpty(p.BaseURL, "https://api.tavily.com/search")
	payload := map[string]any{"query": query, "max_results": count}
	req := p.httpClient().Post(endpoint).
		SetHeader("Accept", "application/json").
		SetJSONBody(payload).
		BearerToken(apiKey)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	out := make([]Result, 0, len(raw.Results))
	for _, r := range raw.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

func callSerper(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "serper")
	if err != nil {
		return nil, err
	}
	endpoint := firstNonEmpty(p.BaseURL, "https://google.serper.dev/search")
	payload := map[string]any{"q": query}
	req := p.httpClient().Post(endpoint).
		SetHeader("Accept", "application/json").
		SetHeader("X-API-KEY", apiKey).
		SetJSONBody(payload)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Organic []struct {
			Title       string `json:"title"`
			Link        string `json:"link"`
			Description string `json:"description"`
			Snippet     string `json:"snippet"`
			Position    int    `json:"position"`
		} `json:"organic"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	sort.Slice(raw.Organic, func(i, j int) bool { return raw.Organic[i].Position < raw.Organic[j].Position })
	out := make([]Result, 0, count)
	for i, r := range raw.Organic {
		if i >= count {
			break
		}
		snippet := firstNonEmpty(r.Snippet, r.Description)
		out = append(out, Result{Title: r.Title, URL: r.Link, Snippet: snippet})
	}
	return out, nil
}

func callSearXNG(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	baseURL := strings.TrimSpace(p.BaseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("searxng base URL is required")
	}
	reqURL, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid search provider base_url")
	}
	params := reqURL.Query()
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("pageno", "1")
	if lang := strings.TrimSpace(p.SearchType); lang != "" && !strings.HasPrefix(strings.ToUpper(lang), "SEARCH_TYPE_") {
		params.Set("language", lang)
	}
	reqURL.RawQuery = params.Encode()

	req := p.httpClient().Get(reqURL.String()).SetHeader("Accept", "application/json")
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	sort.Slice(raw.Results, func(i, j int) bool { return raw.Results[i].Score > raw.Results[j].Score })
	out := make([]Result, 0, count)
	for i, r := range raw.Results {
		if i >= count {
			break
		}
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

func callJina(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "jina")
	if err != nil {
		return nil, err
	}
	count = capCount(count, 10)
	endpoint := firstNonEmpty(p.BaseURL, "https://s.jina.ai/")
	payload := map[string]any{"q": query, "count": count}
	req := p.httpClient().Post(endpoint).
		SetHeader("Accept", "application/json").
		SetHeader("X-Retain-Images", "none").
		SetHeader("Authorization", apiKey).
		SetJSONBody(payload)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	out := make([]Result, 0, len(raw.Data))
	for _, r := range raw.Data {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

func callExa(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "exa")
	if err != nil {
		return nil, err
	}
	endpoint := firstNonEmpty(p.BaseURL, "https://api.exa.ai/search")
	payload := map[string]any{
		"query":      query,
		"numResults": count,
		"contents":   map[string]any{"text": true, "highlights": true},
		"type":       "auto",
	}
	req := p.httpClient().Post(endpoint).
		SetHeader("Accept", "application/json").
		SetJSONBody(payload).
		BearerToken(apiKey)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	out := make([]Result, 0, len(raw.Results))
	for _, r := range raw.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Text})
	}
	return out, nil
}

func callBocha(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "bocha")
	if err != nil {
		return nil, err
	}
	endpoint := firstNonEmpty(p.BaseURL, "https://api.bochaai.com/v1/web-search")
	payload := map[string]any{"query": query, "summary": true, "freshness": "noLimit", "count": count}
	req := p.httpClient().Post(endpoint).
		SetHeader("Accept", "application/json").
		SetJSONBody(payload).
		BearerToken(apiKey)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data struct {
			WebPages struct {
				Value []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Summary string `json:"summary"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	out := make([]Result, 0, len(raw.Data.WebPages.Value))
	for _, r := range raw.Data.WebPages.Value {
		out = append(out, Result{Title: r.Name, URL: r.URL, Snippet: r.Summary})
	}
	return out, nil
}

func callYandex(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	apiKey, err := requireAPIKey(p, "yandex")
	if err != nil {
		return nil, err
	}
	endpoint := firstNonEmpty(p.BaseURL, "https://searchapi.api.cloud.yandex.net/v2/web/search")
	searchType := firstNonEmpty(p.SearchType, "SEARCH_TYPE_RU")
	payload := map[string]any{
		"query":     map[string]any{"queryText": query, "searchType": searchType},
		"groupSpec": map[string]any{"groupMode": "GROUP_MODE_DEEP", "groupsOnPage": count, "docsInGroup": 1},
	}
	req := p.httpClient().Post(endpoint).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", "Api-Key "+apiKey).
		SetJSONBody(payload)
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var rawResp struct {
		RawData string `json:"rawData"`
	}
	if err := json.Unmarshal(body, &rawResp); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	return parseYandexRawData(rawResp.RawData)
}

func callSogou(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	secretID, secretKey := sogouCreds(p)
	if secretID == "" || secretKey == "" {
		return nil, fmt.Errorf("sogou search requires Tencent Cloud SecretId (Search Type) and SecretKey (API Key)")
	}
	host := firstNonEmpty(strings.TrimPrefix(strings.TrimPrefix(p.BaseURL, "https://"), "http://"), "wsa.tencentcloudapi.com")
	host = strings.TrimRight(host, "/")
	action := "SearchPro"
	version := "2025-05-08"
	service := "wsa"
	payload, err := json.Marshal(map[string]any{"Query": query, "Mode": 0})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	timestamp := strconv.FormatInt(now.Unix(), 10)
	date := now.Format("2006-01-02")
	hashedPayload := sha256Hex(payload)
	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\n", "application/json", host)
	signedHeaders := "content-type;host"
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s", "POST", "/", "", canonicalHeaders, signedHeaders, hashedPayload)
	credentialScope := fmt.Sprintf("%s/%s/tc3_request", date, service)
	stringToSign := fmt.Sprintf("TC3-HMAC-SHA256\n%s\n%s\n%s", timestamp, credentialScope, sha256Hex([]byte(canonicalRequest)))
	secretDate := hmacSHA256([]byte("TC3"+secretKey), []byte(date))
	secretService := hmacSHA256(secretDate, []byte(service))
	secretSigning := hmacSHA256(secretService, []byte("tc3_request"))
	signature := hex.EncodeToString(hmacSHA256(secretSigning, []byte(stringToSign)))
	authorization := fmt.Sprintf("TC3-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", secretID, credentialScope, signedHeaders, signature)

	endpoint := "https://" + host + "/"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.BaseURL)), "http://") || strings.Contains(p.BaseURL, "127.0.0.1") || strings.Contains(p.BaseURL, "localhost") {
		endpoint = firstNonEmpty(p.BaseURL, endpoint)
	}
	req := p.httpClient().Post(endpoint).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", authorization).
		SetHeader("Host", host).
		SetHeader("X-TC-Action", action).
		SetHeader("X-TC-Version", version).
		SetHeader("X-TC-Timestamp", timestamp).
		SetBody(bytes.NewReader(payload))
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	var rawResp struct {
		Response struct {
			Error *struct{ Code, Message string } `json:"Error,omitempty"`
			Pages []json.RawMessage               `json:"Pages"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &rawResp); err != nil {
		return nil, fmt.Errorf("invalid search response")
	}
	if rawResp.Response.Error != nil {
		return nil, fmt.Errorf("sogou search failed: %s", rawResp.Response.Error.Message)
	}
	type sogouPage struct {
		Title   string
		URL     string
		Passage string
		Score   float64 `json:"scour"`
	}
	var pages []sogouPage
	for _, raw := range rawResp.Response.Pages {
		var rawStr string
		if err := json.Unmarshal(raw, &rawStr); err == nil {
			var page sogouPage
			if json.Unmarshal([]byte(rawStr), &page) == nil {
				pages = append(pages, page)
			}
			continue
		}
		var page sogouPage
		if json.Unmarshal(raw, &page) == nil {
			pages = append(pages, page)
		}
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Score > pages[j].Score })
	out := make([]Result, 0, count)
	for i, page := range pages {
		if i >= count {
			break
		}
		out = append(out, Result{Title: page.Title, URL: page.URL, Snippet: page.Passage})
	}
	return out, nil
}

func sogouCreds(p Provider) (secretID, secretKey string) {
	st := strings.TrimSpace(p.SearchType)
	if strings.HasPrefix(strings.ToUpper(st), "SEARCH_TYPE_") {
		st = ""
	}
	return st, strings.TrimSpace(p.APIKey)
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
