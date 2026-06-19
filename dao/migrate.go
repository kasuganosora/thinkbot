package dao

import (
	dbpkg "github.com/kasuganosora/thinkbot/db"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/workflow"
	"gorm.io/gorm"
)

func Migrate(database *gorm.DB) error {
	return database.AutoMigrate(
		&dbpkg.User{},
		&config.Setting{},
		&workflow.WorkflowModel{},
	)
}
