package api

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"github.com/kasuganosora/thinkbot/auth"
	_ "github.com/kasuganosora/thinkbot/docs" // Swagger 文档
)

// ============================================================================
// 路由注册
// ============================================================================

// registerRoutes 注册所有 API 路由。
func (s *Server) registerRoutes() {
	r := s.engine

	// 健康检查（公开，仅返回 ok）
	r.GET("/health", func(c *gin.Context) {
		OK(c, gin.H{"status": "ok"})
	})

	// Swagger UI
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	apiGroup := r.Group("/api")

	// --- 认证（无需登录） ---
	authGroup := apiGroup.Group("/auth")
	{
		authGroup.POST("/login", s.handleLogin)
		authGroup.POST("/logout", s.handleLogout)
	}

	// --- 需要登录的接口 ---
	authed := apiGroup.Group("")
	authed.Use(s.cookieAuth())
	{
		// 当前用户信息
		authed.GET("/auth/me", s.handleMe)
		authed.PUT("/auth/password", s.handleChangePassword)

		// --- 授权码 & 身份绑定（所有登录用户） ---
		authed.POST("/bindcode", s.handleGenerateBindCode)
		authed.GET("/bindcode", s.handleListBindCodes)
		authed.GET("/bindings", s.handleListBindings)
		authed.DELETE("/bindings/:id", s.handleDeleteBinding)

		// --- 用户管理（admin） ---
		users := authed.Group("/users")
		users.Use(requirePermission(auth.PermUserManage))
		{
			users.GET("", s.handleListUsers)
			users.POST("", s.handleCreateUser)
			users.GET("/:id", s.handleGetUser)
			users.PUT("/:id", s.handleUpdateUser)
			users.DELETE("/:id", s.handleDeleteUser)
			users.PUT("/:id/role", s.handleUpdateUserRole)
			users.PUT("/:id/disable", s.handleDisableUser)
			users.PUT("/:id/enable", s.handleEnableUser)
			users.PUT("/:id/password", s.handleResetPassword)
		}

		// --- Bot 管理 ---
		bots := authed.Group("/bots")
		{
			// 所有登录用户可查看 Bot 列表（用于聊天页面选择）
			bots.GET("", s.handleListBots)
			bots.GET("/:id", s.handleGetBot)

			// admin 可管理 Bot
			botsAdmin := bots.Group("")
			botsAdmin.Use(requirePermission(auth.PermBotManage))
			{
				botsAdmin.POST("", s.handleCreateBot)
				botsAdmin.PUT("/:id", s.handleUpdateBot)
				botsAdmin.DELETE("/:id", s.handleDeleteBot)
				botsAdmin.POST("/:id/start", s.handleStartBot)
				botsAdmin.POST("/:id/stop", s.handleStopBot)

				// 梦境巩固配置（嵌套在 Bot 下）
				botsAdmin.GET("/:id/dreaming", s.handleGetDreamingConfig)
				botsAdmin.PUT("/:id/dreaming", s.handleUpdateDreamingConfig)
				botsAdmin.GET("/:id/dreaming/status", s.handleDreamingStatus)
				botsAdmin.POST("/:id/dreaming/trigger", s.handleTriggerDreaming)

				// 定时任务管理（嵌套在 Bot 下）
				botsAdmin.GET("/:id/cron", s.handleListCronJobs)
				botsAdmin.POST("/:id/cron", s.handleCreateCronJob)
				botsAdmin.GET("/:id/cron/:jobId", s.handleGetCronJob)
				botsAdmin.PUT("/:id/cron/:jobId", s.handleUpdateCronJob)
				botsAdmin.DELETE("/:id/cron/:jobId", s.handleDeleteCronJob)
				botsAdmin.POST("/:id/cron/:jobId/pause", s.handlePauseCronJob)
				botsAdmin.POST("/:id/cron/:jobId/resume", s.handleResumeCronJob)
				botsAdmin.POST("/:id/cron/:jobId/trigger", s.handleTriggerCronJob)

				// 记忆查询（嵌套在 Bot 下）
				botsAdmin.GET("/:id/memory", s.handleQueryMemory)
				botsAdmin.POST("/:id/memory", s.handleCreateBotMemoryEntry)
				botsAdmin.PUT("/:id/memory/:mid", s.handleUpdateBotMemoryEntry)
				botsAdmin.DELETE("/:id/memory/:mid", s.handleDeleteBotMemoryEntry)
				botsAdmin.GET("/:id/memory/stats", s.handleMemoryStats)

				// Channel 配置管理（嵌套在 Bot 下）
				botsAdmin.GET("/:id/channels", s.handleListChannels)
				botsAdmin.POST("/:id/channels", s.handleCreateChannel)
				botsAdmin.PUT("/:id/channels/:cid", s.handleUpdateChannel)
				botsAdmin.DELETE("/:id/channels/:cid", s.handleDeleteChannel)

				// 平台管理（嵌套在 Bot 下）
				botsAdmin.GET("/:id/platforms", s.handleListBotPlatforms)
				botsAdmin.POST("/:id/platforms", s.handleCreateBotPlatform)
				botsAdmin.PUT("/:id/platforms/:pid", s.handleUpdateBotPlatform)
				botsAdmin.DELETE("/:id/platforms/:pid", s.handleDeleteBotPlatform)

				// 访问控制
				botsAdmin.GET("/:id/access", s.handleGetBotAccess)
				botsAdmin.PUT("/:id/access", s.handleUpdateBotAccess)

				// 文件管理
				botsAdmin.GET("/:id/files", s.handleListBotFiles)
				botsAdmin.POST("/:id/files/mkdir", s.handleBotFileMkdir)
				botsAdmin.POST("/:id/files/upload", s.handleBotFileUpload)

				// 聊天节奏
				botsAdmin.GET("/:id/chat-rhythm", s.handleGetBotRhythm)
				botsAdmin.PUT("/:id/chat-rhythm", s.handleUpdateBotRhythm)

				// 容器管理
				botsAdmin.GET("/:id/container", s.handleGetBotContainer)
				botsAdmin.GET("/:id/container/snapshots", s.handleGetBotContainerSnapshots)
				botsAdmin.POST("/:id/container/start", s.handleStartBotContainer)
				botsAdmin.POST("/:id/container/stop", s.handleStopBotContainer)
				botsAdmin.POST("/:id/container/snapshots", s.handleCreateBotContainerSnapshot)
				botsAdmin.POST("/:id/container/export", s.handleExportBotContainer)
				botsAdmin.POST("/:id/container/import", s.handleImportBotContainer)
				botsAdmin.POST("/:id/container/restore", s.handleRestoreBotContainer)
				botsAdmin.DELETE("/:id/container", s.handleRemoveBotContainer)

			// 上下文压缩
			botsAdmin.GET("/:id/compaction", s.handleGetBotCompaction)
			botsAdmin.PUT("/:id/compaction", s.handleUpdateBotCompaction)
			botsAdmin.GET("/:id/compaction/history", s.handleGetBotCompactionHistory)
			botsAdmin.DELETE("/:id/compaction/history", s.handleClearBotCompactionHistory)

			// Bot 级技能管理
			botsAdmin.GET("/:id/skills", s.handleListBotSkills)
			botsAdmin.GET("/:id/skills/:sid", s.handleGetBotSkill)
			botsAdmin.POST("/:id/skills", s.handleCreateBotSkill)
			botsAdmin.PUT("/:id/skills/:sid", s.handleUpdateBotSkill)
			botsAdmin.DELETE("/:id/skills/:sid", s.handleRemoveBotSkill)

			// Bot MCP 服务器管理
			botsAdmin.GET("/:id/mcp", s.handleListBotMcp)
			botsAdmin.POST("/:id/mcp", s.handleCreateBotMcp)
			botsAdmin.PUT("/:id/mcp/:mid", s.handleUpdateBotMcp)
			botsAdmin.DELETE("/:id/mcp/:mid", s.handleRemoveBotMcp)
			botsAdmin.POST("/:id/mcp/import", s.handleImportBotMcp)
		}
	}

		// Channel 类型列表（所有登录用户可见，驱动前端表单）
		authed.GET("/channels/types", s.handleListChannelTypes)

		// 平台工具目录（所有登录用户可见，驱动 Bot 详情面板）
		authed.GET("/bots/platforms/tool-catalog", s.handleBotToolCatalog)

		// --- LLM 模型管理（admin）— 保留旧路由兼容 ---
		llmGroup := authed.Group("/llm/models")
		llmGroup.Use(requirePermission(auth.PermBotManage))
		{
			llmGroup.GET("", s.handleListLLMModels)
			llmGroup.POST("", s.handleCreateLLMModel)
			llmGroup.PUT("/:id", s.handleUpdateLLMModel)
			llmGroup.DELETE("/:id", s.handleDeleteLLMModel)
		}

		// --- Provider 层级化模型管理（admin）---
		providerGroup := authed.Group("/providers")
		providerGroup.Use(requirePermission(auth.PermBotManage))
		{
			providerGroup.GET("", s.handleListProviders)
			providerGroup.POST("", s.handleCreateProvider)
			providerGroup.PUT("/:pid", s.handleUpdateProvider)
			providerGroup.DELETE("/:pid", s.handleDeleteProvider)
			providerGroup.POST("/:pid/test", s.handleTestProvider)
			providerGroup.POST("/:pid/models", s.handleAddModel)
			providerGroup.PUT("/:pid/models/:mid", s.handleUpdateModel)
			providerGroup.DELETE("/:pid/models/:mid", s.handleDeleteModel)
			providerGroup.POST("/:pid/models/import", s.handleImportModels)
		}

		// --- 聊天（需要 bot.use 权限） ---
		chat := authed.Group("/chat")
		chat.Use(requirePermission(auth.PermBotUse))
		{
			chat.GET("/bots", s.handleChatBots)       // 可聊天的 Bot 列表
			chat.GET("/history", s.handleChatHistory) // 聊天历史（游标分页）
			chat.POST("/send", s.handleChatSend)      // SSE 流式聊天
		}

		// --- 系统配置（admin） ---
		configGroup := authed.Group("/config")
		configGroup.Use(requirePermission(auth.PermSystemConfig))
		{
			configGroup.GET("", s.handleGetConfig)
			configGroup.GET("/:key", s.handleGetConfigKey)
			configGroup.PUT("/:key", s.handleSetConfigKey)
			configGroup.PUT("", s.handleBatchSetConfig)
		}

		// --- 统计数据（admin） ---
		statsGroup := authed.Group("/stats")
		statsGroup.Use(requirePermission(auth.PermUserManage))
		{
			statsGroup.GET("/overview", s.handleStatsOverview)
			statsGroup.GET("/daily", s.handleStatsDailyRange)
			statsGroup.GET("/daily-by-bot", s.handleStatsDailyByBot)
			statsGroup.GET("/records", s.handleStatsRecords)
			statsGroup.GET("/by-bot-model", s.handleStatsByBotModel)
			statsGroup.GET("/bots/:id", s.handleStatsBot)
			statsGroup.GET("/bots/:id/daily", s.handleStatsBotDaily)
		}

		// --- 工作流监控（admin，只读 + 恢复 + 节点重试） ---
		// 工作流的创建和控制由 Agent 通过 task 系列工具完成，
		// 终止由 session 生命周期信号触发。API 只暴露只读监控和崩溃恢复。
		wfGroup := authed.Group("/workflows")
		wfGroup.Use(requirePermission(auth.PermBotManage))
		{
			wfGroup.GET("", s.handleListWorkflows)
			wfGroup.POST("/recover", s.handleRecoverWorkflows)
			wfGroup.GET("/metrics", s.handleWorkflowMetrics)
			wfGroup.GET("/:wfId", s.handleGetWorkflowStatus)
			wfGroup.GET("/:wfId/nodes", s.handleGetWorkflowNodes)
			wfGroup.POST("/:wfId/nodes/:nodeId/retry", s.handleRetryWorkflowNode)
		}

		// --- 技能管理（admin） ---
		skillGroup := authed.Group("/skills")
		skillGroup.Use(requirePermission(auth.PermBotManage))
		{
			skillGroup.GET("", s.handleListSkills)
			skillGroup.GET("/:name", s.handleGetSkill)
			skillGroup.PUT("/:name/enable", s.handleEnableSkill)
			skillGroup.PUT("/:name/disable", s.handleDisableSkill)
		}

		// --- 搜索提供方管理（admin） ---
		searchGroup := authed.Group("/search/providers")
		searchGroup.Use(requirePermission(auth.PermBotManage))
		{
			searchGroup.GET("", s.handleListSearchProviders)
			searchGroup.POST("", s.handleCreateSearchProvider)
			searchGroup.PUT("/:id", s.handleUpdateSearchProvider)
			searchGroup.DELETE("/:id", s.handleRemoveSearchProvider)
			searchGroup.PUT("/:id/toggle", s.handleToggleSearchProvider)
		}

		// --- 系统监控（admin） ---
		sysGroup := authed.Group("/system")
		sysGroup.Use(requirePermission(auth.PermSystemConfig))
		{
			sysGroup.GET("/health", s.handleHealthDetailed)
			sysGroup.GET("/events/metrics", s.handleEventBusMetrics)
		}

		// --- 会话工具（admin） ---
		sessionGroup := authed.Group("/sessions")
		sessionGroup.Use(requirePermission(auth.PermBotManage))
		{
			sessionGroup.GET("/:sid/terminal", s.handleSessionTerminal)
			sessionGroup.POST("/:sid/terminal/exec", s.handleSessionTerminalExec)
			sessionGroup.GET("/:sid/files", s.handleSessionFiles)
			sessionGroup.POST("/:sid/files/mkdir", s.handleSessionFileMkdir)
			sessionGroup.POST("/:sid/files/upload", s.handleSessionFileUpload)
			sessionGroup.GET("/:sid/status", s.handleSessionStatus)
			sessionGroup.POST("/:sid/compact", s.handleSessionCompact)
		}
	}

	// --- 静态文件服务（前端 SPA） ---
	staticDir := "static"
	if _, err := os.Stat(staticDir); err == nil {
		// 直接访问的静态资源文件（js/css/图片等）
		r.Use(serveStatic(staticDir))
		// SPA fallback：未匹配的路由返回 index.html
		r.NoRoute(func(c *gin.Context) {
			// 排除 /api 路径
			if len(c.Request.URL.Path) >= 4 && c.Request.URL.Path[:4] == "/api" {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			indexPath := filepath.Join(staticDir, "index.html")
			c.File(indexPath)
		})
	}
}

// serveStatic 返回静态文件中间件。
// 匹配 static 目录下的实际文件，不存在的路径交给后续 NoRoute 处理。
func serveStatic(staticDir string) gin.HandlerFunc {
	fs := http.FileServer(http.Dir(staticDir))
	return func(c *gin.Context) {
		// 排除 /api 路径
		if len(c.Request.URL.Path) >= 4 && c.Request.URL.Path[:4] == "/api" {
			c.Next()
			return
		}
		// 检查文件是否存在
		filePath := filepath.Join(staticDir, filepath.Clean(c.Request.URL.Path))
		if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
			fs.ServeHTTP(c.Writer, c.Request)
			c.Abort()
			return
		}
		c.Next()
	}
}
