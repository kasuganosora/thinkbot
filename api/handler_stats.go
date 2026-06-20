package api

import (
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
