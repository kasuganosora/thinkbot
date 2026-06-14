package main

import (
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
	"gorm.io/gorm"
)

func main() {
	if err := log.Init(); err != nil {
		panic(err)
	}
	defer log.Logger.Sync()

	log.Logger.Infow("starting thinkbot")

	database, err := db.OpenSQLite("thinkbot.db")
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
