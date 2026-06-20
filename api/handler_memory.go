package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 记忆查询 Handler — 只读访问 Bot 的分层记忆（admin）
// ============================================================================

// handleQueryMemory 查询指定 Bot 的分层记忆。
// GET /api/bots/:id/memory?tier=L1&scope=user:xxx&limit=20
//
// tier: L0（工作记忆）、L1（长期）、L2（场景）、L3（画像），默认全部
// scope: 作用域过滤（如 "channel:general"），可选
//
// @Summary      查询记忆
// @Description  查询指定 Bot 的分层记忆（需要 bot.manage 权限，需开启 dreaming）
// @Tags         记忆
// @Produce      json
// @Param        id     path      string  true   "Bot ID"
// @Param        tier   query     string  false  "记忆层级 (L0/L1/L2/L3)"
// @Param        limit  query     int     false  "返回条数"  default(20)
// @Success      200    {object}  Response
// @Failure      404    {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/memory [get]
func (s *Server) handleQueryMemory(c *gin.Context) {
	botID := c.Param("id")

	bundle, ok := s.botSvc.GetDreamingBundle(botID)
	if !ok {
		Fail(c, errs.NotFound("dreaming not enabled for this bot — enable dreaming to access memory"))
		return
	}

	ctx := c.Request.Context()
	tierStr := c.DefaultQuery("tier", "")
	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	// 解析 tier
	var tier memory.MemoryTier
	switch tierStr {
	case "L0", "l0":
		tier = memory.Tier0Working
	case "L1", "l1":
		tier = memory.Tier1LongTerm
	case "L2", "l2":
		tier = memory.Tier2Episodic
	case "L3", "l3":
		tier = memory.Tier3Profile
	}

	mgr := bundle.TieredMgr
	if mgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
	}

	// RetrieveMerged / RetrieveByTier 不需要 scope 过滤（scope 在内部处理）
	var entries []memory.TieredEntry
	var err error

	if tierStr == "" {
		entries, err = mgr.RetrieveMerged(ctx, nil, limit)
	} else {
		entries, err = mgr.RetrieveByTier(ctx, tier, nil, limit)
	}

	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query memory"))
		return
	}

	// 构建响应
	items := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		items = append(items, gin.H{
			"id":           e.ID,
			"content":      e.Content,
			"scope":        string(e.Scope.Kind) + ":" + e.Scope.ID,
			"tier":         e.Tier.String(),
			"category":     e.Category,
			"source":       e.Source,
			"importance":   e.Importance,
			"createdAt":    e.CreatedAt,
			"lastAccessed": e.LastAccessedAt,
		})
	}

	OK(c, gin.H{
		"entries": items,
		"total":   len(items),
		"tier":    tierStr,
	})
}

// handleMemoryStats 记忆统计信息。
// GET /api/bots/:id/memory/stats
//
// @Summary      记忆统计
// @Description  返回指定 Bot 的记忆统计信息
// @Tags         记忆
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Failure      404  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/memory/stats [get]
func (s *Server) handleMemoryStats(c *gin.Context) {
	botID := c.Param("id")

	bundle, ok := s.botSvc.GetDreamingBundle(botID)
	if !ok {
		Fail(c, errs.NotFound("dreaming not enabled for this bot"))
		return
	}

	mgr := bundle.TieredMgr
	if mgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
	}

	ctx := c.Request.Context()

	// 统计 L1 条目数（global scope）
	count, err := mgr.Aggregate(ctx, memory.Scope{Kind: memory.ScopeGlobal})
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to count memory"))
		return
	}

	// 估算 L2 条目数
	l2Entries, _ := mgr.RetrieveByTier(ctx, memory.Tier2Episodic, nil, 10000)

	OK(c, gin.H{
		"l1Count":    count,
		"l2Estimate": len(l2Entries),
	})
}

// handleTriggerDreaming 手动触发梦境巩固。
// POST /api/bots/:id/dreaming/trigger
//
// @Summary      触发梦境巩固
// @Description  手动触发指定 Bot 的梦境巩固流程
// @Tags         梦境巩固
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Failure      404  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/dreaming/trigger [post]
func (s *Server) handleTriggerDreaming(c *gin.Context) {
	botID := c.Param("id")

	bundle, ok := s.botSvc.GetDreamingBundle(botID)
	if !ok {
		Fail(c, errs.NotFound("dreaming not enabled for this bot"))
		return
	}

	if bundle.Manager == nil {
		Fail(c, errs.Internal("dream manager not initialized"))
		return
	}

	report, err := bundle.Manager.Run(c.Request.Context())
	if err != nil {
		Fail(c, errs.Wrap(err, "dreaming trigger failed"))
		return
	}

	auditLog(c, s.logger, "trigger_dreaming", "bot_id", botID, "phase", report.Phase)

	OK(c, gin.H{
		"lightIngested": report.LightIngested,
		"lightDeduped":  report.LightDeduped,
		"lightDropped":  report.LightDropped,
		"remThemes":     report.REMThemes,
		"remCandidates": report.REMCandidates,
		"deepScored":    report.DeepScored,
		"deepPassed":    report.DeepPassed,
		"deepPromoted":  report.DeepPromoted,
		"duration":      report.Duration().String(),
		"phase":         report.Phase,
		"error":         report.Error,
	})
}

// handleDreamingStatus 梦境巩固运行时状态。
// GET /api/bots/:id/dreaming/status
//
// @Summary      梦境巩固状态
// @Description  返回指定 Bot 的梦境巩固运行时状态
// @Tags         梦境巩固
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/dreaming/status [get]
func (s *Server) handleDreamingStatus(c *gin.Context) {
	botID := c.Param("id")

	bundle, ok := s.botSvc.GetDreamingBundle(botID)
	if !ok {
		OK(c, gin.H{"enabled": false})
		return
	}

	status := gin.H{
		"enabled": true,
		"running": bundle.Manager != nil,
		"cronJob": nil,
	}

	if bundle.CronJob != nil {
		status["cronJob"] = gin.H{
			"id":              bundle.CronJob.ID,
			"name":            bundle.CronJob.Name,
			"schedule":        bundle.CronJob.Schedule,
			"scheduleDisplay": bundle.CronJob.ScheduleDisplay,
			"state":           bundle.CronJob.State,
			"nextRunAt":       bundle.CronJob.NextRunAt,
			"lastRunAt":       bundle.CronJob.LastRunAt,
			"lastResult":      bundle.CronJob.LastResult,
			"runCount":        bundle.CronJob.RunCount,
		}
	}

	if bundle.Scheduler != nil {
		status["schedulerSummary"] = bundle.Scheduler.Summary()
	}

	OK(c, status)
}
