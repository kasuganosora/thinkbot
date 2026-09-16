package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// memoryBundle 返回该 Bot 的梦境 bundle。Bot 在跑时用现成的；否则按需构建（导入/查询
// 在 Bot 未启动时也能碰到 SQLite 里的分层记忆）。调用方必须 defer 返回的 stop。
// bundle==nil 且 err==nil 表示未启用梦境。
func (s *Server) memoryBundle(botID string) (*bot.DreamingBundle, func(), error) {
	nop := func() {}
	if s.botSvc == nil {
		return nil, nop, errs.Internal("bot service not initialized")
	}
	if bundle, ok := s.botSvc.GetDreamingBundle(botID); ok && bundle != nil {
		return bundle, nop, nil
	}
	bundle, err := s.botSvc.BuildDreamingBundleOnDemand(botID)
	if err != nil {
		return nil, nop, err
	}
	if bundle == nil {
		return nil, nop, nil
	}
	return bundle, bundle.Stop, nil
}

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

	bundle, stop, berr := s.memoryBundle(botID)
	if berr != nil {
		Fail(c, errs.Wrap(berr, "failed to open memory store"))
		return
	}
	if bundle == nil {
		// 未启用梦境巩固时分层存储不存在：返回空列表而非报错，由前端显示引导态。
		OK(c, gin.H{"entries": []gin.H{}, "total": 0, "tier": tierStr, "enabled": false})
		return
	}
	defer stop()

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

	bundle, stop, berr := s.memoryBundle(botID)
	if berr != nil {
		Fail(c, errs.Wrap(berr, "failed to open memory store"))
		return
	}
	if bundle == nil {
		OK(c, gin.H{"l1Count": 0, "l2Estimate": 0, "l3Count": 0, "enabled": false})
		return
	}
	defer stop()

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

	// 估算 L2 / L3 条目数
	l2Entries, _ := mgr.RetrieveByTier(ctx, memory.Tier2Episodic, nil, 10000)
	l3Entries, _ := mgr.RetrieveByTier(ctx, memory.Tier3Profile, nil, 10000)

	OK(c, gin.H{
		"l1Count":    l1Count,
		"l2Estimate": len(l2Entries),
		"l3Count":    len(l3Entries),
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
		"userProfiles":    report.UserProfiles,
		"botProfiles":     report.BotProfiles,
		"duration":        report.Duration().String(),
		"phase":           report.Phase,
		"error":           report.Error,
		"message":         message,
		"dreamDiary":      diaryTail,
		// 本轮实际晋升的明细（含理由与原条目引用），供前端即时回显。
		"promotions": report.Promotions,
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

	// 运行记录与 bundle 是否在跑无关：先直接读持久化文件，
	// 这样 bot 未启动时也能看到「上次运行/上次结果/累计次数」。
	lastRun := bot.NewDreamRunStore(dreamRunRecordPath(botID)).Load()

	var bundle *bot.DreamingBundle
	ok := false
	if s.botSvc != nil {
		bundle, ok = s.botSvc.GetDreamingBundle(botID)
	}
	if !ok {
		// bot 未运行：enabled 按持久化配置返回，避免前端把整块运行状态隐藏掉。
		cfg := config.NewBuilder(s.store, s.logger).GetDreamingConfig(botID)
		OK(c, gin.H{
			"enabled": cfg.Enabled,
			"running": false,
			"cronJob": nil,
			"lastRun": lastRun,
		})
		return
	}

	status := gin.H{
		"enabled": true,
		"running": bundle.Manager != nil,
		"cronJob": nil,
		"lastRun": lastRun,
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

	bundle, stop, berr := s.memoryBundle(botID)
	if berr != nil {
		Fail(c, errs.Wrap(berr, "failed to open memory store"))
		return
	}
	if bundle == nil {
		Fail(c, errs.NotFound("dreaming not enabled for this bot"))
		return
	}
	defer stop()
	if bundle.TieredMgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
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

// handleImportMemohMemory 从 Memoh 工作空间备份（.tar / .tar.gz）导入分层记忆。
// POST /api/bots/:id/memory/import  multipart field=file
//
// 只解析 memory/YYYY-MM-DD.md 与 PROFILES.md；心跳/空转/小时画像快照丢弃。
func (s *Server) handleImportMemohMemory(c *gin.Context) {
	botID := c.Param("id")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<20)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		Fail(c, errs.BadRequest("file is required: "+err.Error()))
		return
	}
	f, err := fileHeader.Open()
	if err != nil {
		Fail(c, errs.Internal("failed to open uploaded file: "+err.Error()))
		return
	}
	defer func() { _ = f.Close() }()

	bundle, stop, berr := s.memoryBundle(botID)
	if berr != nil {
		Fail(c, errs.Wrap(berr, "failed to open memory store"))
		return
	}
	if bundle == nil {
		Fail(c, errs.NotFound("dreaming not enabled for this bot"))
		return
	}
	defer stop()
	if bundle.TieredMgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
	}
	store := bundle.TieredMgr.Store()
	if store == nil {
		Fail(c, errs.Internal("memory store not initialized"))
		return
	}

	report, err := memory.ImportMemohArchive(c.Request.Context(), store, botID, f)
	if err != nil {
		Fail(c, errs.Wrap(err, "memoh import failed"))
		return
	}
	auditLog(c, s.logger, "import_memoh_memory", "bot_id", botID,
		"imported", report.Imported, "junk", report.SkippedJunk, "skipped", report.Skipped)
	OK(c, gin.H{
		"imported":    report.Imported,
		"skipped":     report.Skipped,
		"skippedJunk": report.SkippedJunk,
		"profiles":    report.Profiles,
		"errors":      report.Errors,
	})
}

