package searchproviders

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	xhtml "golang.org/x/net/html"

	utilhttp "github.com/kasuganosora/thinkbot/util/http"
)

// InstantAnswerHost is the old DuckDuckGo Instant Answer API host. Search must never call it.
const InstantAnswerHost = "api.duckduckgo.com"

const (
	defaultDDGHTML = "https://html.duckduckgo.com/html/"
	defaultDDGLite = "https://lite.duckduckgo.com/lite/"
	ddgRegion      = "wt-wt"
)

// 测试可覆盖端点与 202 退避，避免单测打到公网或空等。
var (
	ddgHTMLEndpoint = defaultDDGHTML
	ddgLiteEndpoint = defaultDDGLite
	ddg202Sleep     = time.Sleep
	ddg202Backoffs  = []time.Duration{150 * time.Millisecond, 300 * time.Millisecond}
)

func callDuckDuckGo(ctx context.Context, p Provider, query string, count int) ([]Result, error) {
	htmlURL := firstNonEmpty(strings.TrimSpace(p.BaseURL), ddgHTMLEndpoint)
	timeout := p.timeoutOrDefault()

	body, err := postDDG(ctx, htmlURL, query, timeout)
	if err == nil {
		if results := parseDuckDuckGoHTML(string(body), count); len(results) > 0 {
			return results, nil
		}
	}

	// HTML 空结果或被挡时改试 lite。自定义 BaseURL 且未覆盖 lite 端点时不打生产 lite。
	customBase := strings.TrimSpace(p.BaseURL) != "" && strings.TrimSpace(p.BaseURL) != ddgHTMLEndpoint
	if customBase && ddgLiteEndpoint == defaultDDGLite {
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	liteURL := ddgLiteEndpoint
	if liteURL == "" || liteURL == htmlURL {
		if err != nil {
			return nil, err
		}
		return nil, nil
	}

	liteBody, liteErr := postDDG(ctx, liteURL, query, timeout)
	if liteErr != nil {
		if err != nil {
			return nil, err
		}
		return nil, liteErr
	}
	return parseDuckDuckGoHTML(string(liteBody), count), nil
}

func postDDG(ctx context.Context, endpoint, query string, timeout time.Duration) ([]byte, error) {
	if strings.Contains(strings.ToLower(endpoint), InstantAnswerHost) {
		return nil, fmt.Errorf("refusing Instant Answer host %s", InstantAnswerHost)
	}
	form := url.Values{}
	form.Set("q", query)
	form.Set("b", "")
	form.Set("l", ddgRegion)
	encoded := form.Encode()

	client := newSearchClient(timeout, ddgUserAgent)
	var lastErr error
	maxAttempts := 1 + len(ddg202Backoffs)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			ddg202Sleep(ddg202Backoffs[attempt-1])
		}
		req := client.Post(endpoint).
			SetHeader("Content-Type", "application/x-www-form-urlencoded").
			SetBody(strings.NewReader(encoded))
		resp, err := req.SetContext(ctx).Do()
		if isDDG202(resp) {
			lastErr = ddgHTTPError(resp, http.StatusAccepted)
			continue
		}
		if err != nil {
			if resp != nil && !resp.IsSuccess() {
				return nil, ddgHTTPError(resp, resp.StatusCode)
			}
			return nil, err
		}
		if resp != nil {
			return resp.Body, nil
		}
		return nil, fmt.Errorf("duckduckgo empty response")
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("duckduckgo HTTP 202 ratelimit")
}

func isDDG202(resp *utilhttp.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusAccepted
}

func ddgHTTPError(resp *utilhttp.Response, status int) error {
	if resp == nil {
		return newHTTPSearchError(status, nil, 0)
	}
	return newHTTPSearchError(resp.StatusCode, resp.Body, parseRetryAfterHeader(resp.Headers))
}

func parseDuckDuckGoHTML(htmlStr string, count int) []Result {
	if count <= 0 {
		count = maxResultsCap
	}
	doc, err := xhtml.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return filterDDGResults(parseDuckDuckGoLegacy(htmlStr, count), count)
	}
	if got := collectDDGBodyDivs(doc, count); len(got) > 0 {
		return got
	}
	if got := collectDDGLite(doc, count); len(got) > 0 {
		return got
	}
	if got := collectDDGResultA(doc, count); len(got) > 0 {
		return got
	}
	return filterDDGResults(parseDuckDuckGoLegacy(htmlStr, count), count)
}

