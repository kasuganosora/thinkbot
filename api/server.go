package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/identity"
	"github.com/kasuganosora/thinkbot/skill"
	"github.com/kasuganosora/thinkbot/util/traceid"
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
	workflowSvc *WorkflowService
	skillMgr    *skill.SkillManager
	bindSvc     *identity.BindService
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
	workflowSvc *WorkflowService,
	skillMgr *skill.SkillManager,
	bindSvc *identity.BindService,
) *Server {
	gin.SetMode(gin.ReleaseMode)

	// 将 Gin 内部输出（路由警告、校验错误等）重定向到 zap，统一日志管道
	gin.DefaultWriter = &zapWriter{logger: logger, level: logger.Debugw}
	gin.DefaultErrorWriter = &zapWriter{logger: logger, level: logger.Warnw}

	engine := gin.New()

	// 从配置读取地址和 CORS 来源
	addr := store.GetString(config.KeyAPIAddr, ":8080")
	corsOrigins := parseCORSOrigins(store.GetString(config.KeyAPICORSOrigins, ""))

	// 全局中间件
	// 自定义 panic recovery（使用 zap 记录堆栈，替代 gin.Recovery）
	engine.Use(zapRecovery(logger))
	engine.Use(traceIDMiddleware())
	engine.Use(requestLogger(logger))
	engine.Use(corsMiddleware(corsOrigins))

	s := &Server{
		engine:      engine,
		httpSrv:     &http.Server{Addr: addr, Handler: engine},
		logger:      logger.With("component", "api_server"),
		addr:        addr,
		authSvc:     authSvc,
		botSvc:      botSvc,
		cookie:      cookie,
		chatHistory: chatHistory,
		store:       store,
		db:          db,
		workflowSvc: workflowSvc,
		skillMgr:    skillMgr,
		bindSvc:     bindSvc,
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
		_ = s.httpSrv.Shutdown(shutdownCtx)
	}()

	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- 全局中间件 ---

// traceIDMiddleware 为每个请求注入或复用 Trace ID。
// 优先从请求头 X-Trace-ID 读取，否则生成新 ID。
// 将 Trace ID 注入 context 和响应头，供后续日志关联。
func traceIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 提取或生成 Trace ID
		traceID := c.GetHeader(traceid.HeaderKey)
		if traceID == "" {
			traceID = traceid.New()
		}

		// 注入到 request context
		ctx := traceid.WithTraceID(c.Request.Context(), traceID)
		c.Request = c.Request.WithContext(ctx)

		// 设置响应头，方便客户端关联
		c.Header(traceid.HeaderKey, traceID)

		c.Next()
	}
}

// requestLogger 记录每个 HTTP 请求的基本信息。
// 在 c.Next() 返回后读取 gin.Context 中已注入的用户信息，
// 自动附带用户身份和 Trace ID，方便审计追踪。
func requestLogger(logger *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// 对写操作（POST/PUT/DELETE/PATCH），缓存请求体用于审计日志
		var bodyPreview string
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
			if c.Request.Body != nil {
				raw, _ := io.ReadAll(c.Request.Body)
				// 恢复 body 供后续 handler 读取
				c.Request.Body = io.NopCloser(bytes.NewReader(raw))
				bodyPreview = sanitizeBody(raw)
			}
		}

		c.Next()
		duration := time.Since(start)

		// 提取用户信息和 Trace ID
		user := currentUser(c)
		traceID := traceid.FromContext(c.Request.Context())
		fields := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"query", c.Request.URL.RawQuery,
			"status", c.Writer.Status(),
			"duration", duration.String(),
			"ip", c.ClientIP(),
			traceid.LogField, traceID,
		}
		if user != nil {
			fields = append(fields, "user_id", user.ID, "user", user.Username, "role", user.Role)
		} else {
			fields = append(fields, "user", "anonymous")
		}
		if bodyPreview != "" {
			fields = append(fields, "body", bodyPreview)
		}

		// 4xx/5xx 用 Warn 级别，其余 Info
		if c.Writer.Status() >= 400 {
			logger.Warnw("request", fields...)
		} else {
			logger.Infow("request", fields...)
		}
	}
}

// sanitizeBody 清理请求体中的敏感字段，返回截断后的预览。
// 脱敏字段：password, oldPassword, newPassword, token, secret
func sanitizeBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	// 如果是 JSON，做简单脱敏
	s := string(raw)
	for _, re := range sensitiveFieldRegexes {
		s = re.ReplaceAllString(s, `"***"`)
	}
	// 截断到 512 字符
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}

// sensitiveFieldRegexes 预编译的敏感字段脱敏正则（init 时构建，避免每次请求编译）。
var sensitiveFieldRegexes = func() []*regexp.Regexp {
	keys := []string{
		`"password"`, `"oldPassword"`, `"newPassword"`,
		`"token"`, `"secret"`, `"apiKey"`, `"api_key"`,
	}
	res := make([]*regexp.Regexp, len(keys))
	for i, key := range keys {
		res[i] = regexp.MustCompile(key + `\s*:\s*"[^"]*"`)
	}
	return res
}()

// parseCORSOrigins 解析逗号分隔的 CORS 来源列表。
// 空字符串返回 nil，表示使用默认 localhost 规则。
func parseCORSOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	var origins []string
	for o := range strings.SplitSeq(raw, ",") {
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

// ============================================================================
// Gin ↔ zap 集成
// ============================================================================

// zapRecovery 替代 gin.Recovery()，panic 时通过 zap 记录堆栈和请求上下文，
// 而不是写入 os.Stderr。
func zapRecovery(logger *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				// 捕获堆栈
				stack := make([]byte, 4096)
				n := runtime.Stack(stack, false)
				logger.Errorw("panic recovered",
					"error", rec,
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"ip", c.ClientIP(),
					"stack", string(stack[:n]),
				)
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

// zapWriter 是一个 io.Writer 适配器，将 Gin 的输出转发到 zap。
type zapWriter struct {
	logger *zap.SugaredLogger
	level  func(string, ...any)
}

func (w *zapWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.level(msg)
	}
	return len(p), nil
}