// ============================================================================
// 琐碎内容清理（运维）— 清理历史存量里的垃圾短内容
// ============================================================================

// handleCleanupTrivialMemory 运维手段：扫描并清理不符合记忆标准的琐碎内容
// （<5 字符或 <5 词的短噪声，判定见 memory.IsTrivialMemoryContent）。
// POST /api/bots/:id/memory/cleanup-trivial
//
// body: { "tiers": ["L0","L1"], "dryRun": true }
//
//	tiers  : 目标层级，默认 ["L0","L1"]；可补 L2/L3，但默认不清（降低误删风险）
//	dryRun : true（默认）仅统计并取样返回，不删除；false 才真正删除并持久化
//
// 背景：新写入已在捕获层（note_capture）/ActionNote（MemoryWriteStage）/回灌（backfill）
// 被源头拦截，但规则生效前已落入 L0/L1 的短内容仍需一次性运维清理。
//
// 扫描范围是 SQLite 全量行（而不是内存存储）：TieredStore 的内存桶每 scope 有
// MaxEntries 上限（Tier0Working=200），内存里只是「最近的一部分」子集，
// 只扫内存会漏掉绝大多数历史存量（实测 L0 库里 2898 行 / 内存仅 430）。
// 删除：先清内存副本（store.Delete 同 scope 命中时会一并 persistDelete），
// 再用 SQL 按 id 全量删除兜底——因为 store.Delete 对「不在内存桶」的条目会直接返回、
// 不落库删除，历史存量只能靠 SQL 兜底。
func (s *Server) handleCleanupTrivialMemory(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Tiers  []string `json:"tiers"`
		DryRun *bool    `json:"dryRun"`
	}
	// body 允许为空：使用默认 tiers + dryRun=true。
	_ = c.ShouldBindJSON(&req)

	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	tierStrs := req.Tiers
	if len(tierStrs) == 0 {
		tierStrs = []string{"L0", "L1"}
	}

	targets := make([]memory.MemoryTier, 0, len(tierStrs))
	tierLabel := make(map[memory.MemoryTier]string, len(tierStrs))
	for _, ts := range tierStrs {
		var t memory.MemoryTier
		switch strings.ToUpper(ts) {
		case "L0":
			t = memory.Tier0Working
		case "L1":
			t = memory.Tier1LongTerm
		case "L2":
			t = memory.Tier2Episodic
		case "L3":
			t = memory.Tier3Profile
		default:
			continue
		}
		targets = append(targets, t)
		tierLabel[t] = strings.ToUpper(ts)
	}
	if len(targets) == 0 {
		Fail(c, errs.BadRequest("no valid tier specified (expected L0/L1/L2/L3)"))
		return
	}

	bundle, stop, berr := s.memoryBundle(botID)
	if berr != nil {
		Fail(c, errs.Wrap(berr, "failed to open memory store"))
		return
	}
	if bundle == nil {
		Fail(c, errs.NotFound("dreaming not enabled for this bot"))
		return
	}
	defer stop()
	mgr := bundle.TieredMgr
	if mgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
	}
	store := mgr.Store()
	if store == nil {
		Fail(c, errs.Internal("memory store not initialized"))
		return
	}
	if s.db == nil {
		Fail(c, errs.Internal("database not initialized"))
		return
	}

	ctx := c.Request.Context()

	// 以 SQLite 全量行为扫描基准（原因见函数头注释）：内存桶有 per-scope 上限，
	// 只看内存会漏掉大部分历史存量。
	tierInts := make([]int, 0, len(targets))
	for _, t := range targets {
		tierInts = append(tierInts, int(t))
	}

	var rows []struct {
		ID        string
		Tier      int
		ScopeKind string
		ScopeID   string
		Content   string
	}
	if err := s.db.WithContext(ctx).Table("tiered_memories").
		Select("id, tier, scope_kind, scope_id, content").
		Where("tier IN ?", tierInts).
		Find(&rows).Error; err != nil {
		Fail(c, errs.Wrap(err, "failed to scan memory rows"))
		return
	}

	type tierStat struct {
		Scanned int `json:"scanned"`
		Matched int `json:"matched"`
	}
	byTier := make(map[string]*tierStat, len(targets))
	for _, t := range targets {
		byTier[tierLabel[t]] = &tierStat{}
	}
	var sample []gin.H
	const maxSample = 25

	// hit 记录命中项，删除阶段需要 tier+scope 才能清对应的内存桶。
	type hit struct {
		ID    string
		Tier  memory.MemoryTier
		Scope memory.Scope
	}
	var hits []hit

	totalScanned, totalMatched, totalDeleted := 0, 0, 0

	for _, r := range rows {
		tier := memory.MemoryTier(r.Tier)
		label, ok := tierLabel[tier]
		if !ok {
			continue
		}
		byTier[label].Scanned++
		totalScanned++

		if !memory.IsTrivialMemoryContent(r.Content) {
			continue
		}
		byTier[label].Matched++
		totalMatched++
		hits = append(hits, hit{
			ID:    r.ID,
			Tier:  tier,
			Scope: memory.Scope{Kind: memory.ScopeKind(r.ScopeKind), ID: r.ScopeID},
		})

		if len(sample) < maxSample {
			content := r.Content
			if rc := []rune(content); len(rc) > 200 {
				content = string(rc[:200]) + "…"
			}
			sample = append(sample, gin.H{
				"id":      r.ID,
				"tier":    tier.String(),
				"scope":   r.ScopeKind + ":" + r.ScopeID,
				"content": content,
			})
		}
	}

	if !dryRun && len(hits) > 0 {
		// 1) 先清内存副本：命中且仍在内存桶中的条目走 store.Delete（内存 + 落库一次做完），
		//    避免内存残留被后续写路径复活；不在内存桶时该调用是 no-op。
		for _, h := range hits {
			if derr := store.Delete(ctx, h.Tier, h.Scope, h.ID); derr != nil {
				s.logger.Warnw("cleanup-trivial: failed to delete in-memory entry",
					"err", derr, "tier", h.Tier.String(), "id", h.ID)
			}
		}
		// 2) 再按 id 用 SQL 全量删除兜底：store.Delete 对「不在内存桶」的条目直接返回、
		//    不删库（见 TieredStore.Delete），只靠它清不掉历史存量。
		const chunk = 500
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		for start := 0; start < len(ids); start += chunk {
			end := start + chunk
			if end > len(ids) {
				end = len(ids)
			}
			res := s.db.WithContext(ctx).Table("tiered_memories").
				Where("id IN ?", ids[start:end]).
				Delete(&dao.TieredMemoryModel{})
			if res.Error != nil {
				s.logger.Errorw("cleanup-trivial: failed to delete memory rows",
					"err", res.Error, "bot_id", botID)
				continue
			}
			totalDeleted += int(res.RowsAffected)
		}
	}

	if !dryRun && totalDeleted > 0 {
		auditLog(c, s.logger, "cleanup_trivial_memory",
			"bot_id", botID, "deleted", totalDeleted, "tiers", strings.Join(tierStrs, ","))
		s.logger.Infow("cleanup-trivial: removed trivial memory entries",
			"bot_id", botID, "deleted", totalDeleted, "scanned", totalScanned)
	}

	OK(c, gin.H{
		"scanned": totalScanned,
		"matched": totalMatched,
		"deleted": totalDeleted,
		"dryRun":  dryRun,
		"byTier":  byTier,
		"sample":  sample,
	})
}

