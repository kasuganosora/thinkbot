package api

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/auth"
)

// ============================================================================
// 路由注册
// ============================================================================

// registerRoutes 注册所有 API 路由。
func (s *Server) registerRoutes() {
	r := s.engine

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		OK(c, gin.H{"status": "ok"})
	})

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

				// Channel 配置管理（嵌套在 Bot 下）
				botsAdmin.GET("/:botId/channels", s.handleListChannels)
				botsAdmin.POST("/:botId/channels", s.handleCreateChannel)
				botsAdmin.PUT("/:botId/channels/:id", s.handleUpdateChannel)
				botsAdmin.DELETE("/:botId/channels/:id", s.handleDeleteChannel)
			}
		}

		// Channel 类型列表（所有登录用户可见，驱动前端表单）
		authed.GET("/channels/types", s.handleListChannelTypes)

		// --- 聊天（需要 bot.use 权限） ---
		chat := authed.Group("/chat")
		chat.Use(requirePermission(auth.PermBotUse))
		{
			chat.GET("/bots", s.handleChatBots)  // 可聊天的 Bot 列表
			chat.GET("/history", s.handleChatHistory) // 聊天历史（游标分页）
			chat.POST("/send", s.handleChatSend) // SSE 流式聊天
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
			statsGroup.GET("/bots/:id", s.handleStatsBot)
			statsGroup.GET("/bots/:id/daily", s.handleStatsBotDaily)
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
