package main

// @title           thinkbot API
// @version         1.0
// @description     多渠道 AI 聊天机器人框架的 HTTP API
// @host            localhost:8080
// @BasePath        /
// @securityDefinitions.apikey  CookieAuth
// @in                            cookie
// @name                          token
import (
	"os"
	"path/filepath"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/api"
	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
	"github.com/kasuganosora/thinkbot/identity"
	"github.com/kasuganosora/thinkbot/internal/singleinst"
	"github.com/kasuganosora/thinkbot/stats"
	"github.com/kasuganosora/thinkbot/util/log"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func main() {
	// 配置键 db.path / log.level 经环境变量映射消费（EnvKeyToConfigKey:
	// DB_PATH→db.path、LOG_LEVEL→log.level），避免这两个键成为「死键」。
	// 数据库默认落在 data/ 卷（配合 docker 的 ./data:/app/data 持久化，重启不丢库）；
	// 日志级别默认 info（不再硬编码 debug 向生产输出全量调试日志）。
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data/thinkbot.db"
	}
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	// 确保数据库父目录存在（默认 data/，落在持久化卷内）。
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic(err)
		}
	}

	// 注意：不要再额外注册 stderr core。stdout core 已覆盖全部级别（含 WARN/ERROR），
	// 而守护进程以 `> log 2>&1` 启动会把 stdout 与 stderr 合流到同一个文件，
	// 额外的 stderr core 会让每条 WARN/ERROR 落盘两份，凭日志统计错误频率时直接翻倍。
	if err := log.InitWithConfig(log.Config{
		Level: logLevel,
		Outputs: []log.Output{
			log.Stdout(),
			log.File("./logs", "thinkbot"),
			// 可读且自动轮换的纯文本日志（文件名独立，与 JSON 文件各自独立轮换），
			// 实时 tail 用这个，不要再依赖 daemon 重定向到 /tmp/thinkbot.log 的无轮换裸文件。
			log.ConsoleFile("./logs", "thinkbot.console"),
		},
	}); err != nil {
		panic(err)
	}
	defer func() { _ = log.Logger.Sync() }()

	log.Logger.Infow("starting thinkbot")

	app := fx.New(
		// 提供日志（使用已配置的全局 logger，统一输出到 stdout/stderr/file）
		fx.Provide(func() *zap.Logger { return log.Logger.Desugar() }),
		fx.Provide(func(l *zap.Logger) *zap.SugaredLogger { return l.Sugar() }),

		// 数据库
		fx.Provide(func() (*gorm.DB, error) {
			return db.OpenSQLiteWithLogger(dbPath, log.NewGormLogger(log.GormConfig{
				Level:                     log.GormInfo,
				SlowThreshold:             200_000_000,
				IgnoreRecordNotFoundError: true,
			}))
		}),

		// 数据库迁移
		fx.Invoke(func(db *gorm.DB) error {
			return dao.Migrate(db)
		}),

		// 模块
		config.Module,
		singleinst.Module, // 单实例版本协商，必须在 bot.Module 之前，避免双实例并发消费
		auth.Module,
		bot.Module,
		identity.Module,
		stats.Module,
		api.Module,

		// 优雅关闭
		fx.NopLogger,
	)

	app.Run()
}
