package dao

import (
	dbpkg "github.com/kasuganosora/thinkbot/db"
	"gorm.io/gorm"
)

func Migrate(database *gorm.DB) error {
	return database.AutoMigrate(&dbpkg.User{})
}
