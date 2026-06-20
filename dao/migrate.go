package dao

import "gorm.io/gorm"

// Migrate 执行所有数据库表的自动迁移。
func Migrate(database *gorm.DB) error {
	return database.AutoMigrate(
		&User{},
		&Setting{},
		&WorkflowModel{},
		&UsageDaily{},
		&EntryModel{},
		&WindowStateModel{},
	)
}
