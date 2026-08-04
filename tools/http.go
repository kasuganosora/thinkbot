package tools

import (
	"fmt"
	"strings"

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
				"Returns the HTTP status code, Content-Type and a truncated response body. " +
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

				logger.Debugw("web_fetch executing",
					"url", rawURL, "method", strings.ToUpper(method))

				req := client.NewRequest(method, rawURL).SetContext(ctx)

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
				if err != nil {
					// resp 可能在错误时非 nil（如非 2xx 状态码）
					if resp != nil {
						return map[string]any{
							"statusCode":  resp.StatusCode,
							"status":      fmt.Sprintf("%d", resp.StatusCode),
							"contentType": resp.Headers.Get("Content-Type"),
							"body":        resp.String(),
							"bodySize":    len(resp.Body),
							"truncated":   int64(len(resp.Body)) >= int64(cfg.MaxFetchSize),
							"finalURL":    rawURL,
						}, nil
					}
					logger.Warnw("web_fetch failed", "url", rawURL, "method", method, "err", err)
					return nil, fmt.Errorf("request failed: %w", err)
				}

				return map[string]any{
					"statusCode":  resp.StatusCode,
					"status":      fmt.Sprintf("%d", resp.StatusCode),
					"contentType": resp.Headers.Get("Content-Type"),
					"body":        resp.String(),
					"bodySize":    len(resp.Body),
					"truncated":   int64(len(resp.Body)) >= int64(cfg.MaxFetchSize),
					"finalURL":    rawURL,
				}, nil
			}),
		},
	}
}
