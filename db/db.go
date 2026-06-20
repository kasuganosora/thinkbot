package db

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// sqlitePragmas 是 SQLite DSN 中附加的 PRAGMA 参数，用于启用 WAL 模式和锁等待。
// _busy_timeout=5000: 遇到锁时最多等待 5 秒，而非立即返回 SQLITE_BUSY 错误
// _journal_mode=WAL: 启用 Write-Ahead Logging，允许并发读写
const sqlitePragmas = "?_busy_timeout=5000&_journal_mode=WAL"

// OpenSQLite 打开 SQLite 数据库，使用默认 GORM 配置。
// 自动启用 WAL 模式和 busy_timeout 以防止并发锁死。
func OpenSQLite(path string) (*gorm.DB, error) {
	return gorm.Open(sqlite.Open(path+sqlitePragmas), &gorm.Config{})
}

// OpenSQLiteWithLogger 打开 SQLite 数据库并指定 GORM logger。
// 自动启用 WAL 模式和 busy_timeout 以防止并发锁死。
func OpenSQLiteWithLogger(path string, logger gormlogger.Interface) (*gorm.DB, error) {
	return gorm.Open(sqlite.Open(path+sqlitePragmas), &gorm.Config{
		Logger: logger,
	})
}
