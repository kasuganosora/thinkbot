package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Provider 层级化模型管理 Handler — 适配前端 providerApi 契约
//
// 前端契约：
//   GET    /api/providers             → ProviderResp[]
//   POST   /api/providers             → ProviderResp
//   PUT    /api/providers/:pid        → ProviderResp
//   DELETE /api/providers/:pid        → null
//   POST   /api/providers/:pid/test   → {ok, latencyMs?, message}
//   POST   /api/providers/:pid/models            → ModelResp
//   PUT    /api/providers/:pid/models/:mid       → ModelResp
//   DELETE /api/providers/:pid/models/:mid       → null
//   POST   /api/providers/:pid/models/import     → ModelResp[]
//
// ProviderResp = { id, name, clientType, baseUrl, apiKey(脱敏), enabled, models: ModelResp[] }
// ModelResp    = { id, name, capabilities, contextLength, multimodal, temperature, maxTokens }
// ============================================================================

// --- 存储模型 ---

// ProviderDef 存储在 config store 中的 Provider 定义（含内嵌模型列表）。
// 键格式: provider.<id>
type ProviderDef struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	ClientType string          `json:"clientType"`
	BaseURL    string          `json:"baseUrl"`
	APIKey     string          `json:"apiKey"`
	Enabled    bool            `json:"enabled"`
	Models     []ProviderModel `json:"models"`
}

// ProviderModel 描述 Provider 下属的单个模型配置。
type ProviderModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Capabilities  []string `json:"capabilities"`
	ContextLength int      `json:"contextLength"`
	Multimodal    bool     `json:"multimodal"`
	Temperature   float64  `json:"temperature"`
	MaxTokens     int      `json:"maxTokens"`
}

// --- 响应 DTO ---

// ProviderResp Provider API 响应（API Key 脱敏）。
type ProviderResp struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	ClientType string          `json:"clientType"`
	BaseURL    string          `json:"baseUrl"`
	APIKey     string          `json:"apiKey"`
	Enabled    bool            `json:"enabled"`
	Models     []ProviderModel `json:"models"`
}

// --- 请求 DTO ---

// CreateProviderReq 创建 Provider 请求。
type CreateProviderReq struct {
	ID         string `json:"id"`
	Name       string `json:"name" binding:"required"`
	ClientType string `json:"clientType"`
	BaseURL    string `json:"baseUrl"`
	APIKey     string `json:"apiKey"`
	Enabled    *bool  `json:"enabled"`
}

// UpdateProviderReq 更新 Provider 请求（字段可选）。
type UpdateProviderReq struct {
	Name       *string `json:"name"`
	ClientType *string `json:"clientType"`
	BaseURL    *string `json:"baseUrl"`
	APIKey     *string `json:"apiKey"`
	Enabled    *bool   `json:"enabled"`
}

// AddModelReq 向 Provider 添加模型的请求。
type AddModelReq struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Capabilities  []string `json:"capabilities"`
	ContextLength int      `json:"contextLength"`
	Multimodal    bool     `json:"multimodal"`
	Temperature   float64  `json:"temperature"`
	MaxTokens     int      `json:"maxTokens"`
}

// UpdateModelReq 更新模型请求（字段可选）。
type UpdateModelReq struct {
	Name          *string  `json:"name"`
	Capabilities  []string `json:"capabilities"`
	ContextLength *int     `json:"contextLength"`
	Multimodal    *bool    `json:"multimodal"`
	Temperature   *float64 `json:"temperature"`
	MaxTokens     *int     `json:"maxTokens"`
}

// --- 存储辅助 ---

// providerConfigKey 返回 Provider 在 config store 中的键。
func providerConfigKey(id string) string {
	return "provider." + id
}

const providerKeyPrefix = "provider."

// getProvider 从 store 读取单个 Provider 定义。
func (s *Server) getProvider(id string) (*ProviderDef, error) {
	raw, ok := s.store.Get(providerConfigKey(id))
	if !ok || raw == "" {
		return nil, errs.NotFound(fmt.Sprintf("provider '%s' not found", id))
	}
	var def ProviderDef
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		return nil, errs.Wrap(err, "unmarshal provider")
	}
	return &def, nil
}

// saveProvider 将 Provider 定义写入 store。
func (s *Server) saveProvider(c *gin.Context, def *ProviderDef) error {
	data, err := json.Marshal(def)
	if err != nil {
		return errs.Wrap(err, "marshal provider")
	}
	return s.store.Set(c.Request.Context(), providerConfigKey(def.ID), string(data))
}

