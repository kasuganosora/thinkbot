package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
)

// ============================================================================
// Server — Gin HTTP 服务器
// ============================================================================

// Server 包装 gin.Engine 和 http.Server，管理生命周期。
// 持有所有 handler 所需的依赖引用。
type Server struct {
	engine  *gin.Engine
	httpSrv *http.Server
	logger  *zap.SugaredLogger
	addr    string

	// 依赖
	authSvc     *auth.AuthService
	botSvc      *BotService
	cookie      *CookieManager
	chatHistory *ChatHistoryService
	store       *config.Store
	db          *gorm.DB
}

// NewServer 创建并配置 Gin Server。
func NewServer(
	authSvc *auth.AuthService,
	botSvc *BotService,
	cookie *CookieManager,
	chatHistory *ChatHistoryService,
	store *config.Store,
	db *gorm.DB,
	logger *zap.SugaredLogger,
) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	// 从配置读取地址和 CORS 来源
	addr := store.GetString(config.KeyAPIAddr, ":8080")
	corsOrigins := parseCORSOrigins(store.GetString(config.KeyAPICORSOrigins, ""))

	// 全局中间件
	engine.Use(gin.Recovery())
	engine.Use(requestLogger(logger))
	engine.Use(corsMiddleware(corsOrigins))

	s := &Server{
		engine:  engine,
		httpSrv: &http.Server{Addr: addr, Handler: engine},
		logger:  logger.With("component", "api_server"),
		addr:    addr,
		authSvc: authSvc,
		botSvc:  botSvc,
		cookie:  cookie,
		chatHistory: chatHistory,
		store:   store,
		db:      db,
	}

	// 注册所有路由
	s.registerRoutes()

	return s
}

// Start 启动 HTTP 服务器（阻塞）。
func (s *Server) Start(ctx context.Context) error {
	s.logger.Infow("API server starting", "addr", s.addr)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.httpSrv.Shutdown(shutdownCtx)
	}()

	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- 全局中间件 ---

func requestLogger(logger *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start)
		logger.Infow("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration", duration.String(),
			"ip", c.ClientIP(),
		)
	}
}

// parseCORSOrigins 解析逗号分隔的 CORS 来源列表。
// 空字符串返回 nil，表示使用默认 localhost 规则。
func parseCORSOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	var origins []string
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}

// isLocalhostOrigin 判断来源是否为 localhost 开发地址。
func isLocalhostOrigin(origin string) bool {
	for _, prefix := range []string{"http://localhost:", "http://127.0.0.1:", "https://localhost:", "https://127.0.0.1:"} {
		if strings.HasPrefix(origin, prefix) {
			return true
		}
	}
	// 允许不带端口的标准 localhost（浏览器有时省略端口）
	if origin == "http://localhost" || origin == "https://localhost" {
		return true
	}
	return false
}

// corsMiddleware 返回 CORS 中间件。
// 如果 whitelist 为空，默认允许 localhost 来源（开发模式）。
// 否则仅允许白名单中的来源。
func corsMiddleware(whitelist []string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(whitelist))
	for _, o := range whitelist {
		allowed[o] = true
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			permit := false
			if len(whitelist) == 0 {
				// 开发模式：允许 localhost
				permit = isLocalhostOrigin(origin)
			} else {
				permit = allowed[origin]
			}
			if permit {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
				c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
				c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
