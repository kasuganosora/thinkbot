package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// 定时任务管理 Handler — CRUD + 暂停/恢复/触发（admin）
// ============================================================================

// handleListCronJobs 列出指定 Bot 的所有定时任务。
// GET /api/bots/:id/cron
//
// @Summary      定时任务列表
// @Description  列出指定 Bot 的所有定时任务
// @Tags         定时任务
// @Produce      json
// @Param        id      path     string  true   "Bot ID"
// @Param        active  query    bool    false  "仅活跃任务"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron [get]
func (s *Server) handleListCronJobs(c *gin.Context) {
	botID := c.Param("id")
	mgr := s.botSvc.GetCronManager(botID)

	activeOnly := c.Query("active") == "true"

	var jobs []*cron.Job
	if activeOnly {
		jobs = mgr.ListActiveJobs()
	} else {
		jobs = mgr.ListJobs()
	}
	OK(c, gin.H{"jobs": jobs, "total": len(jobs)})
}

// handleGetCronJob 获取单个定时任务详情。
// GET /api/bots/:id/cron/:jobId
//
// @Summary      获取定时任务
// @Description  获取指定定时任务的详情
// @Tags         定时任务
// @Produce      json
// @Param        id     path     string  true  "Bot ID"
// @Param        jobId  path     string  true  "任务 ID"
// @Success      200  {object}  Response
// @Failure      404  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron/{jobId} [get]
func (s *Server) handleGetCronJob(c *gin.Context) {
	botID := c.Param("id")
	jobID := c.Param("jobId")
	mgr := s.botSvc.GetCronManager(botID)

	job, ok := mgr.GetJob(jobID)
	if !ok {
		Fail(c, errs.Newf("job %q not found", jobID))
		return
	}
	OK(c, job)
}

// handleCreateCronJob 创建定时任务。
// POST /api/bots/:id/cron
//
// @Summary      创建定时任务
// @Description  为指定 Bot 创建新的定时任务
// @Tags         定时任务
// @Accept       json
// @Produce      json
// @Param        id    path      string           true  "Bot ID"
// @Param        body  body      CreateCronJobReq true  "创建定时任务请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron [post]
func (s *Server) handleCreateCronJob(c *gin.Context) {
	botID := c.Param("id")

	var req CreateCronJobReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	mgr := s.botSvc.GetCronManager(botID)
	job, err := mgr.CreateJob(cron.CreateJobRequest{
		Name:        req.Name,
		Description: req.Description,
		Prompt:      req.Prompt,
		Schedule:    req.Schedule,
		Model:       req.Model,
		Channel:     req.Channel,
		Skills:      req.Skills,
		Feature:     req.Feature,
		MaxRuns:     req.MaxRuns,
		Tags:        req.Tags,
	})
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to create cron job"))
		return
	}
	auditLog(c, s.logger, "create_cron_job", "bot_id", botID, "job_id", job.ID, "name", req.Name, "schedule", req.Schedule)
	OK(c, job)
}

// handleUpdateCronJob 更新定时任务。
// PUT /api/bots/:id/cron/:jobId
//
// @Summary      更新定时任务
// @Description  更新指定定时任务（字段可选）
// @Tags         定时任务
// @Accept       json
// @Produce      json
// @Param        id     path      string            true  "Bot ID"
// @Param        jobId  path      string            true  "任务 ID"
// @Param        body   body      UpdateCronJobReq  true  "更新定时任务请求"
// @Success      200    {object}  Response
// @Failure      400    {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron/{jobId} [put]
func (s *Server) handleUpdateCronJob(c *gin.Context) {
	botID := c.Param("id")
	jobID := c.Param("jobId")

	var req UpdateCronJobReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	updates := map[string]any{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Prompt != nil {
		updates["prompt"] = *req.Prompt
	}
	if req.Schedule != nil {
		updates["schedule"] = *req.Schedule
	}
	if req.Model != nil {
		updates["model"] = *req.Model
	}
	if req.Channel != nil {
		updates["channel"] = *req.Channel
	}
	if req.Feature != nil {
		updates["feature"] = *req.Feature
	}
	if req.MaxRuns != nil {
		updates["max_runs"] = *req.MaxRuns
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}

	mgr := s.botSvc.GetCronManager(botID)
	job, err := mgr.UpdateJob(jobID, updates)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to update cron job"))
		return
	}
	auditLog(c, s.logger, "update_cron_job", "bot_id", botID, "job_id", jobID, "fields", strutil.MapKeys(updates))
	OK(c, job)
}

