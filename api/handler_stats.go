package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/stats"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 统计数据 Handler — 汇总 / Bot 维度 / 按日（admin）
// ============================================================================

// handleStatsOverview 全平台统计概览。
// GET /api/stats/overview?from=2024-01-01&to=2024-12-31
//
// @Summary      统计概览
// @Description  全平台模型用量统计概览（需要 user.manage 权限）
// @Tags         统计
// @Produce      json
// @Param        from  query     string  false  "起始日期 (YYYY-MM-DD)"
// @Param        to    query     string  false  "结束日期 (YYYY-MM-DD)"
// @Success      200   {object}  Response
// @Security     CookieAuth
// @Router       /api/stats/overview [get]
func (s *Server) handleStatsOverview(c *gin.Context) {
	from, to := parseDateRange(c)

	result, err := stats.GetAllBotsModelStats(s.db, from, to)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query stats"))
		return
	}
	OK(c, result)
}

// handleStatsBot 单个 Bot 的模型统计。
// GET /api/stats/bots/:id?from=2024-01-01&to=2024-12-31
//
// @Summary      Bot 统计
// @Description  查询单个 Bot 的模型用量统计
// @Tags         统计
// @Produce      json
// @Param        id    path      string  true   "Bot ID"
// @Param        from  query     string  false  "起始日期 (YYYY-MM-DD)"
// @Param        to    query     string  false  "结束日期 (YYYY-MM-DD)"
// @Success      200   {object}  Response
// @Security     CookieAuth
// @Router       /api/stats/bots/{id} [get]
func (s *Server) handleStatsBot(c *gin.Context) {
	botID := c.Param("id")
	from, to := parseDateRange(c)

	result, err := stats.GetBotModelStats(s.db, botID, from, to)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query bot stats"))
		return
	}
	OK(c, result)
}

// handleStatsBotDaily 单个 Bot 的按日统计。
// GET /api/stats/bots/:id/daily?from=2024-01-01&to=2024-12-31
//
// @Summary      按日统计
// @Description  查询单个 Bot 的按日模型用量统计
// @Tags         统计
// @Produce      json
// @Param        id    path      string  true   "Bot ID"
// @Param        from  query     string  false  "起始日期 (YYYY-MM-DD)"
// @Param        to    query     string  false  "结束日期 (YYYY-MM-DD)"
// @Success      200   {object}  Response
// @Security     CookieAuth
// @Router       /api/stats/bots/{id}/daily [get]
func (s *Server) handleStatsBotDaily(c *gin.Context) {
	botID := c.Param("id")
	from, to := parseDateRange(c)

	result, err := stats.GetDailyStats(s.db, botID, from, to)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query daily stats"))
		return
	}
	OK(c, result)
}

// parseDateRange 从查询参数解析日期范围。
func parseDateRange(c *gin.Context) (from, to *time.Time) {
	if v := c.Query("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			from = &t
		}
	}
	if v := c.Query("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			to = &t
		}
	}
	return
}

// handleStatsDailyRange 全局按日统计序列（可选 botId 过滤）。
// GET /api/stats/daily?from=2024-01-01&to=2024-12-31&botId=assistant
func (s *Server) handleStatsDailyRange(c *gin.Context) {
	from, to := parseDateRange(c)
	botID := c.Query("botId")

	result, err := stats.GetDailyStatsGlobal(s.db, botID, from, to)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query daily stats"))
		return
	}
	OK(c, result)
}

