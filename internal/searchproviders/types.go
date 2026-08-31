package searchproviders

import "time"

// Provider 是 Web UI / API 持久化到 data/search/providers.json 的搜索提供方。
type Provider struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Letter     string `json:"letter"`
	Color      string `json:"color"`
	Enabled    bool   `json:"enabled"`
	APIKey     string `json:"apiKey"`
	SearchType string `json:"searchType"`
	Timeout    int    `json:"timeout"`
	BaseURL    string `json:"baseUrl"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// Result 是一条真实搜索命中。禁止用搜索引擎首页链接冒充命中。
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// 与 Web UI 下拉框一致的提供方类型。
const (
	TypeBrave      = "brave"
	TypeBing       = "bing"
	TypeGoogle     = "google"
	TypeTavily     = "tavily"
	TypeSogou      = "sogou"
	TypeSerper     = "serper"
	TypeSearXNG    = "searxng"
	TypeJina       = "jina"
	TypeExa        = "exa"
	TypeBocha      = "bocha"
	TypeDuckDuckGo = "duckduckgo"
	TypeYandex     = "yandex"
)

// TypeInfo 是提供方类型的展示元数据（letter / color 给前端用）。
type TypeInfo struct {
	Label  string
	Letter string
	Color  string
}

var typeInfo = map[string]TypeInfo{
	TypeBrave:      {Label: "Brave", Letter: "B", Color: "#fb542b"},
	TypeBing:       {Label: "Bing", Letter: "b", Color: "#0078d4"},
	TypeGoogle:     {Label: "Google", Letter: "G", Color: "#4285f4"},
	TypeTavily:     {Label: "Tavily", Letter: "T", Color: "#3aa675"},
	TypeSogou:      {Label: "搜狗", Letter: "S", Color: "#fa5000"},
	TypeSerper:     {Label: "Serper", Letter: "S", Color: "#5468ff"},
	TypeSearXNG:    {Label: "SearXNG", Letter: "X", Color: "#3050ff"},
	TypeJina:       {Label: "Jina", Letter: "J", Color: "#e0245e"},
	TypeExa:        {Label: "Exa", Letter: "E", Color: "#1a73e8"},
	TypeBocha:      {Label: "博查", Letter: "B", Color: "#00a870"},
	TypeDuckDuckGo: {Label: "DuckDuckGo", Letter: "D", Color: "#de5833"},
	TypeYandex:     {Label: "Yandex", Letter: "Y", Color: "#fc3f1d"},
}

// TypeMeta 返回类型的展示名、字母和颜色；未知类型给一个占位。
func TypeMeta(t string) (label, letter, color string) {
	if m, ok := typeInfo[t]; ok {
		return m.Label, m.Letter, m.Color
	}
	l := "?"
	if len(t) > 0 {
		l = string([]rune(t)[0])
	}
	return t, l, "#888"
}

// KnownType 是否为已登记的提供方类型。
func KnownType(t string) bool {
	_, ok := typeInfo[t]
	return ok
}

// timeoutOrDefault 返回提供方超时，缺省 15 秒（与 Memoh 一致）。
func (p Provider) timeoutOrDefault() time.Duration {
	if p.Timeout > 0 {
		return time.Duration(p.Timeout) * time.Second
	}
	return 15 * time.Second
}
