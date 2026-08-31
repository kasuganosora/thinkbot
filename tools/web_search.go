package tools

import (
	"fmt"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/internal/searchproviders"
	"github.com/kasuganosora/thinkbot/llm"
)

// SearchConfig 配置 web_search 工具。
//
// 真正的搜索后端不再由 Engine/SearXNGURL 在进程启动时钉死，
// 而是每次调用时读取 Web UI 写入的 providers.json 里已启用的提供方。
type SearchConfig struct {
	// Engine 已废弃： Ignored。保留字段以免旧调用方编译失败。
	Engine string

	// SearXNGURL 已废弃： Ignored。SearXNG 请在搜索提供方设置里填写 Base URL。
	SearXNGURL string

	// MaxResults 默认返回条数，默认 5。
	MaxResults int

	// UserAgent 已废弃：各后端自带 UA（DuckDuckGo HTML 使用浏览器 UA）。
	UserAgent string

	// Store 可选。测试注入；为空时使用 data/search/providers.json。
	Store *searchproviders.Store
}

// DefaultSearchConfig 返回默认搜索配置。
func DefaultSearchConfig() SearchConfig {
	return SearchConfig{MaxResults: 5}
}

func searchToolDef(cfg SearchConfig) agenttools.ToolDef {
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = 5
	}
	store := cfg.Store
	if store == nil {
		store = searchproviders.DefaultStore()
	}

	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "web_search",
			Description: "Search the internet for information. Returns the title, URL and snippet of relevant pages. " +
				"Use this for recent or time-sensitive information, fact checking, and concepts you are unsure about. " +
				"Follow up with web_fetch when a result's snippet is not detailed enough.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query. Use concise keywords rather than a full sentence.",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "Maximum number of results to return. Defaults to 5.",
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

				provider, err := store.Enabled()
				if err != nil {
					return nil, err
				}

				results, err := searchproviders.Search(ctx, *provider, query, maxResults)
				if err != nil {
					return nil, err
				}

				return map[string]any{
					"query":       query,
					"engine":      provider.Type,
					"provider":    provider.Name,
					"resultCount": len(results),
					"results":     results,
				}, nil
			}),
		},
		Category: "search",
	}
}

// RegisterSearchTools 注册搜索相关工具。
func RegisterSearchTools(mgr *agenttools.ToolManager, cfg SearchConfig) error {
	return mgr.Register(searchToolDef(cfg))
}

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
