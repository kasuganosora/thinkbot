package api

import (
	"strconv"
	"strings"

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

// handleGetSessionWorkflow 返回指定 bot + 会话最近提交的一条工作流。
// GET /api/session-workflow?botId=xxx&sessionId=yyy
//
// 用途：前端刷新页面后 activeWorkflowId 会丢失（它只能从实时 SSE 事件里拿到），
// 工作流卡片随之消失，而工作流本身仍在后台运行。前端载入会话后调用本接口恢复卡片。
//
// 没有匹配时返回 200 + workflow:null（不是 404）——「这个会话没有工作流」是正常状态，
// 不是错误，前端不该为此打印错误日志。
//
// @Summary      会话工作流
// @Description  查询指定 bot + 会话最近提交的工作流（用于页面刷新后恢复卡片）
// @Tags         工作流
// @Produce      json
// @Param        botId      query     string  true   "Bot ID"
// @Param        sessionId  query     string  false  "会话 ID"
// @Success      200        {object}  Response
// @Security     CookieAuth
// @Router       /api/session-workflow [get]
func (s *Server) handleGetSessionWorkflow(c *gin.Context) {
	botID := c.Query("botId")
	if botID == "" {
		// 参数缺失是客户端问题，必须 400 —— 用 errs.New 会落成 500，
		// 让调用方误判为服务端故障。
		Fail(c, errs.BadRequest("botId is required"))
		return
	}

	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	wf := mgr.LatestWorkflowForSession(botID, c.Query("sessionId"))
	if wf == nil {
		OK(c, gin.H{"workflow": nil})
		return
	}
	OK(c, gin.H{"workflow": gin.H{
		"id":        wf.ID,
		"status":    wf.Status,
		"nodeCount": len(wf.Nodes),
		"createdAt": wf.CreatedAt,
	}})
}

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

// handleContinueWorkflow 手动重触发工作流续跑。
// POST /api/workflows/:wfId/continue
//
// 用途：工作流已完成、但续跑 agent 的回复因服务重启丢失（续跑消息已注入、agent 上下文
// 随引擎关闭丢失、回复从未落库）时，前端「继续」按钮或运维手动调用本接口，重新注入续跑
// 消息唤醒 agent，把丢失的续跑回复补回来。也可用于启动期自动续跑未能覆盖的边界场景。
//
// 与节点重试不同：续跑作用于「已终态的工作流」，重新把工作流结果作为系统消息注入会话，
// 让 agent 基于各节点真实产出继续完成最初需求。可被同一生命周期内多次调用（每次都重新注入）。
//
// @Summary      续跑工作流
// @Description  重新注入续跑消息，唤醒 agent 继续完成最初需求（用于续跑回复因重启丢失的恢复）
// @Tags         工作流
// @Produce      json
// @Param        wfId  path  string  true  "工作流 ID"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Failure      500   {object}  Response
// @Security     CookieAuth
// @Router       /api/workflows/{wfId}/continue [post]
func (s *Server) handleContinueWorkflow(c *gin.Context) {
	wfID := c.Param("wfId")

	mgr, err := s.workflowSvc.Manager()
	if err != nil {
		Fail(c, errs.Wrap(err, "workflow engine not available"))
		return
	}

	if err := mgr.TriggerContinuation(wfID); err != nil {
		// 非终态（wfId 还在跑）属客户端误用 → 400；引擎未装配等属服务端问题 → 500。
		if strings.Contains(err.Error(), "not terminal") {
			Fail(c, errs.BadRequest(err.Error()))
		} else {
			Fail(c, errs.Wrap(err, "failed to continue workflow"))
		}
		return
	}

	auditLog(c, s.logger, "continue_workflow", "workflow", wfID)
	OK(c, gin.H{"workflowId": wfID, "status": "continuing"})
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

	// 返回前端期望的 {workflowId, nodeId, status} 格式
	status := "running"
	if result != nil && !result.Success {
		status = "failed"
	}
	OK(c, gin.H{
		"workflowId": wfID,
		"nodeId":     nodeID,
		"status":     status,
	})
}
