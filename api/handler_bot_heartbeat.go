package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/heartbeat"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot 心跳管理 Handler
//
// 每个 Bot 独立管理心跳配置和日志，存储在 data/heartbeat/{botId}/。
// heartbeatStore 已迁移为 Server 字段，由 NewServer 注入，与 NewBundle 共享同一数据目录。
//
// 路由：
//   GET    /api/bots/:id/heartbeat       → 获取心跳配置
//   PUT    /api/bots/:id/heartbeat       → 更新心跳配置
//   GET    /api/bots/:id/heartbeat/logs  → 查询心跳日志
//   DELETE /api/bots/:id/heartbeat/logs  → 清空心跳日志
// ============================================================================

// handleGetHeartbeatConfig 获取 Bot 心跳配置。
func (s *Server) handleGetHeartbeatConfig(c *gin.Context) {
	botID := c.Param("id")

	cfg, err := s.heartbeatStore.LoadConfig(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load heartbeat config"))
		return
	}
	if cfg == nil {
		// 首次访问，返回默认配置
		cfg = &heartbeat.Config{Enabled: true, Interval: 30}
	}
	OK(c, cfg)
}

// handleUpdateHeartbeatConfig 更新 Bot 心跳配置。
func (s *Server) handleUpdateHeartbeatConfig(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Enabled  *bool `json:"enabled"`
		Interval *int  `json:"interval"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}

	// 加载现有配置
	cfg, err := s.heartbeatStore.LoadConfig(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load heartbeat config"))
		return
	}
	if cfg == nil {
		cfg = &heartbeat.Config{Enabled: true, Interval: 30}
	}

	// 部分更新
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.Interval != nil {
		interval := *req.Interval
		if interval < 1 {
			interval = 1
		}
		if interval > 1440 {
			interval = 1440
		}
		cfg.Interval = interval
	}

	// 保存
	if err := s.heartbeatStore.SaveConfig(botID, cfg); err != nil {
		Fail(c, errs.Wrap(err, "save heartbeat config"))
		return
	}

	auditLog(c, s.logger, "update_heartbeat_config", "bot_id", botID,
		"enabled", cfg.Enabled, "interval", cfg.Interval)
	OK(c, cfg)
}

// handleListHeartbeatLogs 查询心跳日志。
// Query params: status=all|normal|alert
func (s *Server) handleListHeartbeatLogs(c *gin.Context) {
	botID := c.Param("id")
	status := c.DefaultQuery("status", "all")

	logStore, err := s.heartbeatStore.LoadLogs(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load heartbeat logs"))
		return
	}

	allLogs := logStore.Logs
	totalHistory := len(allLogs) // 历史总数（滚动窗口内）

	logs := allLogs
	if status != "" && status != "all" {
		filtered := make([]heartbeat.Log, 0, len(allLogs))
		for _, l := range allLogs {
			if l.Status == status {
				filtered = append(filtered, l)
			}
		}
		logs = filtered
	}

	OK(c, gin.H{
		"logs":  logs,
		"total": totalHistory, // 历史总数
		"count": len(logs),    // 当前过滤后条数
	})
}

// handleClearHeartbeatLogs 清空心跳日志。
func (s *Server) handleClearHeartbeatLogs(c *gin.Context) {
	botID := c.Param("id")

	if err := s.heartbeatStore.ClearLogs(botID); err != nil {
		Fail(c, errs.Wrap(err, "clear heartbeat logs"))
		return
	}

	auditLog(c, s.logger, "clear_heartbeat_logs", "bot_id", botID)
	OK(c, nil)
}
