package tools

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	utilhttp "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// web_fetch — 获取网页内容 / 发送 HTTP 请求
// ============================================================================

func webFetchToolDef(cfg Config) agenttools.ToolDef {
	client := utilhttp.New(
		utilhttp.WithTimeout(cfg.HTTPTimeout),
		utilhttp.WithHeader("User-Agent", cfg.UserAgent),
		utilhttp.WithMaxBodySize(int64(cfg.MaxFetchSize)),
	)

	return agenttools.ToolDef{
		Category: "utility",
		Tool: llm.Tool{
			Name: "web_fetch",
			Description: "Fetch the content of a URL over HTTP (GET by default). " +
				"Set method/headers/body to send other request types (POST/PUT/DELETE/PATCH/HEAD). " +
				"Returns the HTTP status code, Content-Type and a truncated response body; " +
				"a non-2xx response is reported as an error with the status code and a body snippet. " +
				"Non-ASCII characters in the URL are percent-encoded automatically. " +
				"IMPORTANT: The url MUST be one the user provided or one you obtained from a tool result — never invent URLs.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The URL to request. MUST start with http:// or https://",
					},
					"method": map[string]any{
						"type":        "string",
						"description": "HTTP method. Defaults to GET. One of GET/POST/PUT/DELETE/PATCH/HEAD.",
						"default":     "GET",
					},
					"headers": map[string]any{
						"type":        "object",
						"description": "Optional custom request headers as key/value pairs.",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "Optional request body, used with POST/PUT/PATCH.",
					},
				},
				"required": []string{"url"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				logger := traceid.L(ctx)

				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				rawURL, _ := m["url"].(string)
				if rawURL == "" {
					return nil, fmt.Errorf("url is required")
				}
				if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
					return nil, fmt.Errorf("url must start with http:// or https://")
				}

				method, _ := m["method"].(string)
				if method == "" {
					method = "GET"
				}

				// 模型常直接给出含中文/空格的原始 URL（如 https://www.bing.com/search?q=Re0+老吴+梗）。
				// Go 会替路径转义，但查询串原样上线，Bing 等站点直接回 400 空 body。
				// 这里按浏览器的做法转义非 ASCII / 空白 / 非法字符，已有的 %XX 保持不变（不双重转义）。
				reqURL, err := normalizeFetchURL(rawURL)
				if err != nil {
					return nil, err
				}

				logger.Debugw("web_fetch executing",
					"url", reqURL, "method", strings.ToUpper(method))

				req := client.NewRequest(method, reqURL).SetContext(ctx)

				if method == "GET" {
					req.SetHeader("Accept", "text/html,application/json,*/*")
				}

				// 自定义请求头
				if headers, ok := m["headers"].(map[string]any); ok {
					for k, v := range headers {
						req.SetHeader(k, fmt.Sprint(v))
					}
				}

				// 请求体
				if bodyStr, _ := m["body"].(string); bodyStr != "" {
					req.SetBody(strings.NewReader(bodyStr))
				}

				resp, err := req.Do()
				if resp != nil && !resp.IsSuccess() {
					// 非 2xx（utilhttp 此时同时返回 resp 与 err）：作为工具错误返回（is_error=true），
					// 带上状态码与 body 片段，避免模型把 400 空 body 当成「页面没有内容」。
					logger.Infow("web_fetch non-2xx", "url", utilhttp.SanitizeURL(reqURL), "method", method, "status", resp.StatusCode)
					return nil, webFetchStatusError(method, reqURL, resp)
				}
				if err != nil {
					logger.Warnw("web_fetch failed", "url", utilhttp.SanitizeURL(reqURL), "method", method, "err", err)
					return nil, fmt.Errorf("request failed: %w", err)
				}

				return map[string]any{
					"statusCode":  resp.StatusCode,
					"status":      fmt.Sprintf("%d", resp.StatusCode),
					"contentType": resp.Headers.Get("Content-Type"),
					"body":        resp.String(),
					"bodySize":    len(resp.Body),
					"truncated":   int64(len(resp.Body)) >= int64(cfg.MaxFetchSize),
					"finalURL":    reqURL,
				}, nil
			}),
		},
	}
}

// webFetchErrorSnippetBytes 是非 2xx 错误里附带的 body 片段上限。
const webFetchErrorSnippetBytes = 500

// webFetchStatusError 把非 2xx 响应转成工具错误：状态码 + Content-Type + body 片段。
func webFetchStatusError(method, reqURL string, resp *utilhttp.Response) error {
	body := strings.TrimSpace(resp.String())
	snippet := "<empty body>"
	if body != "" {
		snippet = truncateUTF8(body, webFetchErrorSnippetBytes)
	}
	ct := resp.Headers.Get("Content-Type")
	if ct == "" {
		ct = "-"
	}
	return fmt.Errorf("HTTP %d %s from %s %s (content-type: %s): %s",
		resp.StatusCode, httpStatusText(resp.StatusCode), strings.ToUpper(method),
		utilhttp.SanitizeURL(reqURL), ct, snippet)
}

func httpStatusText(code int) string {
	switch {
	case code >= 500:
		return "server error"
	case code >= 400:
		return "client error"
	case code >= 300:
		return "redirect not followed"
	}
	return "unexpected status"
}

// truncateUTF8 截到不超过 n 字节且落在 rune 边界上，截断时追加省略号。
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// normalizeFetchURL 把模型给出的 URL 规范成可以直接上线的形式：
//   - 主机名含非 ASCII（中文域名）→ IDNA/punycode；
//   - 路径 / 查询 / 片段中的非 ASCII、空白、控制字符及 " < > \ ^ ` { | } → %XX（UTF-8 字节）；
//   - 已有的合法 %XX 原样保留（已编码的 URL 不会被二次编码），落单的 % 转成 %25；
//   - 其余字符（含 / ? & = + # 等保留字符）原样保留，不改变 URL 语义。
func normalizeFetchURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	sep := strings.Index(raw, "://")
	if sep < 0 {
		return "", fmt.Errorf("url must start with http:// or https://")
	}
	prefix, rest := raw[:sep+3], raw[sep+3:]
	authEnd := strings.IndexAny(rest, "/?#")
	if authEnd < 0 {
		authEnd = len(rest)
	}
	authority, tail := rest[:authEnd], rest[authEnd:]
	if authority == "" {
		return "", fmt.Errorf("invalid url %q: missing host", raw)
	}
	if !isASCII(authority) {
		userinfo, hostport := "", authority
		if i := strings.LastIndex(authority, "@"); i >= 0 {
			userinfo, hostport = authority[:i+1], authority[i+1:]
		}
		host, port := hostport, ""
		if i := strings.LastIndex(hostport, ":"); i >= 0 && !strings.Contains(hostport[i:], "]") {
			host, port = hostport[:i], hostport[i:]
		}
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return "", fmt.Errorf("invalid host %q: %w", host, err)
		}
		authority = userinfo + ascii + port
	}
	out := prefix + authority + escapeURLTail(tail)
	if _, err := url.Parse(out); err != nil {
		return "", fmt.Errorf("invalid url %q: %w", raw, err)
	}
	return out, nil
}

func escapeURLTail(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]):
			b.WriteByte(c) // 已编码：原样保留
		case c == '%':
			b.WriteString("%25")
		case c >= 0x80 || c <= 0x20 || c == 0x7f || strings.IndexByte("\"<>\\^`{|}", c) >= 0:
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
