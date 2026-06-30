package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/workflow"
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
//
// @Summary      工作流列表
// @Description  列出最近的工作流（需要 bot.manage 权限）
// @Tags         工作流
// @Produce      json
// @Param        limit  query     int  false  "返回数量"  default(20)
// @Success      200    {object}  Response
// @Security     CookieAuth
// @Router       /api/workflows [get]
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
//
// @Summary      工作流状态
// @Description  查询指定工作流的详细状态
// @Tags         工作流
// @Produce      json
// @Param        wfId  path      string  true  "工作流 ID"
// @Success      200   {object}  Response
// @Failure      500   {object}  Response
// @Security     CookieAuth
// @Router       /api/workflows/{wfId} [get]
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
//
// @Summary      工作流节点
// @Description  查询指定工作流的节点列表
// @Tags         工作流
// @Produce      json
// @Param        wfId    path      string  true   "工作流 ID"
// @Param        format  query     string  false  "输出格式 (flat/tree)"  default(flat)
// @Success      200     {object}  Response
// @Failure      500     {object}  Response
// @Security     CookieAuth
// @Router       /api/workflows/{wfId}/nodes [get]
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
//
// @Summary      恢复工作流
// @Description  恢复所有中断的工作流
// @Tags         工作流
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/workflows/recover [post]
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
//
// @Summary      工作流指标
// @Description  返回工作流引擎的全局指标
// @Tags         工作流
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/workflows/metrics [get]
func (s *Server) handleWorkflowMetrics(c *gin.Context) {
	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	snapshot := mgr.MetricsSnapshot()
	OK(c, gin.H{
		"submitted":     snapshot.Submitted,
		"completed":     snapshot.Completed,
		"failed":        snapshot.Failed,
		"terminated":    snapshot.Terminated,
		"running":       snapshot.Running,
		"nodeExecuted":  snapshot.NodeExecuted,
		"nodeFailed":    snapshot.NodeFailed,
		"nodeRetries":   snapshot.NodeRetries,
		"nodeReviews":   snapshot.NodeReviews,
		"nodeSkipped":   snapshot.NodeSkipped,
		"persistErrors": snapshot.PersistErrors,
	})
}

// handleRetryWorkflowNode 重试工作流中的指定节点。
// POST /api/workflows/:wfId/nodes/:nodeId/retry
//
// @Summary      重试节点
// @Description  重试指定工作流中失败的节点
// @Tags         工作流
// @Produce      json
// @Param        wfId    path  string  true  "工作流 ID"
// @Param        nodeId  path  string  true  "节点 ID"
// @Success      200     {object}  Response
// @Failure      500     {object}  Response
// @Security     CookieAuth
// @Router       /api/workflows/{wfId}/nodes/{nodeId}/retry [post]
func (s *Server) handleRetryWorkflowNode(c *gin.Context) {
	wfID := c.Param("wfId")
	nodeID := c.Param("nodeId")

	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	result, err := mgr.Control(c.Request.Context(), wfID, workflow.ControlRequest{
		Action: workflow.ActionRetry,
		NodeID: nodeID,
	})
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to retry workflow node"))
		return
	}

	auditLog(c, s.logger, "retry_workflow_node", "workflow", wfID, "node", nodeID)
	OK(c, result)
}
