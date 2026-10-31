package api

import (
	"context"

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
		// 敏感键在列表中仅以掩码展示，避免 JWT 密钥/渠道 token 经列表接口泄露。
		if config.IsSensitiveKey(spec.Key) {
			val = "***"
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

	// 敏感键（JWT 密钥、渠道 token、DB 凭据等）禁止经读取接口原文回显，
	// 防止密钥经 GET /api/config/:key 泄露。
	if config.IsSensitiveKey(key) {
		Fail(c, errs.Forbidden("config key is sensitive and cannot be read via API"))
		return
	}

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

	// 沙箱 CPU 上限先校验再落库：非法值会被原样拼进 docker --cpus，真正的报错
	// 要等到创建/更新容器时才出现，且埋在 docker 输出里难以归因到配置项。
	if key == config.KeySandboxCPULimit {
		if err := validateSandboxCPULimit(req.Value); err != nil {
			Fail(c, err)
			return
		}
	}

	if err := s.store.Set(c.Request.Context(), key, req.Value); err != nil {
		Fail(c, errs.Wrap(err, "failed to set config"))
		return
	}
	auditLog(c, s.logger, "set_config", "key", key)
	// 沙箱 CPU 上限改完就地应用到运行中的 bot 容器，不必等重建。
	if key == config.KeySandboxCPULimit {
		if needsRecreate := s.applySandboxCPULimit(c.Request.Context()); needsRecreate {
			OKMsg(c, "config updated（已保存；「不限制」需重建 bot 容器后才生效）", nil)
			return
		}
	}
	OKMsg(c, "config updated", nil)
}

// applySandboxCPULimit 把新写入的 sandbox.cpu_limit 就地应用到所有 bot 容器。
// 失败只记日志：配置已经落库，容器重建时一样会生效。
// 返回 true 表示设成了「不限制」，docker update 取消不掉，需要重建容器。
func (s *Server) applySandboxCPULimit(ctx context.Context) bool {
	if s.botSvc == nil {
		return false
	}
	return s.botSvc.ApplySandboxCPULimitAll(ctx)
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

	// 与单条设置一致：批量写入前先校验沙箱 CPU 上限，避免整批落库后才发现非法值。
	if v, ok := req.Items[config.KeySandboxCPULimit]; ok {
		if err := validateSandboxCPULimit(v); err != nil {
			Fail(c, err)
			return
		}
	}

	if err := s.store.SetMany(c.Request.Context(), req.Items); err != nil {
		Fail(c, errs.Wrap(err, "failed to batch set config"))
		return
	}
	auditLog(c, s.logger, "batch_set_config", "keys", strutil.MapKeys(req.Items))
	if _, ok := req.Items[config.KeySandboxCPULimit]; ok {
		if needsRecreate := s.applySandboxCPULimit(c.Request.Context()); needsRecreate {
			OKMsg(c, "config batch updated（沙箱 CPU 设为不限制，需重建 bot 容器后才生效）", nil)
			return
		}
	}
	OKMsg(c, "config batch updated", nil)
}
