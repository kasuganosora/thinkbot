package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// 系统配置 Handler — 读取 / 设置（admin）
// ============================================================================

// handleGetConfig 列出所有配置项（带描述和分类）。
// GET /api/config
func (s *Server) handleGetConfig(c *gin.Context) {
	items, err := s.store.ListSettings(c.Request.Context())
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to load config"))
		return
	}
	OK(c, items)
}

// handleGetConfigKey 获取单个配置项。
// GET /api/config/:key
func (s *Server) handleGetConfigKey(c *gin.Context) {
	key := c.Param("key")

	val, ok := s.store.Get(key)
	if !ok {
		Fail(c, errs.NotFound("config key not found"))
		return
	}
	OK(c, gin.H{"key": key, "value": val})
}

// handleSetConfigKey 设置单个配置项。
// PUT /api/config/:key
func (s *Server) handleSetConfigKey(c *gin.Context) {
	key := c.Param("key")

	var req SetConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.store.Set(c.Request.Context(), key, req.Value); err != nil {
		Fail(c, errs.Wrap(err, "failed to set config"))
		return
	}
	auditLog(c, s.logger, "set_config", "key", key)
	OKMsg(c, "config updated", nil)
}

// handleBatchSetConfig 批量设置配置项。
// PUT /api/config
func (s *Server) handleBatchSetConfig(c *gin.Context) {
	var req BatchSetConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.store.SetMany(c.Request.Context(), req.Items); err != nil {
		Fail(c, errs.Wrap(err, "failed to batch set config"))
		return
	}
	auditLog(c, s.logger, "batch_set_config", "keys", strutil.MapKeys(req.Items))
	OKMsg(c, "config batch updated", nil)
}
