package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// 系统配置 Handler — 读取 / 设置（admin）
// ============================================================================

// handleGetConfig 列出所有配置项（带描述和分类）。
// GET /api/config
//
// @Summary      获取所有配置
// @Description  列出所有系统配置项（需要 system.config 权限）
// @Tags         系统配置
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/config [get]
func (s *Server) handleGetConfig(c *gin.Context) {
	// 从 DB 读取用户已保存的值（可能为空）
	dbItems, err := s.store.ListSettings(c.Request.Context())
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to load config"))
		return
	}
	dbMap := make(map[string]config.Setting, len(dbItems))
	for _, item := range dbItems {
		dbMap[item.Key] = item
	}

	// 以 GlobalMetaSpecs() 定义为基准，合并 DB 值 + 默认值
	specs := config.GlobalMetaSpecs()
	defaults := config.DefaultMap()

	items := make([]config.Setting, 0, len(specs))
	for _, spec := range specs {
		val := ""
		if dbItem, ok := dbMap[spec.Key]; ok && dbItem.Value != "" {
			val = dbItem.Value
		} else if dv, ok := defaults[spec.Key]; ok {
			val = dv
		}
		items = append(items, config.Setting{
			Key:         spec.Key,
			Value:       val,
			Category:    spec.Category,
			Description: spec.Description,
		})
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
//
// @Summary      设置配置项
// @Description  设置单个系统配置项的值
// @Tags         系统配置
// @Accept       json
// @Produce      json
// @Param        key   path      string       true  "配置键"
// @Param        body  body      SetConfigReq true  "配置值请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/config/{key} [put]
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
//
// @Summary      批量设置配置
// @Description  批量设置多个系统配置项
// @Tags         系统配置
// @Accept       json
// @Produce      json
// @Param        body  body      BatchSetConfigReq  true  "批量配置请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/config [put]
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
