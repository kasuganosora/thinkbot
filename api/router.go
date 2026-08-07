package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"

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

				// Channel 配置管理 — 已废弃，统一使用 Platform API（/api/bots/:id/platforms）
				// 旧 Channel API 路由已移除

				// 平台管理（嵌套在 Bot 下）
				botsAdmin.GET("/:id/platforms", s.handleListBotPlatforms)
				botsAdmin.POST("/:id/platforms", s.handleCreateBotPlatform)
				botsAdmin.PUT("/:id/platforms/:pid", s.handleUpdateBotPlatform)
				botsAdmin.DELETE("/:id/platforms/:pid", s.handleDeleteBotPlatform)

				// 文件管理
				botsAdmin.GET("/:id/files", s.handleListBotFiles)
				botsAdmin.GET("/:id/files/download", s.handleBotFileDownload)
				botsAdmin.POST("/:id/files/mkdir", s.handleBotFileMkdir)
				botsAdmin.POST("/:id/files/upload", s.handleBotFileUpload)

				// 容器管理
				botsAdmin.PUT("/:id/container/config", s.handleUpdateBotContainerConfig)
				botsAdmin.GET("/:id/container", s.handleGetBotContainer)
				botsAdmin.GET("/:id/container/snapshots", s.handleGetBotContainerSnapshots)
				botsAdmin.POST("/:id/container/start", s.handleStartBotContainer)
				botsAdmin.POST("/:id/container/stop", s.handleStopBotContainer)
				botsAdmin.POST("/:id/container/snapshots", s.handleCreateBotContainerSnapshot)
				botsAdmin.POST("/:id/container/export", s.handleExportBotContainer)
				botsAdmin.POST("/:id/container/import", s.handleImportBotContainer)
				botsAdmin.POST("/:id/container/restore", s.handleRestoreBotContainer)
				botsAdmin.DELETE("/:id/container", s.handleRemoveBotContainer)

				// 运行时检查（概览页，接入真实 sandbox 状态）
				botsAdmin.GET("/:id/runtime-checks", s.handleBotRuntimeChecks)

				// 访问控制（默认行为 + 规则列表）
				botsAdmin.GET("/:id/access", s.handleGetBotAccess)
				botsAdmin.PUT("/:id/access", s.handleUpdateBotAccess)

				// 终端（容器 shell，接入真实 sandbox exec）
				botsAdmin.GET("/:id/terminal", s.handleBotTerminal)
				botsAdmin.POST("/:id/terminal/exec", s.handleBotTerminalExec)

				// 心跳管理
				botsAdmin.GET("/:id/heartbeat", s.handleGetHeartbeatConfig)
				botsAdmin.PUT("/:id/heartbeat", s.handleUpdateHeartbeatConfig)
				botsAdmin.GET("/:id/heartbeat/logs", s.handleListHeartbeatLogs)
				botsAdmin.DELETE("/:id/heartbeat/logs", s.handleClearHeartbeatLogs)

				// 上下文压缩（agent memory compaction）
				botsAdmin.GET("/:id/compaction", s.handleGetBotCompaction)
				botsAdmin.PUT("/:id/compaction", s.handleUpdateBotCompaction)
				botsAdmin.GET("/:id/compaction/history", s.handleGetBotCompactionHistory)
				botsAdmin.DELETE("/:id/compaction/history", s.handleClearBotCompactionHistory)

				// 聊天节奏（群聊回复节奏控制）
				botsAdmin.GET("/:id/chat-rhythm", s.handleGetBotRhythm)
				botsAdmin.PUT("/:id/chat-rhythm", s.handleUpdateBotRhythm)

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

				// 会话管理（Session 列表 / 新建）
				botsAdmin.GET("/:id/sessions", s.handleListSessions)
				botsAdmin.POST("/:id/sessions", s.handleCreateSession)
			}
		}

		// Session 级操作（删除 / 更新，按 session ID 直接操作）
		sessionCRUD := authed.Group("/sessions")
		sessionCRUD.Use(requirePermission(auth.PermBotManage))
		{
			sessionCRUD.DELETE("/:sid", s.handleDeleteSession)
			sessionCRUD.PUT("/:sid", s.handleUpdateSession)
		}

		// Channel 类型列表 — 已废弃，平台类型信息统一通过 /api/bots/platforms/tool-catalog 获取
		// authed.GET("/channels/types", s.handleListChannelTypes)

		// 平台工具目录（所有登录用户可见，驱动 Bot 详情面板）
		authed.GET("/bots/platforms/tool-catalog", s.handleBotToolCatalog)

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
			chat.GET("/bots", s.handleChatBots)          // 可聊天的 Bot 列表
			chat.GET("/history", s.handleChatHistory)    // 聊天历史（游标分页）
			chat.POST("/send", s.handleChatSend)         // SSE 流式聊天
			chat.POST("/abort", s.handleChatAbort)       // 中止正在执行的聊天
			chat.POST("/append", s.handleChatAppend)     // 生成中追加用户补充（同一轮）
			chat.GET("/active", s.handleChatActiveTasks) // 查询后台仍在执行的任务 traceID
			chat.GET("/resume", s.handleChatResume)      // 按 traceID 重连续流（SSE）

			// 运营操作（admin）：重置 token 预算，解除预算永久卡死导致 bot 无响应
			chatAdmin := chat.Group("")
			chatAdmin.Use(requirePermission(auth.PermBotManage))
			{
				chatAdmin.POST("/token-budget/reset", s.handleResetTokenBudget)
			}
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
		// 会话内的工作流进度（status / nodes / 节点重试）由该用户的 Bot 在对话中
		// 创建，应对【任意已登录用户】可见，不应要求 admin 权限——否则普通成员在
		// 对话里看不到 workflow 卡片。列表 / 指标 / 崩溃恢复属于运营操作，保留 admin。
		wfRead := authed.Group("/workflows")
		{
			wfRead.GET("/:wfId", s.handleGetWorkflowStatus)
			wfRead.GET("/:wfId/nodes", s.handleGetWorkflowNodes)
			wfRead.POST("/:wfId/nodes/:nodeId/retry", s.handleRetryWorkflowNode)
		}

		// 按会话查最近一条工作流：前端刷新页面后用它恢复工作流卡片
		// （activeWorkflowId 只存在于内存，刷新即丢，而工作流仍在后台运行）。
		// 刻意**不挂在 /workflows 组下**：它与 `/workflows/:wfId` 属同级路径，
		// gin 对同级「静态段 + 通配段」的注册会 panic。
		authed.GET("/session-workflow", s.handleGetSessionWorkflow)

		wfAdmin := authed.Group("/workflows")
		wfAdmin.Use(requirePermission(auth.PermBotManage))
		{
			wfAdmin.GET("", s.handleListWorkflows)
			wfAdmin.POST("/recover", s.handleRecoverWorkflows)
			wfAdmin.GET("/metrics", s.handleWorkflowMetrics)
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
			sessionGroup.GET("/:sid/files/download", s.handleSessionFileDownload)
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
			// index.html 绝不能被缓存：它引用的是带内容哈希的 chunk 文件名。
			// 若浏览器复用旧 index.html，就会继续加载旧 chunk，用户「刷新了也没生效」。
			// http.FileServer / c.File 默认只发 Last-Modified，浏览器对 HTML 会启用
			// 启发式缓存，因此必须显式声明 no-store。
			setNoStore(c)
			c.File(indexPath)
		})
	}
}

