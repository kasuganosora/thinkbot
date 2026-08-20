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
	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/api"
	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
	"github.com/kasuganosora/thinkbot/identity"
	"github.com/kasuganosora/thinkbot/stats"
	"github.com/kasuganosora/thinkbot/util/log"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func main() {
	// 注意：不要再额外注册 stderr core。stdout core 已覆盖全部级别（含 WARN/ERROR），
	// 而守护进程以 `> log 2>&1` 启动会把 stdout 与 stderr 合流到同一个文件，
	// 额外的 stderr core 会让每条 WARN/ERROR 落盘两份，凭日志统计错误频率时直接翻倍。
	if err := log.InitWithConfig(log.Config{
		Level: "debug",
		Outputs: []log.Output{
			log.Stdout(),
			log.File("./logs", "thinkbot"),
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
			return db.OpenSQLiteWithLogger("thinkbot.db", log.NewGormLogger(log.GormConfig{
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
