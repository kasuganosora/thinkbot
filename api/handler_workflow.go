package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 工作流监控 Handler — 只读查询 / 崩溃恢复（admin）
//
// 工作流的创建（Submit）、流程控制（retry/terminate）由 Agent 通过
// task / task_control 工具完成，不通过 REST API 暴露。
// 终止操作由 session 生命周期信号触发，连通 pipeline 一起终止。
// ============================================================================

// handleListWorkflows 列出最近的工作流。
// GET /api/workflows?limit=20
func (s *Server) handleListWorkflows(c *gin.Context) {
	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	workflows := mgr.ListWorkflows(limit)
	items := make([]gin.H, 0, len(workflows))
	for _, wf := range workflows {
		items = append(items, gin.H{
			"id":          wf.ID,
			"status":      wf.Status,
			"requirement": wf.Requirement,
			"nodeCount":   len(wf.Nodes),
			"createdAt":   wf.CreatedAt,
		})
	}

	OK(c, gin.H{"workflows": items, "total": len(items)})
}

// handleGetWorkflowStatus 查询工作流状态。
// GET /api/workflows/:wfId
func (s *Server) handleGetWorkflowStatus(c *gin.Context) {
	wfID := c.Param("wfId")

	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	result, err := mgr.GetStatus(wfID)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to get workflow status"))
		return
	}
	OK(c, result)
}

// handleGetWorkflowNodes 查询工作流节点列表。
// GET /api/workflows/:wfId/nodes?format=flat|tree
func (s *Server) handleGetWorkflowNodes(c *gin.Context) {
	wfID := c.Param("wfId")
	format := c.DefaultQuery("format", "flat")

	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	result, err := mgr.ListNodes(wfID, format)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to list workflow nodes"))
		return
	}
	OK(c, result)
}

// handleRecoverWorkflows 恢复所有中断的工作流。
// POST /api/workflows/recover
func (s *Server) handleRecoverWorkflows(c *gin.Context) {
	result, err := s.workflowSvc.Recover(c.Request.Context())
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to recover workflows"))
		return
	}
	auditLog(c, s.logger, "recover_workflows", "total", result.Total, "resumed", result.Resumed)
	OK(c, result)
}

// handleWorkflowMetrics 查询工作流管理器指标。
// GET /api/workflows/metrics
func (s *Server) handleWorkflowMetrics(c *gin.Context) {
	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	submitted, completed, failed, terminated, running := mgr.Metrics()
	OK(c, gin.H{
		"submitted":  submitted,
		"completed":  completed,
		"failed":     failed,
		"terminated": terminated,
		"running":    running,
	})
}