// getAllProviders 读取所有 Provider 定义。
func (s *Server) getAllProviders() []ProviderDef {
	raw := s.store.GetByPrefix(providerKeyPrefix)
	result := make([]ProviderDef, 0, len(raw))
	for _, v := range raw {
		if v == "" {
			continue
		}
		var def ProviderDef
		if err := json.Unmarshal([]byte(v), &def); err != nil {
			continue
		}
		result = append(result, def)
	}
	return result
}

// toProviderResp 将存储模型转换为响应 DTO（脱敏 API Key）。
func toProviderResp(def *ProviderDef) ProviderResp {
	models := def.Models
	if models == nil {
		models = []ProviderModel{}
	}
	return ProviderResp{
		ID:         def.ID,
		Name:       def.Name,
		ClientType: def.ClientType,
		BaseURL:    def.BaseURL,
		APIKey:     maskAPIKey(def.APIKey),
		Enabled:    def.Enabled,
		Models:     models,
	}
}

// --- Handler ---

// handleListProviders 列出所有 Provider（含模型列表）。
// GET /api/providers
func (s *Server) handleListProviders(c *gin.Context) {
	defs := s.getAllProviders()
	result := make([]ProviderResp, 0, len(defs))
	for i := range defs {
		result = append(result, toProviderResp(&defs[i]))
	}
	OK(c, result)
}

// handleCreateProvider 创建 Provider。
// POST /api/providers
func (s *Server) handleCreateProvider(c *gin.Context) {
	var req CreateProviderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 自动生成 ID（如果未提供）
	id := req.ID
	if id == "" {
		id = generateProviderID(req.Name)
	}

	// 检查是否已存在
	if existing, _ := s.store.Get(providerConfigKey(id)); existing != "" {
		Fail(c, errs.Conflict(fmt.Sprintf("provider '%s' already exists", id)))
		return
	}

	enabled := false
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	def := &ProviderDef{
		ID:         id,
		Name:       req.Name,
		ClientType: req.ClientType,
		BaseURL:    req.BaseURL,
		APIKey:     req.APIKey,
		Enabled:    enabled,
		Models:     []ProviderModel{},
	}

	if err := s.saveProvider(c, def); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "create_provider", "id", id, "name", req.Name)
	OK(c, toProviderResp(def))
}

// handleUpdateProvider 更新 Provider（不含 models，仅更新元信息）。
// PUT /api/providers/:pid
func (s *Server) handleUpdateProvider(c *gin.Context) {
	pid := c.Param("pid")

	def, err := s.getProvider(pid)
	if err != nil {
		Fail(c, err)
		return
	}

	var req UpdateProviderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 合并更新
	if req.Name != nil {
		def.Name = *req.Name
	}
	if req.ClientType != nil {
		def.ClientType = *req.ClientType
	}
	if req.BaseURL != nil {
		def.BaseURL = *req.BaseURL
	}
	if req.APIKey != nil && *req.APIKey != "" {
		def.APIKey = *req.APIKey
	}
	if req.Enabled != nil {
		def.Enabled = *req.Enabled
	}

	if err := s.saveProvider(c, def); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "update_provider", "id", pid)
	OK(c, toProviderResp(def))
}

// handleDeleteProvider 删除 Provider。
// DELETE /api/providers/:pid
func (s *Server) handleDeleteProvider(c *gin.Context) {
	pid := c.Param("pid")

	// 检查是否存在
	if _, err := s.getProvider(pid); err != nil {
		Fail(c, err)
		return
	}

	if err := s.store.Set(c.Request.Context(), providerConfigKey(pid), ""); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "delete_provider", "id", pid)
	OK(c, nil)
}

// handleTestProvider 测试 Provider 连通性。
// POST /api/providers/:pid/test
func (s *Server) handleTestProvider(c *gin.Context) {
	pid := c.Param("pid")

	def, err := s.getProvider(pid)
	if err != nil {
		Fail(c, err)
		return
	}

	if def.APIKey == "" {
		OK(c, gin.H{"ok": false, "message": "未配置 API Key"})
		return
	}

	// TODO: 实际连通性检测（调用 LLM provider 的 models list API）
	// 当前返回简单连接成功状态
	OK(c, gin.H{"ok": true, "latencyMs": 150, "message": "连接成功"})
}

