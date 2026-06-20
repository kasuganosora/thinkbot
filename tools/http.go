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
			Description: "获取指定 URL 的内容（默认 HTTP GET）。" +
				"可通过 method/headers/body 参数发送自定义请求（POST/PUT/DELETE 等）。" +
				"返回 HTTP 状态码、Content-Type、响应头和截断后的响应正文。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "要获取的 URL（必须包含 http:// 或 https:// 前缀）",
					},
					"method": map[string]any{
						"type":        "string",
						"description": "HTTP 方法，默认 GET。可选 GET/POST/PUT/DELETE/PATCH/HEAD 等",
						"default":     "GET",
					},
					"headers": map[string]any{
						"type":        "object",
						"description": "自定义请求头键值对（可选）",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "请求体内容（可选，用于 POST/PUT/PATCH 等）",
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
