package api

import (
	"os"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 系统监控 Handler — 健康检查 / 运行时信息 / 事件总线指标
// ============================================================================

var startTime = time.Now()

// handleHealthDetailed 详细健康检查（含运行时信息）。
// GET /health/detailed
func (s *Server) handleHealthDetailed(c *gin.Context) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	host, _ := os.Hostname()

	OK(c, gin.H{
		"status":     "ok",
		"host":       host,
		"uptime":     time.Since(startTime).String(),
		"uptimeSec":  int64(time.Since(startTime).Seconds()),
		"goroutines": runtime.NumGoroutine(),
		"memory": gin.H{
			"allocMB":      m.Alloc / 1024 / 1024,
			"totalAllocMB": m.TotalAlloc / 1024 / 1024,
			"sysMB":        m.Sys / 1024 / 1024,
			"gcCount":      m.NumGC,
		},
		"goVersion": runtime.Version(),
		"bots": gin.H{
			"running": s.botSvc.RunningCount(),
		},
	})
}

// handleEventBusMetrics 事件总线指标。
// GET /api/system/events/metrics
func (s *Server) handleEventBusMetrics(c *gin.Context) {
	bus := s.botSvc.EventBus()
	if bus == nil {
		OK(c, gin.H{"enabled": false})
		return
	}

	// MemoryEventBus 实现 Metrics() 方法
	type metricser interface {
		Metrics() any
		ActiveSubscriptions() int
		LatestSeq() uint64
	}

	if m, ok := bus.(metricser); ok {
		OK(c, gin.H{
			"enabled":             true,
			"activeSubscriptions": m.ActiveSubscriptions(),
			"latestSeq":           m.LatestSeq(),
			"metrics":             m.Metrics(),
		})
		return
	}

	OK(c, gin.H{"enabled": true})
}
