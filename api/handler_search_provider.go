package api

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/internal/searchproviders"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// 搜索提供方配置 Handler
//
// 管理系统级的搜索提供方配置。
// 数据存储在 data/search/providers.json。
// web_search 工具在每次调用时读取此文件中已启用的提供方。
//
// 路径：
//   GET    /api/search/providers              → 列表
//   POST   /api/search/providers              → 新增
//   PUT    /api/search/providers/:id          → 更新
//   DELETE /api/search/providers/:id          → 删除
//   PUT    /api/search/providers/:id/toggle   → 切换启用/禁用
// ============================================================================

type searchProviderEntry = searchproviders.Provider

func loadSearchProviders() ([]searchProviderEntry, error) {
	return searchproviders.DefaultStore().List()
}

func saveSearchProviders(providers []searchProviderEntry) error {
	return searchproviders.DefaultStore().Save(providers)
}

func getSearchTypeMeta(t string) (label, letter, color string) {
	return searchproviders.TypeMeta(t)
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

// handleCreateSearchProvider 新增搜索提供方。
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

// handleToggleSearchProvider 切换搜索提供方启用/禁用。
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