// setNoStore 声明响应不可缓存。
//
// 用于 index.html 这类「入口文档」：它引用带内容哈希的资源文件名，一旦被浏览器缓存，
// 前端发版后用户即使刷新也会继续加载旧 chunk，表现为「代码改了但页面没变」。
func setNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
}

// hashedAssetRE 匹配 Vite 构建产物中带内容哈希的文件名，如：
//
//	index-BymryakM.js / Chat-B3U2oJJs.js / style-a1b2c3d4.css / index-BymryakM.js.map
//
// 这类文件名随内容变化，可以安全地长期缓存。
// 末尾允许一个可选的 .map，因为 sourcemap 形如 <name>-<hash>.js.map。
var hashedAssetRE = regexp.MustCompile(`-[A-Za-z0-9_-]{8,}\.(js|css|woff2?|ttf|otf|eot|svg|png|jpe?g|gif|webp|avif)(\.map)?$`)

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
		urlPath := c.Request.URL.Path
		filePath := filepath.Join(staticDir, filepath.Clean(urlPath))
		if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
			// 缓存策略分两类：
			//   - 带内容哈希的资源：文件名即版本，可放心长缓存（immutable 免去重复校验）。
			//   - 其余（index.html 等无哈希入口文档）：必须每次校验，否则前端发版后
			//     用户刷新仍会加载旧 chunk。
			if hashedAssetRE.MatchString(urlPath) {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				setNoStore(c)
			}
			fs.ServeHTTP(c.Writer, c.Request)
			c.Abort()
			return
		}
		c.Next()
	}
}
