package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// LLM 模型管理 Handler — CRUD / 测试连接（admin）
// ============================================================================

// LLMModelResp LLM 模型配置响应（API Key 脱敏）。
type LLMModelResp struct {
	ID          string  `json:"id"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	APIKey      string  `json:"apiKey"`
	BaseURL     string  `json:"baseUrl"`
	ChatPath    string  `json:"chatPath,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Multimodal  bool    `json:"multimodal,omitempty"`
}

// handleListLLMModels 列出所有 LLM 模型配置。
// GET /api/llm/models
func (s *Server) handleListLLMModels(c *gin.Context) {
	builder := config.NewBuilder(s.store, s.logger)
	models := builder.GetAllLLMModels()

	result := make([]LLMModelResp, 0, len(models))
	for id, def := range models {
		temp := 0.7
		if def.Temperature != nil {
			temp = *def.Temperature
		}
		result = append(result, LLMModelResp{
			ID:          id,
			Provider:    def.Provider,
			Model:       def.Model,
			APIKey:      maskAPIKey(def.APIKey),
			BaseURL:     def.BaseURL,
			ChatPath:    def.ChatPath,
			Temperature: temp,
			MaxTokens:   def.MaxTokens,
			Multimodal:  def.Multimodal,
		})
	}

	OK(c, result)
}

// handleCreateLLMModel 创建 LLM 模型配置。
// POST /api/llm/models
func (s *Server) handleCreateLLMModel(c *gin.Context) {
	var req CreateLLMModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if req.ID == "" {
		Fail(c, errs.BadRequest("id is required"))
		return
	}

	key := config.LLMConfigKey(req.ID)

	// 检查是否已存在
	if existing, ok := s.store.Get(key); ok && existing != "" {
		Fail(c, errs.Conflict(fmt.Sprintf("LLM model '%s' already exists", req.ID)))
		return
	}

	def := config.ModelDef{
		Provider:   req.Provider,
		Model:      req.Model,
		APIKey:     req.APIKey,
		BaseURL:    req.BaseURL,
		ChatPath:   req.ChatPath,
		Multimodal: req.Multimodal,
	}
	if req.Temperature > 0 {
		t := req.Temperature
		def.Temperature = &t
	}
	if req.MaxTokens > 0 {
		def.MaxTokens = req.MaxTokens
	}

	jsonBytes, err := json.Marshal(def)
	if err != nil {
		Fail(c, errs.Wrap(err, "marshal LLM config"))
		return
	}

	if err := s.store.Set(c.Request.Context(), key, string(jsonBytes)); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "create_llm_model", "id", req.ID, "provider", req.Provider, "model", req.Model)
	OK(c, LLMModelResp{
		ID:         req.ID,
		Provider:   def.Provider,
		Model:      def.Model,
		APIKey:     maskAPIKey(def.APIKey),
		BaseURL:    def.BaseURL,
		ChatPath:   def.ChatPath,
		Multimodal: def.Multimodal,
	})
}

// handleUpdateLLMModel 更新 LLM 模型配置。
// PUT /api/llm/models/:id
func (s *Server) handleUpdateLLMModel(c *gin.Context) {
	id := c.Param("id")
	key := config.LLMConfigKey(id)

	// 读取现有配置
	builder := config.NewBuilder(s.store, s.logger)
	existing, ok := builder.GetLLMModel(id)
	if !ok {
		Fail(c, errs.NotFound(fmt.Sprintf("LLM model '%s' not found", id)))
		return
	}

	var req UpdateLLMModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 合并更新
	if req.Provider != nil {
		existing.Provider = *req.Provider
	}
	if req.Model != nil {
		existing.Model = *req.Model
	}
	if req.APIKey != nil && *req.APIKey != "" {
		existing.APIKey = *req.APIKey
	}
	if req.BaseURL != nil {
		existing.BaseURL = *req.BaseURL
	}
	if req.ChatPath != nil {
		existing.ChatPath = *req.ChatPath
	}
	if req.Temperature != nil {
		existing.Temperature = req.Temperature
	}
	if req.MaxTokens != nil {
		existing.MaxTokens = *req.MaxTokens
	}
	if req.Multimodal != nil {
		existing.Multimodal = *req.Multimodal
	}

	jsonBytes, err := json.Marshal(existing)
	if err != nil {
		Fail(c, errs.Wrap(err, "marshal LLM config"))
		return
	}

	if err := s.store.Set(c.Request.Context(), key, string(jsonBytes)); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "update_llm_model", "id", id)
	OKMsg(c, "LLM model updated", nil)
}

// handleDeleteLLMModel 删除 LLM 模型配置。
// DELETE /api/llm/models/:id
func (s *Server) handleDeleteLLMModel(c *gin.Context) {
	id := c.Param("id")
	key := config.LLMConfigKey(id)

	// 检查是否存在
	if _, ok := s.store.Get(key); !ok {
		Fail(c, errs.NotFound(fmt.Sprintf("LLM model '%s' not found", id)))
		return
	}

	// 检查是否有 Bot 正在使用此模型
	defs, err := s.botSvc.ListDefinitions()
	if err != nil {
		Fail(c, err)
		return
	}
	for _, def := range defs {
		if def.LLMMain == id || def.LLMLight == id {
			Fail(c, errs.BadRequest(fmt.Sprintf("LLM model '%s' is in use by bot '%s', please reassign first", id, def.Name)))
			return
		}
	}

	if err := s.store.Set(c.Request.Context(), key, ""); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "delete_llm_model", "id", id)
	OKMsg(c, "LLM model deleted", nil)
}

// maskAPIKey 脱敏 API Key，仅显示前 6 位和后 4 位。
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 12 {
		return strings.Repeat("*", len(key))
	}
	return key[:6] + "..." + key[len(key)-4:]
}
