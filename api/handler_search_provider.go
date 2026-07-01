package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// 搜索提供方管理 Handler
//
// 管理系统级别的搜索引擎提供方配置。
// 数据存储在 data/search/providers.json。
//
// 路由：
//   GET    /api/search/providers              → 列表
//   POST   /api/search/providers              → 新增
//   PUT    /api/search/providers/:id          → 更新
//   DELETE /api/search/providers/:id          → 删除
//   PUT    /api/search/providers/:id/toggle   → 切换启用/禁用
// ============================================================================

// searchProviderEntry 搜索提供方实体。
type searchProviderEntry struct {
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

// searchProvidersFile 返回搜索提供方配置文件路径。
func searchProvidersFile() string {
	return filepath.Join("data", "search", "providers.json")
}

// loadSearchProviders 从文件加载搜索提供方列表。
func loadSearchProviders() ([]searchProviderEntry, error) {
	path := searchProvidersFile()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var providers []searchProviderEntry
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, err
	}
	return providers, nil
}

// saveSearchProviders 保存搜索提供方列表。
func saveSearchProviders(providers []searchProviderEntry) error {
	path := searchProvidersFile()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// searchProviderTypeMeta 返回搜索类型的默认 letter 和 color。
var searchProviderTypeMap = map[string]struct {
	Label  string
	Letter string
	Color  string
}{
	"brave":      {"Brave", "B", "#fb542b"},
	"bing":       {"Bing", "b", "#0078d4"},
	"google":     {"Google", "G", "#4285f4"},
	"tavily":     {"Tavily", "T", "#3aa675"},
	"sogou":      {"搜狗", "S", "#fa5000"},
	"serper":     {"Serper", "S", "#5468ff"},
	"searxng":    {"SearXNG", "X", "#3050ff"},
	"jina":       {"Jina", "J", "#e0245e"},
	"exa":        {"Exa", "E", "#1a73e8"},
	"bocha":      {"博查", "B", "#00a870"},
	"duckduckgo": {"DuckDuckGo", "D", "#de5833"},
	"yandex":     {"Yandex", "Y", "#fc3f1d"},
}

func getSearchTypeMeta(t string) (label, letter, color string) {
	if m, ok := searchProviderTypeMap[t]; ok {
		return m.Label, m.Letter, m.Color
	}
	l := "?"
	if len(t) > 0 {
		l = string([]rune(t)[0])
	}
	return t, l, "#888"
}

// handleListSearchProviders 列出所有搜索提供方。
func (s *Server) handleListSearchProviders(c *gin.Context) {
	providers, err := loadSearchProviders()
	if err != nil {
		Fail(c, errs.Wrap(err, "load search providers"))
		return
	}
	if providers == nil {
		providers = []searchProviderEntry{}
	}
	OK(c, gin.H{"providers": providers})
}

// handleCreateSearchProvider 创建搜索提供方。
func (s *Server) handleCreateSearchProvider(c *gin.Context) {
	var req struct {
		Type       string `json:"type"`
		Name       string `json:"name"`
		APIKey     string `json:"apiKey"`
		SearchType string `json:"searchType"`
		Timeout    int    `json:"timeout"`
		BaseURL    string `json:"baseUrl"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}
	if req.Type == "" {
		Fail(c, errs.BadRequest("type is required"))
		return
	}

	providers, _ := loadSearchProviders()

	label, letter, color := getSearchTypeMeta(req.Type)
	name := req.Name
	if name == "" {
		name = label
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 15
	}

	now := time.Now().UTC().Format(time.RFC3339)
	entry := searchProviderEntry{
		ID:         idgen.New("sp"),
		Type:       req.Type,
		Name:       name,
		Letter:     letter,
		Color:      color,
		Enabled:    false,
		APIKey:     req.APIKey,
		SearchType: req.SearchType,
		Timeout:    timeout,
		BaseURL:    req.BaseURL,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	providers = append(providers, entry)
	if err := saveSearchProviders(providers); err != nil {
		Fail(c, errs.Wrap(err, "save search providers"))
		return
	}

	auditLog(c, s.logger, "create_search_provider", "type", req.Type, "name", name)
	OK(c, entry)
}

// handleUpdateSearchProvider 更新搜索提供方。
func (s *Server) handleUpdateSearchProvider(c *gin.Context) {
	id := c.Param("id")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}

	providers, err := loadSearchProviders()
	if err != nil {
		Fail(c, errs.Wrap(err, "load search providers"))
		return
	}

	idx := -1
	for i, p := range providers {
		if p.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		Fail(c, errs.NotFound("search provider not found"))
		return
	}

	p := &providers[idx]
	if v, ok := req["name"].(string); ok {
		p.Name = v
	}
	if v, ok := req["apiKey"].(string); ok {
		p.APIKey = v
	}
	if v, ok := req["searchType"].(string); ok {
		p.SearchType = v
	}
	if v, ok := req["timeout"]; ok {
		switch t := v.(type) {
		case float64:
			p.Timeout = int(t)
		}
	}
	if v, ok := req["baseUrl"].(string); ok {
		p.BaseURL = v
	}
	if v, ok := req["enabled"].(bool); ok {
		p.Enabled = v
	}
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := saveSearchProviders(providers); err != nil {
		Fail(c, errs.Wrap(err, "save search providers"))
		return
	}

	auditLog(c, s.logger, "update_search_provider", "id", id)
	OK(c, *p)
}

// handleToggleSearchProvider 切换搜索提供方的启用/禁用。
func (s *Server) handleToggleSearchProvider(c *gin.Context) {
	id := c.Param("id")

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}

	providers, err := loadSearchProviders()
	if err != nil {
		Fail(c, errs.Wrap(err, "load search providers"))
		return
	}

	idx := -1
	for i, p := range providers {
		if p.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		Fail(c, errs.NotFound("search provider not found"))
		return
	}

	providers[idx].Enabled = req.Enabled
	providers[idx].UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := saveSearchProviders(providers); err != nil {
		Fail(c, errs.Wrap(err, "save search providers"))
		return
	}

	auditLog(c, s.logger, "toggle_search_provider", "id", id, "enabled", fmt.Sprintf("%v", req.Enabled))
	OK(c, providers[idx])
}

// handleRemoveSearchProvider 删除搜索提供方。
func (s *Server) handleRemoveSearchProvider(c *gin.Context) {
	id := c.Param("id")

	providers, err := loadSearchProviders()
	if err != nil {
		Fail(c, errs.Wrap(err, "load search providers"))
		return
	}

	found := false
	for i, p := range providers {
		if p.ID == id {
			providers = append(providers[:i], providers[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		Fail(c, errs.NotFound("search provider not found"))
		return
	}

	if err := saveSearchProviders(providers); err != nil {
		Fail(c, errs.Wrap(err, "save search providers"))
		return
	}

	auditLog(c, s.logger, "remove_search_provider", "id", id)
	OK(c, nil)
}
