package db

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// OpenSQLite 打开 SQLite 数据库，使用默认 GORM 配置。
func OpenSQLite(path string) (*gorm.DB, error) {
	return gorm.Open(sqlite.Open(path), &gorm.Config{})
}

// OpenSQLiteWithLogger 打开 SQLite 数据库并指定 GORM logger。
func OpenSQLiteWithLogger(path string, logger gormlogger.Interface) (*gorm.DB, error) {
	return gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger,
	})
}
