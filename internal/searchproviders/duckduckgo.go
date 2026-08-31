package searchproviders

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
)

// InstantAnswerHost is the old DuckDuckGo Instant Answer API host. Search must never call it.
const InstantAnswerHost = "api.duckduckgo.com"

func callDuckDuckGo(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	endpoint := firstNonEmpty(p.BaseURL, "https://html.duckduckgo.com/html/")
	form := url.Values{}
	form.Set("q", query)
	form.Set("b", "")
	form.Set("kl", "")

	client := newSearchClient(p.timeoutOrDefault(), ddgUserAgent)
	req := client.Post(endpoint).
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetBody(strings.NewReader(form.Encode()))
	body, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	return parseDuckDuckGoHTML(string(body), count), nil
}

var (
	ddgResultLinkRe    = regexp.MustCompile(`class="result__a"[^>]*href="([^"]+)"`)
	ddgResultTitleRe   = regexp.MustCompile(`class="result__a"[^>]*>([^<]+)<`)
	ddgResultSnippetRe = regexp.MustCompile(`class="result__snippet"[^>]*>([\s\S]*?)</a>`)
	ddgHTMLTagRe       = regexp.MustCompile(`<[^>]*>`)
)

func parseDuckDuckGoHTML(htmlStr string, count int) []Result {
	links := ddgResultLinkRe.FindAllStringSubmatch(htmlStr, -1)
	titles := ddgResultTitleRe.FindAllStringSubmatch(htmlStr, -1)
	snippets := ddgResultSnippetRe.FindAllStringSubmatch(htmlStr, -1)
	n := len(links)
	if len(titles) < n {
		n = len(titles)
	}
	if count < n {
		n = count
	}
	results := make([]Result, 0, n)
	for i := 0; i < n; i++ {
		rawURL := html.UnescapeString(links[i][1])
		realURL := extractDDGURL(rawURL)
		title := html.UnescapeString(strings.TrimSpace(titles[i][1]))
		snippet := ""
		if i < len(snippets) {
			snippet = html.UnescapeString(strings.TrimSpace(ddgHTMLTagRe.ReplaceAllString(snippets[i][1], "")))
		}
		if realURL == "" {
			continue
		}
		if isDummySearchURL(realURL) {
			continue
		}
		results = append(results, Result{Title: title, URL: realURL, Snippet: snippet})
	}
	return results
}

func extractDDGURL(rawURL string) string {
	if strings.Contains(rawURL, "uddg=") {
		parsed, err := url.Parse(rawURL)
		if err == nil {
			if uddg := parsed.Query().Get("uddg"); uddg != "" {
				return uddg
			}
		}
	}
	if strings.HasPrefix(rawURL, "//") {
		return "https:" + rawURL
	}
	return rawURL
}

func isDummySearchURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Host)
	if host == "duckduckgo.com" || host == "www.duckduckgo.com" || host == "html.duckduckgo.com" {
		q := parsed.Query()
		if q.Get("q") != "" && (parsed.Path == "/" || parsed.Path == "") {
			return true
		}
	}
	if strings.Contains(host, "api.duckduckgo.com") {
		return true
	}
	return false
}
