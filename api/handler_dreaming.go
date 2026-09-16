package api

import (
	"encoding/json"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 梦境巩固配置 Handler — 读取 / 设置（admin）
// ============================================================================

// handleGetDreamingConfig 获取指定 Bot 的梦境巩固配置。
// GET /api/bots/:id/dreaming
//
// @Summary      获取梦境配置
// @Description  获取指定 Bot 的梦境巩固配置
// @Tags         梦境巩固
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/dreaming [get]
func (s *Server) handleGetDreamingConfig(c *gin.Context) {
	botID := c.Param("id")

	builder := config.NewBuilder(s.store, s.logger)
	cfg := builder.GetDreamingConfig(botID)

	OK(c, DreamingConfigResp{
		Enabled:  cfg.Enabled,
		Schedule: cfg.Schedule,
	})
}

// handleUpdateDreamingConfig 更新指定 Bot 的梦境巩固配置。
// PUT /api/bots/:id/dreaming
//
// 请求体（字段可选）：
//
//	{"enabled": true, "schedule": "0 3 * * *"}
//
// 注意：修改配置后需要重启 Bot 才能生效。
//
// @Summary      更新梦境配置
// @Description  更新指定 Bot 的梦境巩固配置（字段可选）
// @Tags         梦境巩固
// @Accept       json
// @Produce      json
// @Param        id    path      string                  true  "Bot ID"
// @Param        body  body      UpdateDreamingConfigReq true  "更新梦境配置请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/dreaming [put]
func (s *Server) handleUpdateDreamingConfig(c *gin.Context) {
	botID := c.Param("id")

	var req UpdateDreamingConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 读取当前配置，合并更新
	builder := config.NewBuilder(s.store, s.logger)
	cfg := builder.GetDreamingConfig(botID)

	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.Schedule != nil {
		if *req.Schedule == "" {
			Fail(c, errs.BadRequest("schedule must not be empty"))
			return
		}
		cfg.Schedule = *req.Schedule
	}

	if err := builder.SetDreamingConfig(c.Request.Context(), botID, cfg); err != nil {
		Fail(c, errs.Wrap(err, "failed to set dreaming config"))
		return
	}

	auditLog(c, s.logger, "update_dreaming_config", "bot_id", botID, "enabled", cfg.Enabled, "schedule", cfg.Schedule)

	OKMsg(c, "dreaming config updated, restart bot to take effect", DreamingConfigResp{
		Enabled:  cfg.Enabled,
		Schedule: cfg.Schedule,
	})
}

// ============================================================================
// 梦境晋升明细 Handler — 列出最近被 dreaming 提升为 L1 的记忆
// ============================================================================

// handleListDreamPromotions 列出该 Bot 最近被梦境巩固提升的长期记忆（L1，source="dreaming"）。
// GET /api/bots/:id/dreaming/promotions?limit=20
//
// 每条含：内容、分类、范围、得分，以及固化在 metadata 中的「提升理由」(dream_reason)
// 与「引用的原 L0 条目 ID」(source_entry_ids)。前端「最近晋升的记忆」面板据此展示
// 每次提升的证据链（为什么被记住、引用了哪些原始记录）。
//
// @Summary      列出梦境晋升明细
// @Description  列出该 Bot 最近被梦境巩固提升的长期记忆及其理由与原条目引用
// @Tags         梦境巩固
// @Produce      json
// @Param        id    path  string true  "Bot ID"
// @Param        limit query int    false "返回条数" default(20)
// @Success      200   {object} Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/dreaming/promotions [get]
func (s *Server) handleListDreamPromotions(c *gin.Context) {
	botID := c.Param("id")
	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	bundle, stop, berr := s.memoryBundle(botID)
	if berr != nil {
		Fail(c, errs.Wrap(berr, "failed to open memory store"))
		return
	}
	if bundle == nil {
		// 未启用梦境巩固：返回空列表（与 handleQueryMemory 一致）。
		OK(c, gin.H{"promotions": []gin.H{}, "total": 0, "enabled": false})
		return
	}
	defer stop()

	mgr := bundle.TieredMgr
	if mgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
	}

	ctx := c.Request.Context()
	// 取 L1 全量（按时间倒序），再筛选 source="dreaming" 取最近 limit 条。
	// 注：内存桶每 scope 有上限（L1=500），单 bot 的 dreaming 晋升通常远小于此；
	// 与 handleQueryMemory 一致走 RetrieveByTier（内存态）。如需重启后仍全量可见，
	// 应改直查 SQLite tiered_memories 全表（见 memory 运维 gotcha）。
	entries, err := mgr.RetrieveByTier(ctx, memory.Tier1LongTerm, nil, 500)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to list L1"))
		return
	}

	items := make([]gin.H, 0, limit)
	for _, e := range entries {
		if e.Source != "dreaming" {
			continue
		}
		items = append(items, gin.H{
			"id":             e.ID,
			"content":        e.Content,
			"category":       e.Category,
			"scope":          string(e.Scope.Kind) + ":" + e.Scope.ID,
			"score":          e.Importance,
			"reason":         parsePromotionReason(e.Metadata),
			"source_ids":     parseSourceIDs(e.Metadata),
			"source_entries": parseSourceEntries(e.Metadata),
			"createdAt":      e.CreatedAt,
		})
		if len(items) >= limit {
			break
		}
	}

	OK(c, gin.H{
		"promotions": items,
		"total":      len(items),
		"enabled":    true,
	})
}

// parsePromotionReason 从 L1 条目 metadata 中解析固化的「提升理由」。
// metadata["dream_reason"] 可能是：
//   - memory.DreamPromotionReason 结构体（内存态，刚写入、未经历 SQLite 往返）
//   - map[string]any（SQLite 回读后的通用映射）
//
// 两者都需正确解析，否则前端拿不到理由。
func parsePromotionReason(md map[string]any) memory.DreamPromotionReason {
	var reason memory.DreamPromotionReason
	if md == nil {
		return reason
	}
	switch v := md["dream_reason"].(type) {
	case memory.DreamPromotionReason:
		return v
	case map[string]any:
		if b, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(b, &reason)
		}
	}
	return reason
}

// parseSourceIDs 从 L1 条目 metadata 中解析「引用的原 L0 条目 ID」。
// 经 JSON 往返后 []string 会变成 []any，需兼容两种形态。
func parseSourceIDs(md map[string]any) []string {
	var out []string
	if md == nil {
		return out
	}
	switch v := md["source_entry_ids"].(type) {
	case []string:
		return v
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// parseSourceEntries 从 L1 条目 metadata 中解析「引用的原 L0 条目内容快照」。
// metadata["source_entries"] 可能是：
//   - []memory.DreamSourceEntry（内存态，刚写入、未经历 SQLite 往返）
//   - []any（SQLite 回读后的通用切片，元素为 map[string]any）
//
// 两者都需正确解析，否则前端拿不到原内容。
func parseSourceEntries(md map[string]any) []memory.DreamSourceEntry {
	var out []memory.DreamSourceEntry
	if md == nil {
		return out
	}
	switch v := md["source_entries"].(type) {
	case []memory.DreamSourceEntry:
		return v
	case []any:
		for _, x := range v {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, memory.DreamSourceEntry{
				ID:      strOr(m, "id"),
				Content: strOr(m, "content"),
				Scope:   strOr(m, "scope"),
				Speaker: strOr(m, "speaker"),
			})
		}
	}
	return out
}

// strOr 从 map 中安全取字符串字段（缺失或非字符串时返回空串）。
func strOr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
