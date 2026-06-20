package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	utilhttp "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// web_search — Web 搜索工具
//
// 支持多种搜索引擎后端：
//   - DuckDuckGo Instant Answer API（默认，免费，无需 API Key）
//   - SearXNG（自建实例，隐私友好）
//   - 可扩展其他搜索 API
// ============================================================================

// SearchConfig 配置搜索工具。
type SearchConfig struct {
	// Engine 搜索引擎类型："duckduckgo"、"searxng"。
	// 默认 "duckduckgo"。
	Engine string

	// SearXNGURL SearXNG 实例 URL（Engine="searxng" 时使用）。
	SearXNGURL string

	// Timeout 搜索请求超时。
	// 默认 15 秒。
	Timeout time.Duration

	// MaxResults 最大返回结果数。
	// 默认 5。
	MaxResults int

	// UserAgent HTTP User-Agent。
	UserAgent string
}

// DefaultSearchConfig 返回默认搜索配置。
func DefaultSearchConfig() SearchConfig {
	return SearchConfig{
		Engine:     "duckduckgo",
		Timeout:    15 * time.Second,
		MaxResults: 5,
		UserAgent:  "ThinkbotBot/1.0",
	}
}

// SearchResult 是单条搜索结果。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// searchToolDef 返回 web_search 工具定义。
func searchToolDef(cfg SearchConfig) agenttools.ToolDef {
	if cfg.Engine == "" {
		cfg.Engine = "duckduckgo"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxResults == 0 {
		cfg.MaxResults = 5
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "ThinkbotBot/1.0"
	}

	searcher := newSearcher(cfg)

	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "web_search",
			Description: "搜索互联网获取信息。返回相关网页的标题、URL和摘要。" +
				"适用于查找最新信息、事实核查、获取不了解的概念解释。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "搜索关键词",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "最大返回结果数（默认5）",
					},
				},
				"required": []string{"query"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				query, _ := m["query"].(string)
				if query == "" {
					return nil, fmt.Errorf("query is required")
				}
				maxResults := cfg.MaxResults
				if v, ok := toIntSearch(m["max_results"]); ok && v > 0 {
					maxResults = v
				}

				results, err := searcher.Search(ctx, query, maxResults)
				if err != nil {
					return map[string]any{
						"query":   query,
						"error":   err.Error(),
						"results": []SearchResult{},
					}, nil
				}

				return map[string]any{
					"query":       query,
					"engine":      cfg.Engine,
					"resultCount": len(results),
					"results":     results,
				}, nil
			}),
		},
		Category: "search",
	}
}

// ============================================================================
// 搜索引擎实现
// ============================================================================

// searcher 搜索引擎接口。
type searcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
}

// newSearcher 根据配置创建搜索引擎。
func newSearcher(cfg SearchConfig) searcher {
	switch cfg.Engine {
	case "searxng":
		return &searxngSearcher{config: cfg, client: newSearchClient(cfg)}
	default:
		return &duckDuckGoSearcher{config: cfg, client: newSearchClient(cfg)}
	}
}

// newSearchClient 根据配置创建带 trace ID 注入的 HTTP 客户端。
func newSearchClient(cfg SearchConfig) *utilhttp.Client {
	return utilhttp.New(
		utilhttp.WithTimeout(cfg.Timeout),
		utilhttp.WithHeader("User-Agent", cfg.UserAgent),
		utilhttp.WithMaxBodySize(1<<20), // 1MB
	)
}

// duckDuckGoSearcher 使用 DuckDuckGo Instant Answer API。
type duckDuckGoSearcher struct {
	config SearchConfig
	client *utilhttp.Client
}

func (s *duckDuckGoSearcher) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	apiURL := fmt.Sprintf("https://api.duckduckgo.com/?q=%s&format=json&no_html=1&skip_disambig=1",
		url.QueryEscape(query))

	resp, err := s.client.Get(apiURL).SetContext(ctx).Do()
	if err != nil {
		// 非 2xx 时 resp 可能为 nil
		if resp != nil {
			traceid.L(ctx).Warnw("duckduckgo search: non-2xx response",
				"query", query, "status", resp.StatusCode)
		}
		return nil, err
	}

	var ddgResp struct {
		Abstract      string `json:"Abstract"`
		AbstractURL   string `json:"AbstractURL"`
		Heading       string `json:"Heading"`
		Answer        string `json:"Answer"`
		Definition    string `json:"Definition"`
		DefinitionURL string `json:"DefinitionURL"`
		Related       []struct {
			Text string `json:"Text"`
			URL  string `json:"FirstURL"`
		} `json:"RelatedTopics"`
		Results []struct {
			Text string `json:"Text"`
			URL  string `json:"FirstURL"`
		} `json:"Results"`
	}

	if err := json.Unmarshal(resp.Body, &ddgResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	var results []SearchResult

	// 主要结果
	if ddgResp.Abstract != "" || ddgResp.Heading != "" {
		results = append(results, SearchResult{
			Title:   ddgResp.Heading,
			Snippet: ddgResp.Abstract,
			URL:     ddgResp.AbstractURL,
		})
	}

	// 定义
	if ddgResp.Definition != "" {
		results = append(results, SearchResult{
			Title:   "Definition: " + ddgResp.Heading,
			Snippet: ddgResp.Definition,
			URL:     ddgResp.DefinitionURL,
		})
	}

	// 直接答案
	if ddgResp.Answer != "" {
		results = append(results, SearchResult{
			Title:   "Answer",
			Snippet: ddgResp.Answer,
		})
	}

	// 相关主题
	for _, rt := range ddgResp.Related {
		if len(results) >= maxResults {
			break
		}
		if rt.Text == "" {
			continue
		}
		results = append(results, SearchResult{
			Title:   truncateText(rt.Text, 80),
			Snippet: rt.Text,
			URL:     rt.URL,
		})
	}

	// 如果 Instant Answer 没有结果，回退到 HTML 搜索链接
	if len(results) == 0 {
		results = append(results, SearchResult{
			Title:   "Search results for " + query,
			Snippet: "No instant answer available. See full search results.",
			URL:     "https://duckduckgo.com/?q=" + url.QueryEscape(query),
		})
	}

	if len(results) > maxResults {
		results = results[:maxResults]
	}

	return results, nil
}

// searxngSearcher 使用 SearXNG 实例。
type searxngSearcher struct {
	config SearchConfig
	client *utilhttp.Client
}

func (s *searxngSearcher) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	baseURL := s.config.SearXNGURL
	if baseURL == "" {
		return nil, fmt.Errorf("searxng: URL not configured")
	}

	apiURL := fmt.Sprintf("%s/search?q=%s&format=json&categories=general",
		strings.TrimRight(baseURL, "/"), url.QueryEscape(query))

	resp, err := s.client.Get(apiURL).SetContext(ctx).Do()
	if err != nil {
		if resp != nil {
			traceid.L(ctx).Warnw("searxng search: non-2xx response",
				"query", query, "status", resp.StatusCode)
		}
		return nil, err
	}

	var sxResp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}

	if err := json.Unmarshal(resp.Body, &sxResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	results := make([]SearchResult, 0, maxResults)
	for i, r := range sxResp.Results {
		if i >= maxResults {
			break
		}
		results = append(results, SearchResult{
			Title:   r.Title,
			Snippet: r.Content,
			URL:     r.URL,
		})
	}

	return results, nil
}

// RegisterSearchTools 注册搜索相关工具。
func RegisterSearchTools(mgr *agenttools.ToolManager, cfg SearchConfig) error {
	return mgr.Register(searchToolDef(cfg))
}

// 辅助函数
func toIntSearch(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
