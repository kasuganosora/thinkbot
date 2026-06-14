package main

import (
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
	"gorm.io/gorm"
)

func main() {
	if err := log.InitWithConfig(log.Config{
		Level: "debug",
		Outputs: []log.Output{
			// stdout：全级别，console 格式
			log.Stdout(),
			// stderr：仅 warn 及以上，console 格式
			{Type: log.OutputStderr, Level: "warn", Format: log.FormatConsole},
			// 文件：全级别，JSONL 格式，滚动
			log.File("./logs", "thinkbot"),
		},
	}); err != nil {
		panic(err)
	}
	defer log.Logger.Sync()

	log.Logger.Infow("starting thinkbot")

	database, err := db.OpenSQLiteWithLogger("thinkbot.db", log.NewGormLogger(log.GormConfig{
		Level:                     log.GormInfo,
		SlowThreshold:             200_000_000, // 200ms in ns
		IgnoreRecordNotFoundError: true,
	}))
	if err != nil {
		log.Logger.Fatalw("failed to open database", "error", err)
	}

	if err := dao.Migrate(database); err != nil {
		log.Logger.Fatalw("failed to migrate database", "error", err)
	}

	if err := runExample(database); err != nil {
		log.Logger.Errorw("example execution failed", "error", err)
	}
}

func runExample(database *gorm.DB) error {
	user := &db.User{Name: "ThinkBot", Email: "hello@thinkbot.local"}
	if err := database.Create(user).Error; err != nil {
		return errs.Wrap(err, "create user record")
	}

	log.Logger.Infow("example completed", "user_id", user.ID)
	return nil
}