// handleStatsDailyByBot 按日×各 Bot 的 token 使用量（堆叠图表用）。
// GET /api/stats/daily-by-bot?from=2024-01-01&to=2024-12-31
//
// 响应格式：{ bots: [{id, name}], series: [{ date, usage: { <botId>: tokens } }] }
func (s *Server) handleStatsDailyByBot(c *gin.Context) {
	from, to := parseDateRange(c)

	entries, err := stats.GetDailyByBotStats(s.db, from, to)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query daily-by-bot stats"))
		return
	}

	// 收集所有出现的 bot ID
	botSet := make(map[string]bool)
	for _, e := range entries {
		botSet[e.BotID] = true
	}

	// 构造 bots 列表（尝试从 BotService 获取名称）
	type BotInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	bots := make([]BotInfo, 0, len(botSet))
	for bid := range botSet {
		name := bid
		if def, err := s.botSvc.GetDefinition(bid); err == nil && def != nil {
			name = def.Name
		}
		bots = append(bots, BotInfo{ID: bid, Name: name})
	}

	// 构造按日分组的 series
	type SeriesEntry struct {
		Date  string         `json:"date"`
		Usage map[string]int `json:"usage"`
	}
	dateMap := make(map[string]*SeriesEntry)
	for _, e := range entries {
		dateStr := e.Date.UTC().Format("2006-01-02T00:00:00Z")
		se, ok := dateMap[dateStr]
		if !ok {
			se = &SeriesEntry{Date: dateStr, Usage: make(map[string]int)}
			dateMap[dateStr] = se
		}
		se.Usage[e.BotID] = e.Tokens
	}

	// 转换为有序切片
	series := make([]SeriesEntry, 0, len(dateMap))
	for _, se := range dateMap {
		series = append(series, *se)
	}

	OK(c, gin.H{"bots": bots, "series": series})
}

// handleStatsRecords 调用流水明细（分页）。
// GET /api/stats/records?from&to&page&pageSize&botId
func (s *Server) handleStatsRecords(c *gin.Context) {
	from, to := parseDateRange(c)
	botID := c.Query("botId")

	page := 1
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := 20
	if v := c.Query("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			pageSize = n
		}
	}

	items, total, err := stats.GetUsageRecords(s.db, botID, from, to, page, pageSize)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query usage records"))
		return
	}

	// 为每条记录补上 botName 和 provider
	type RecordWithExtra struct {
		stats.UsageRecord
		BotName  string `json:"botName"`
		Provider string `json:"provider"`
	}
	enriched := make([]RecordWithExtra, 0, len(items))
	for _, r := range items {
		name := r.BotID
		if def, err := s.botSvc.GetDefinition(r.BotID); err == nil && def != nil {
			name = def.Name
		}
		provider := providerOfModel(r.Model)
		enriched = append(enriched, RecordWithExtra{UsageRecord: r, BotName: name, Provider: provider})
	}

	OK(c, gin.H{"total": total, "page": page, "pageSize": pageSize, "items": enriched})
}

// handleStatsByBotModel Bot×Model 矩阵统计。
// GET /api/stats/by-bot-model?from&to
func (s *Server) handleStatsByBotModel(c *gin.Context) {
	from, to := parseDateRange(c)

	result, err := stats.GetAllBotsModelStats(s.db, from, to)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to query by-bot-model stats"))
		return
	}

	// 为每条记录补上 botName
	type BotModelStatWithName struct {
		stats.BotModelStat
		BotName string `json:"botName"`
	}
	enriched := make([]BotModelStatWithName, 0, len(result))
	for _, r := range result {
		name := r.BotID
		if def, err := s.botSvc.GetDefinition(r.BotID); err == nil && def != nil {
			name = def.Name
		}
		enriched = append(enriched, BotModelStatWithName{BotModelStat: r, BotName: name})
	}

	OK(c, enriched)
}

// providerOfModel 根据模型名推导供应商名称。
func providerOfModel(model string) string {
	switch {
	case strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4"):
		return "openai"
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gemini-"):
		return "google"
	case strings.HasPrefix(model, "grok-"):
		return "grok"
	case strings.HasPrefix(model, "deepseek-"):
		return "deepseek"
	case strings.HasPrefix(model, "qwen"):
		return "alibaba"
	default:
		return "unknown"
	}
}
