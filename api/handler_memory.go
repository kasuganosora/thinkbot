package api

import (
	"strconv"
	"strings"

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
	tierStr := c.DefaultQuery("tier", "")
	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	bundle, ok := s.botSvc.GetDreamingBundle(botID)
	if !ok {
		// 未启用梦境巩固时分层存储不存在：返回空列表而非报错，由前端显示引导态。
		OK(c, gin.H{"entries": []gin.H{}, "total": 0, "tier": tierStr, "enabled": false})
		return
	}

	ctx := c.Request.Context()

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

	// 说明：RetrieveMerged(ctx, nil, ...) 在 scope 为 nil 时会因 for-range 空切片而返回 0 条，
	// 故「全部层级」改用按层级逐个 RetrieveByTier（nil scope 在 store 内等价于「全部作用域」）。
	var entries []memory.TieredEntry
	var err error

	if tierStr == "" {
		// 展示顺序：先排长期/场景/画像等「耐久」记忆（更有管理价值），
		// 工作记忆(L0)量大且易变，放最后填充。受总 limit 约束，
		// 各层均不被整层吞掉；要专门看 L0 可用 tier=L0 过滤。
		allTiers := []memory.MemoryTier{
			memory.Tier1LongTerm, memory.Tier2Episodic,
			memory.Tier3Profile, memory.Tier0Working,
		}
		remaining := limit
		for _, t := range allTiers {
			if remaining <= 0 {
				break
			}
			part, e := mgr.RetrieveByTier(ctx, t, nil, remaining)
			if e != nil {
				err = e
				break
			}
			entries = append(entries, part...)
			remaining -= len(part)
		}
	} else {
		entries, err = mgr.RetrieveByTier(ctx, tier, nil, limit)
	}

	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query memory"))
		return
	}

	// 构建响应（TieredManager 自动记忆）
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
		"enabled": true,
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
		OK(c, gin.H{"l1Count": 0, "l2Estimate": 0, "enabled": false})
		return
	}

	mgr := bundle.TieredMgr
	if mgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
	}

	ctx := c.Request.Context()

	// 统计 L1 条目数（全 scope）。Aggregate 需要配置 Aggregator，未配置时会报错，
	// 故改用 RetrieveByTier 直接计数，与下方 L2 估算口径一致、且不依赖 Aggregator。
	l1Entries, l1Err := mgr.RetrieveByTier(ctx, memory.Tier1LongTerm, nil, 10000)
	l1Count := 0
	if l1Err == nil {
		l1Count = len(l1Entries)
	}

	// 估算 L2 条目数
	l2Entries, _ := mgr.RetrieveByTier(ctx, memory.Tier2Episodic, nil, 10000)

	OK(c, gin.H{
		"l1Count":    l1Count,
		"l2Estimate": len(l2Entries),
		"enabled":    true,
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
		// 调试友好：Bot 未启动时也允许按需构建 bundle 触发，无需先 start。
		s.logger.Infow("dreaming trigger: bot not running, building bundle on demand", "bot_id", botID)
		var berr error
		bundle, berr = s.botSvc.BuildDreamingBundleOnDemand(botID)
		if berr != nil {
			Fail(c, errs.Wrap(berr, "failed to build dreaming bundle on demand"))
			return
		}
		if bundle == nil {
			Fail(c, errs.NotFound("dreaming not enabled for this bot"))
			return
		}
		defer bundle.Stop()
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

	// 调试辅助：解释 ingested=0 的常见原因，便于快速定位问题。
	message := ""
	switch {
	case report.Error != "":
		message = report.Error
	case report.LightIngested == 0 && report.SkippedInactive > 0:
		message = "所有 scope 因超过活跃阈值被跳过（需有近期 L0 写入才会处理）"
	case report.LightIngested == 0 && report.LightDeduped > 0:
		message = "本轮没有新 L0 候选，已复用此前分期的候选进入 REM/Deep"
	case report.LightIngested == 0:
		message = "没有可巩固的 L0 工作记忆（L0 为空或历史均已处理）"
	}

	// 附带梦境日记尾部，便于排查管线内部行为。
	diary := bundle.Manager.DreamDiary()
	diaryTail := diary
	if len(diary) > 12 {
		diaryTail = diary[len(diary)-12:]
	}

	OK(c, gin.H{
		"lightIngested":   report.LightIngested,
		"lightDeduped":    report.LightDeduped,
		"lightDropped":    report.LightDropped,
		"remThemes":       report.REMThemes,
		"remCandidates":   report.REMCandidates,
		"deepScored":      report.DeepScored,
		"deepPassed":      report.DeepPassed,
		"deepPromoted":    report.DeepPromoted,
		"skippedInactive": report.SkippedInactive,
		"duration":        report.Duration().String(),
		"phase":           report.Phase,
		"error":           report.Error,
		"message":         message,
		"dreamDiary":      diaryTail,
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

// ============================================================================
// 分层记忆条目删除 Handler（admin）
// ============================================================================

// handleDeleteTieredMemoryEntry 删除指定 Bot 的一条分层记忆条目。
// DELETE /api/bots/:id/memory/entry?id=<entryID>&tier=<Lx_xxx>&scope=<kind:id>
//
// 记忆按 tier+scope 分桶存储，删除必须同时提供这三项才能精确定位。
// 仅删除单条记忆（如错误的 fact / 脏数据），不影响其他层级与巩固管线。
func (s *Server) handleDeleteTieredMemoryEntry(c *gin.Context) {
	botID := c.Param("id")
	id := c.Query("id")
	tierStr := c.Query("tier")
	scopeStr := c.Query("scope")
	if id == "" || tierStr == "" || scopeStr == "" {
		Fail(c, errs.BadRequest("id, tier and scope are all required"))
		return
	}

	var tier memory.MemoryTier
	switch tierStr {
	case memory.Tier0Working.String():
		tier = memory.Tier0Working
	case memory.Tier1LongTerm.String():
		tier = memory.Tier1LongTerm
	case memory.Tier2Episodic.String():
		tier = memory.Tier2Episodic
	case memory.Tier3Profile.String():
		tier = memory.Tier3Profile
	default:
		Fail(c, errs.BadRequest("invalid tier: "+tierStr))
		return
	}

	var scope memory.Scope
	if i := strings.Index(scopeStr, ":"); i >= 0 {
		scope = memory.Scope{Kind: memory.ScopeKind(scopeStr[:i]), ID: scopeStr[i+1:]}
	} else {
		scope = memory.Scope{Kind: memory.ScopeKind(scopeStr)}
	}

	bundle, ok := s.botSvc.GetDreamingBundle(botID)
	if !ok {
		// bot 可能已停止但 dreaming 已配置：按需构建 bundle 以触达分层存储。
		var berr error
		bundle, berr = s.botSvc.BuildDreamingBundleOnDemand(botID)
		if berr != nil {
			Fail(c, errs.Wrap(berr, "failed to build dreaming bundle on demand"))
			return
		}
		if bundle == nil {
			Fail(c, errs.NotFound("dreaming not enabled for this bot"))
			return
		}
		defer bundle.Stop()
	}

	store := bundle.TieredMgr.Store()
	if store == nil {
		Fail(c, errs.Internal("memory store not initialized"))
		return
	}

	if err := store.Delete(c.Request.Context(), tier, scope, id); err != nil {
		Fail(c, errs.Wrap(err, "failed to delete memory entry"))
		return
	}

	auditLog(c, s.logger, "delete_memory_entry", "bot_id", botID, "tier", tierStr, "scope", scopeStr, "entry_id", id)
	OK(c, gin.H{"deleted": id})
}