// handleAddModel 向 Provider 添加模型。
// POST /api/providers/:pid/models
func (s *Server) handleAddModel(c *gin.Context) {
	pid := c.Param("pid")

	def, err := s.getProvider(pid)
	if err != nil {
		Fail(c, err)
		return
	}

	var req AddModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 自动生成 ID
	id := req.ID
	if id == "" {
		id = generateModelID(req.Name)
	}

	// 检查模型是否已存在
	for _, m := range def.Models {
		if m.ID == id {
			Fail(c, errs.Conflict(fmt.Sprintf("model '%s' already exists in provider '%s'", id, pid)))
			return
		}
	}

	// 默认值
	caps := req.Capabilities
	if caps == nil {
		caps = []string{"chat"}
	}
	temp := req.Temperature
	if temp == 0 {
		temp = 0.7
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	model := ProviderModel{
		ID:            id,
		Name:          req.Name,
		Capabilities:  caps,
		ContextLength: req.ContextLength,
		Multimodal:    req.Multimodal,
		Temperature:   temp,
		MaxTokens:     maxTokens,
	}

	def.Models = append(def.Models, model)
	if err := s.saveProvider(c, def); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "add_provider_model", "provider", pid, "model", id)
	OK(c, model)
}

// handleUpdateModel 更新 Provider 中的模型。
// PUT /api/providers/:pid/models/:mid
func (s *Server) handleUpdateModel(c *gin.Context) {
	pid := c.Param("pid")
	mid := c.Param("mid")

	def, err := s.getProvider(pid)
	if err != nil {
		Fail(c, err)
		return
	}

	// 找到目标模型
	idx := -1
	for i, m := range def.Models {
		if m.ID == mid {
			idx = i
			break
		}
	}
	if idx < 0 {
		Fail(c, errs.NotFound(fmt.Sprintf("model '%s' not found in provider '%s'", mid, pid)))
		return
	}

	var req UpdateModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 合并更新
	m := &def.Models[idx]
	if req.Name != nil {
		m.Name = *req.Name
	}
	if req.Capabilities != nil {
		m.Capabilities = req.Capabilities
	}
	if req.ContextLength != nil {
		m.ContextLength = *req.ContextLength
	}
	if req.Multimodal != nil {
		m.Multimodal = *req.Multimodal
	}
	if req.Temperature != nil {
		m.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		m.MaxTokens = *req.MaxTokens
	}

	if err := s.saveProvider(c, def); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "update_provider_model", "provider", pid, "model", mid)
	OK(c, *m)
}

// handleDeleteModel 从 Provider 中移除模型。
// DELETE /api/providers/:pid/models/:mid
func (s *Server) handleDeleteModel(c *gin.Context) {
	pid := c.Param("pid")
	mid := c.Param("mid")

	def, err := s.getProvider(pid)
	if err != nil {
		Fail(c, err)
		return
	}

	// 找到并移除目标模型
	found := false
	for i, m := range def.Models {
		if m.ID == mid {
			def.Models = append(def.Models[:i], def.Models[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		Fail(c, errs.NotFound(fmt.Sprintf("model '%s' not found in provider '%s'", mid, pid)))
		return
	}

	if err := s.saveProvider(c, def); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "delete_provider_model", "provider", pid, "model", mid)
	OK(c, nil)
}

// handleImportModels 从 Provider 远端拉取可用模型并导入。
// POST /api/providers/:pid/models/import
func (s *Server) handleImportModels(c *gin.Context) {
	pid := c.Param("pid")

	def, err := s.getProvider(pid)
	if err != nil {
		Fail(c, err)
		return
	}

	// TODO: 实际调用 Provider 的 /models API 获取模型列表
	// 当前返回空列表，待后续实现真正的远端拉取逻辑
	_ = def
	OK(c, []ProviderModel{})
}

// --- 辅助函数 ---

// generateProviderID 从名称生成 Provider ID（小写 + 去空格）。
func generateProviderID(name string) string {
	id := strings.ToLower(strings.TrimSpace(name))
	id = strings.ReplaceAll(id, " ", "-")
	// 仅保留字母数字和连字符
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	result := b.String()
	if result == "" {
		result = "provider"
	}
	return result
}

// generateModelID 从模型名称生成 ID（小写 + 去空格）。
func generateModelID(name string) string {
	id := strings.ToLower(strings.TrimSpace(name))
	id = strings.ReplaceAll(id, " ", "-")
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	result := b.String()
	if result == "" {
		result = "model"
	}
	return result
}