func collectDDGBodyDivs(n *xhtml.Node, count int) []Result {
	var out []Result
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if len(out) >= count || node == nil {
			return
		}
		if node.Type == xhtml.ElementNode && node.Data == "div" && hasClassToken(node, "result__body") {
			if r, ok := resultFromBodyDiv(node); ok {
				out = append(out, r)
			}
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func resultFromBodyDiv(n *xhtml.Node) (Result, bool) {
	h2 := findFirstElem(n, "h2")
	title := ""
	if h2 != nil {
		title = compactText(h2)
	}
	href := ""
	if a := findFirstElemWithClass(n, "a", "result__a"); a != nil {
		href = nodeAttr(a, "href")
		if title == "" {
			title = compactText(a)
		}
	}
	if href == "" {
		if a := findDirectChild(n, "a"); a != nil {
			href = nodeAttr(a, "href")
			if title == "" {
				title = compactText(a)
			}
		}
	}
	if href == "" {
		if a := findFirstElem(n, "a"); a != nil {
			href = nodeAttr(a, "href")
			if title == "" {
				title = compactText(a)
			}
		}
	}
	snippet := ""
	if sn := findFirstElemWithClass(n, "a", "result__snippet"); sn != nil {
		snippet = compactText(sn)
	}
	if snippet == "" {
		if sn := findFirstElemWithClass(n, "td", "result-snippet"); sn != nil {
			snippet = compactText(sn)
		}
	}
	return finishDDGResult(title, href, snippet)
}

func collectDDGLite(n *xhtml.Node, count int) []Result {
	var out []Result
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if len(out) >= count || node == nil {
			return
		}
		if node.Type == xhtml.ElementNode && node.Data == "a" && hasClassToken(node, "result-link") {
			href := nodeAttr(node, "href")
			title := compactText(node)
			snippet := liteSnippetAfter(node)
			if r, ok := finishDDGResult(title, href, snippet); ok {
				out = append(out, r)
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func liteSnippetAfter(a *xhtml.Node) string {
	// lite 页每条结果是相邻 tr：result-link 下一组 tr 里的 result-snippet。
	start := a
	for start != nil && start.Data != "tr" {
		start = start.Parent
	}
	if start == nil {
		return ""
	}
	for sib := start.NextSibling; sib != nil; sib = sib.NextSibling {
		if sib.Type != xhtml.ElementNode {
			continue
		}
		if sn := findFirstElemWithClass(sib, "td", "result-snippet"); sn != nil {
			return compactText(sn)
		}
		if sib.Data == "tr" && findFirstElemWithClass(sib, "a", "result-link") != nil {
			return ""
		}
	}
	return ""
}

func collectDDGResultA(n *xhtml.Node, count int) []Result {
	var out []Result
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if len(out) >= count || node == nil {
			return
		}
		if node.Type == xhtml.ElementNode && node.Data == "a" && hasClassToken(node, "result__a") {
			href := nodeAttr(node, "href")
			title := compactText(node)
			snippet := ""
			if p := node.Parent; p != nil {
				if sn := findFirstElemWithClass(p.Parent, "a", "result__snippet"); sn != nil && p.Parent != nil {
					snippet = compactText(sn)
				} else if sn := findFirstElemWithClass(p, "a", "result__snippet"); sn != nil {
					snippet = compactText(sn)
				}
			}
			if r, ok := finishDDGResult(title, href, snippet); ok {
				out = append(out, r)
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func finishDDGResult(title, href, snippet string) (Result, bool) {
	href = html.UnescapeString(strings.TrimSpace(href))
	if isDDGAdURL(href) {
		return Result{}, false
	}
	realURL := extractDDGURL(href)
	if realURL == "" || isDummySearchURL(realURL) || isDDGAdURL(realURL) {
		return Result{}, false
	}
	return Result{
		Title:   strings.TrimSpace(html.UnescapeString(title)),
		URL:     realURL,
		Snippet: strings.TrimSpace(html.UnescapeString(snippet)),
	}, true
}

func filterDDGResults(in []Result, count int) []Result {
	out := make([]Result, 0, len(in))
	for _, r := range in {
		if isDDGAdURL(r.URL) || isDummySearchURL(r.URL) || r.URL == "" {
			continue
		}
		out = append(out, r)
		if len(out) >= count {
			break
		}
	}
	return out
}

func isDDGAdURL(u string) bool {
	u = strings.TrimSpace(html.UnescapeString(u))
	if u == "" {
		return false
	}
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return strings.Contains(strings.ToLower(u), "y.js")
	}
	host := strings.ToLower(parsed.Hostname())
	path := strings.ToLower(parsed.Path)
	if !strings.Contains(path, "y.js") {
		return false
	}
	return host == "" || strings.Contains(host, "duckduckgo.com")
}

func extractDDGURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if strings.HasPrefix(rawURL, "//") {
		rawURL = "https:" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err == nil {
		if uddg := parsed.Query().Get("uddg"); uddg != "" {
			if dec, err := url.QueryUnescape(uddg); err == nil && dec != "" {
				return dec
			}
			return uddg
		}
	}
	return rawURL
}

func isDummySearchURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Host)
	if host == InstantAnswerHost || strings.Contains(host, InstantAnswerHost) {
		return true
	}
	if host == "duckduckgo.com" || host == "www.duckduckgo.com" || host == "html.duckduckgo.com" || host == "lite.duckduckgo.com" {
		q := parsed.Query()
		if q.Get("q") != "" && (parsed.Path == "/" || parsed.Path == "") {
			return true
		}
	}
	return false
}

func hasClassToken(n *xhtml.Node, want string) bool {
	for _, tok := range strings.Fields(nodeAttr(n, "class")) {
		if tok == want {
			return true
		}
	}
	return false
}

func nodeAttr(n *xhtml.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func findFirstElem(n *xhtml.Node, tag string) *xhtml.Node {
	if n == nil {
		return nil
	}
	if n.Type == xhtml.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := findFirstElem(c, tag); got != nil {
			return got
		}
	}
	return nil
}

func findFirstElemWithClass(n *xhtml.Node, tag, class string) *xhtml.Node {
	if n == nil {
		return nil
	}
	if n.Type == xhtml.ElementNode && (tag == "" || n.Data == tag) && hasClassToken(n, class) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := findFirstElemWithClass(c, tag, class); got != nil {
			return got
		}
	}
	return nil
}

func findDirectChild(n *xhtml.Node, tag string) *xhtml.Node {
	if n == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == xhtml.ElementNode && c.Data == tag {
			return c
		}
	}
	return nil
}

func compactText(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node == nil {
			return
		}
		if node.Type == xhtml.TextNode {
			b.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.FieldsFunc(b.String(), unicode.IsSpace), " ")
}

var (
	ddgResultLinkRe    = regexp.MustCompile(`class="result__a"[^>]*href="([^"]+)"`)
	ddgResultTitleRe   = regexp.MustCompile(`class="result__a"[^>]*>([^<]+)<`)
	ddgResultSnippetRe = regexp.MustCompile(`class="result__snippet"[^>]*>([\s\S]*?)</a>`)
	ddgHTMLTagRe       = regexp.MustCompile(`<[^>]*>`)
)

func parseDuckDuckGoLegacy(htmlStr string, count int) []Result {
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
		title := html.UnescapeString(strings.TrimSpace(titles[i][1]))
		snippet := ""
		if i < len(snippets) {
			snippet = html.UnescapeString(strings.TrimSpace(ddgHTMLTagRe.ReplaceAllString(snippets[i][1], "")))
		}
		if r, ok := finishDDGResult(title, links[i][1], snippet); ok {
			results = append(results, r)
		}
	}
	return results
}

// OverrideDDGEndpoints 覆盖 HTML/lite 端点（单测用）。空字符串表示保持原值。
func OverrideDDGEndpoints(htmlURL, liteURL string) func() {
	prevH, prevL, prevSleep := ddgHTMLEndpoint, ddgLiteEndpoint, ddg202Sleep
	if strings.TrimSpace(htmlURL) != "" {
		ddgHTMLEndpoint = htmlURL
	}
	if strings.TrimSpace(liteURL) != "" {
		ddgLiteEndpoint = liteURL
	}
	ddg202Sleep = func(time.Duration) {}
	return func() {
		ddgHTMLEndpoint = prevH
		ddgLiteEndpoint = prevL
		ddg202Sleep = prevSleep
	}
}
