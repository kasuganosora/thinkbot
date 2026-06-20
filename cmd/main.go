package main

import (
	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/api"
	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
	"github.com/kasuganosora/thinkbot/util/log"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func main() {
	if err := log.InitWithConfig(log.Config{
		Level: "debug",
		Outputs: []log.Output{
			log.Stdout(),
			{Type: log.OutputStderr, Level: "warn", Format: log.FormatConsole},
			log.File("./logs", "thinkbot"),
		},
	}); err != nil {
		panic(err)
	}
	defer func() { _ = log.Logger.Sync() }()

	log.Logger.Infow("starting thinkbot")

	app := fx.New(
		// 提供日志
		fx.Provide(zap.NewProduction),
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
		api.Module,

		// 优雅关闭
		fx.NopLogger,
	)

	app.Run()
}