// ============================================================================
// 精确重复清理（运维）— 清理同一 (层级+范围+内容) 出现 ≥N 次的刷屏/重复条目
// ============================================================================

// handleCleanupDuplicateMemory 运维手段：清理「精确重复」的记忆条目。
// 同一 (tier, scope, content) 出现 >= minCount 次视为重复刷屏/spam，保留 1 条、删除其余。
// POST /api/bots/:id/memory/cleanup-duplicates
//
// body: { "tiers": ["L0","L1"], "minCount": 5, "dryRun": true }
//
//	tiers    : 目标层级，默认 ["L0","L1"]
//	minCount : 同一 (tier,scope,content) 出现次数阈值，默认 5（>=2 才有意义）
//	dryRun   : true（默认）仅统计并分组返回，不删除；false 才真正删除并持久化
//
// 背景：投票 bot / 心跳 bot 会把同一句话反复落库（如 "今日の迷路です！ #AiMaze" ×15、
// 推广链接 ×7），它们内容完全一致、属纯刷屏噪声。按「精确内容 + 同范围」分组计数，
// 既能命中刷屏，又不会误删不同用户在不同范围里各自说过的相同短句（如两人各说一次"谢谢"）。
//
// 去重语义：每组保留 1 条（最先写入的那条，按 id 升序取最小），其余删除——"去重"而非
// "全删"，避免把可能仍有参考价值的唯一副本也清掉。若确认是无价值 bot 垃圾，可整体调高
// 阈值后分多次清理，或直接用单条删除接口。
//
// 删除路径与 cleanup-trivial 一致：先 store.Delete 清内存副本（防复活），再 SQL 按 id
// 全量删除兜底（store.Delete 对「不在内存桶」的条目只返回不落库）。
func (s *Server) handleCleanupDuplicateMemory(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Tiers    []string `json:"tiers"`
		MinCount int      `json:"minCount"`
		DryRun   *bool    `json:"dryRun"`
	}
	// body 允许为空：使用默认 tiers + minCount=5 + dryRun=true。
	_ = c.ShouldBindJSON(&req)

	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	minCount := req.MinCount
	if minCount < 2 {
		// minCount<2 意味着"出现 1 次也算重复"，失去去重意义；强制下限 2。
		minCount = 5
	}

	tierStrs := req.Tiers
	if len(tierStrs) == 0 {
		tierStrs = []string{"L0", "L1"}
	}
	targets := make([]memory.MemoryTier, 0, len(tierStrs))
	tierLabel := make(map[memory.MemoryTier]string, len(tierStrs))
	for _, ts := range tierStrs {
		var t memory.MemoryTier
		switch strings.ToUpper(ts) {
		case "L0":
			t = memory.Tier0Working
		case "L1":
			t = memory.Tier1LongTerm
		case "L2":
			t = memory.Tier2Episodic
		case "L3":
			t = memory.Tier3Profile
		default:
			continue
		}
		targets = append(targets, t)
		tierLabel[t] = strings.ToUpper(ts)
	}
	if len(targets) == 0 {
		Fail(c, errs.BadRequest("no valid tier specified (expected L0/L1/L2/L3)"))
		return
	}

	bundle, stop, berr := s.memoryBundle(botID)
	if berr != nil {
		Fail(c, errs.Wrap(berr, "failed to open memory store"))
		return
	}
	if bundle == nil {
		Fail(c, errs.NotFound("dreaming not enabled for this bot"))
		return
	}
	defer stop()
	mgr := bundle.TieredMgr
	if mgr == nil {
		Fail(c, errs.Internal("memory manager not initialized"))
		return
	}
	store := mgr.Store()
	if store == nil {
		Fail(c, errs.Internal("memory store not initialized"))
		return
	}
	if s.db == nil {
		Fail(c, errs.Internal("database not initialized"))
		return
	}

	ctx := c.Request.Context()

	// 以 SQLite 全量行为扫描基准（同 cleanup-trivial）：内存桶有 per-scope 上限，
	// 只看内存会漏掉绝大多数历史存量。
	tierInts := make([]int, 0, len(targets))
	for _, t := range targets {
		tierInts = append(tierInts, int(t))
	}

	var rows []struct {
		ID        string
		Tier      int
		ScopeKind string
		ScopeID   string
		Content   string
	}
	if err := s.db.WithContext(ctx).Table("tiered_memories").
		Select("id, tier, scope_kind, scope_id, content").
		Where("tier IN ?", tierInts).
		Order("id ASC").
		Find(&rows).Error; err != nil {
		Fail(c, errs.Wrap(err, "failed to scan memory rows"))
		return
	}

	type tierStat struct {
		Scanned int `json:"scanned"`
		Matched int `json:"matched"` // 该层级将被删除的重复行数
	}
	byTier := make(map[string]*tierStat, len(targets))
	for _, t := range targets {
		byTier[tierLabel[t]] = &tierStat{}
	}

	// 按 (tier, scope_kind, scope_id, content) 精确分组。
	type groupKey struct {
		Tier      int
		ScopeKind string
		ScopeID   string
		Content   string
	}
	type groupRow struct {
		ID    string
		Tier  int
		Scope string
	}
	groups := make(map[groupKey][]groupRow)
	for _, r := range rows {
		label, ok := tierLabel[memory.MemoryTier(r.Tier)]
		if !ok {
			continue
		}
		byTier[label].Scanned++
		k := groupKey{r.Tier, r.ScopeKind, r.ScopeID, r.Content}
		groups[k] = append(groups[k], groupRow{
			ID:    r.ID,
			Tier:  r.Tier,
			Scope: r.ScopeKind + ":" + r.ScopeID,
		})
	}

	type dupGroup struct {
		Content string
		Tier    memory.MemoryTier
		Scope   memory.Scope
		Count   int
		KeepID  string
		DelIDs  []string
	}
	var dupGroups []dupGroup
	totalScanned := len(rows)
	totalMatched := 0 // 将被删除的重复行总数（每组保留 1，其余计入）
	totalGroups := 0

	for k, rs := range groups {
		if len(rs) < minCount {
			continue
		}
		totalGroups++
		keep := rs[0] // Order("id ASC") → 最先写入（最小 id）的那条保留
		del := make([]string, 0, len(rs)-1)
		for i := 1; i < len(rs); i++ {
			del = append(del, rs[i].ID)
		}
		label := tierLabel[memory.MemoryTier(k.Tier)]
		byTier[label].Matched += len(del)
		totalMatched += len(del)
		dupGroups = append(dupGroups, dupGroup{
			Content: k.Content,
			Tier:    memory.MemoryTier(k.Tier),
			Scope:   memory.Scope{Kind: memory.ScopeKind(k.ScopeKind), ID: k.ScopeID},
			Count:   len(rs),
			KeepID:  keep.ID,
			DelIDs:  del,
		})
	}

	// 预览分组采样（截断内容），上限 25 组。
	const maxGroups = 25
	type groupSample struct {
		Content string `json:"content"`
		Tier    string `json:"tier"`
		Scope   string `json:"scope"`
		Count   int    `json:"count"`
		KeepID  string `json:"keepId"`
	}
	var groupSamples []groupSample
	for _, g := range dupGroups {
		if len(groupSamples) >= maxGroups {
			break
		}
		content := g.Content
		if rc := []rune(content); len(rc) > 200 {
			content = string(rc[:200]) + "…"
		}
		groupSamples = append(groupSamples, groupSample{
			Content: content,
			Tier:    g.Tier.String(),
			Scope:   string(g.Scope.Kind) + ":" + g.Scope.ID,
			Count:   g.Count,
			KeepID:  g.KeepID,
		})
	}

	totalDeleted := 0
	if !dryRun && totalMatched > 0 {
		// 收集所有待删 id + 对应的 (tier, scope) 以便清内存桶。
		type delHit struct {
			ID    string
			Tier  memory.MemoryTier
			Scope memory.Scope
		}
		hits := make([]delHit, 0, totalMatched)
		allIDs := make([]string, 0, totalMatched)
		for _, g := range dupGroups {
			for _, id := range g.DelIDs {
				hits = append(hits, delHit{ID: id, Tier: g.Tier, Scope: g.Scope})
				allIDs = append(allIDs, id)
			}
		}
		// 1) 先清内存副本：命中且仍在内存桶的走 store.Delete（内存 + 落库一次做完），
		//    否则该调用是 no-op。
		for _, h := range hits {
			if derr := store.Delete(ctx, h.Tier, h.Scope, h.ID); derr != nil {
				s.logger.Warnw("cleanup-duplicate: failed to delete in-memory entry",
					"err", derr, "tier", h.Tier.String(), "id", h.ID)
			}
		}
		// 2) 再按 id 用 SQL 全量删除兜底。
		const chunk = 500
		for start := 0; start < len(allIDs); start += chunk {
			end := start + chunk
			if end > len(allIDs) {
				end = len(allIDs)
			}
			res := s.db.WithContext(ctx).Table("tiered_memories").
				Where("id IN ?", allIDs[start:end]).
				Delete(&dao.TieredMemoryModel{})
			if res.Error != nil {
				s.logger.Errorw("cleanup-duplicate: failed to delete memory rows",
					"err", res.Error, "bot_id", botID)
				continue
			}
			totalDeleted += int(res.RowsAffected)
		}
		if totalDeleted > 0 {
			auditLog(c, s.logger, "cleanup_duplicate_memory",
				"bot_id", botID, "deleted", totalDeleted, "groups", totalGroups, "minCount", minCount, "tiers", strings.Join(tierStrs, ","))
			s.logger.Infow("cleanup-duplicate: removed duplicate memory entries",
				"bot_id", botID, "deleted", totalDeleted, "groups", totalGroups, "minCount", minCount)
		}
	}

	OK(c, gin.H{
		"scanned":         totalScanned,
		"duplicateGroups": totalGroups,
		"matched":         totalMatched,
		"toDelete":        totalMatched,
		"deleted":         totalDeleted,
		"dryRun":          dryRun,
		"minCount":        minCount,
		"byTier":          byTier,
		"groups":          groupSamples,
	})
}