// handleDeleteCronJob 删除定时任务。
// DELETE /api/bots/:id/cron/:jobId
//
// @Summary      删除定时任务
// @Description  删除指定的定时任务
// @Tags         定时任务
// @Produce      json
// @Param        id     path     string  true  "Bot ID"
// @Param        jobId  path     string  true  "任务 ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron/{jobId} [delete]
func (s *Server) handleDeleteCronJob(c *gin.Context) {
	botID := c.Param("id")
	jobID := c.Param("jobId")

	mgr := s.botSvc.GetCronManager(botID)
	if err := mgr.DeleteJob(jobID); err != nil {
		Fail(c, errs.Wrap(err, "failed to delete cron job"))
		return
	}
	auditLog(c, s.logger, "delete_cron_job", "bot_id", botID, "job_id", jobID)
	OKMsg(c, "cron job deleted", nil)
}

// handlePauseCronJob 暂停定时任务。
// POST /api/bots/:id/cron/:jobId/pause
//
// @Summary      暂停定时任务
// @Description  暂停指定的定时任务
// @Tags         定时任务
// @Produce      json
// @Param        id     path     string  true  "Bot ID"
// @Param        jobId  path     string  true  "任务 ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron/{jobId}/pause [post]
func (s *Server) handlePauseCronJob(c *gin.Context) {
	botID := c.Param("id")
	jobID := c.Param("jobId")

	mgr := s.botSvc.GetCronManager(botID)
	if err := mgr.PauseJob(jobID); err != nil {
		Fail(c, errs.Wrap(err, "failed to pause cron job"))
		return
	}
	auditLog(c, s.logger, "pause_cron_job", "bot_id", botID, "job_id", jobID)
	OKMsg(c, "cron job paused", nil)
}

// handleResumeCronJob 恢复定时任务。
// POST /api/bots/:id/cron/:jobId/resume
//
// @Summary      恢复定时任务
// @Description  恢复指定的定时任务
// @Tags         定时任务
// @Produce      json
// @Param        id     path     string  true  "Bot ID"
// @Param        jobId  path     string  true  "任务 ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron/{jobId}/resume [post]
func (s *Server) handleResumeCronJob(c *gin.Context) {
	botID := c.Param("id")
	jobID := c.Param("jobId")

	mgr := s.botSvc.GetCronManager(botID)
	if err := mgr.ResumeJob(jobID); err != nil {
		Fail(c, errs.Wrap(err, "failed to resume cron job"))
		return
	}
	auditLog(c, s.logger, "resume_cron_job", "bot_id", botID, "job_id", jobID)
	OKMsg(c, "cron job resumed", nil)
}

// handleTriggerCronJob 手动触发定时任务（立即执行）。
// POST /api/bots/:id/cron/:jobId/trigger
//
// @Summary      触发定时任务
// @Description  手动触发指定的定时任务立即执行
// @Tags         定时任务
// @Produce      json
// @Param        id     path     string  true  "Bot ID"
// @Param        jobId  path     string  true  "任务 ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/cron/{jobId}/trigger [post]
func (s *Server) handleTriggerCronJob(c *gin.Context) {
	botID := c.Param("id")
	jobID := c.Param("jobId")

	mgr := s.botSvc.GetCronManager(botID)
	if err := mgr.TriggerJob(jobID); err != nil {
		Fail(c, errs.Wrap(err, "failed to trigger cron job"))
		return
	}
	auditLog(c, s.logger, "trigger_cron_job", "bot_id", botID, "job_id", jobID)
	OKMsg(c, "cron job triggered, will execute on next scheduler tick", nil)
}
