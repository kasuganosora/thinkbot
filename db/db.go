package db

import (
	"path/filepath"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// sqlitePragmas 是 SQLite DSN 中附加的 PRAGMA 参数，用于启用 WAL 模式和锁等待。
// _busy_timeout=5000: 遇到锁时最多等待 5 秒，而非立即返回 SQLITE_BUSY 错误
// _journal_mode=WAL: 启用 Write-Ahead Logging，允许并发读写
const sqlitePragmas = "?_busy_timeout=5000&_journal_mode=WAL"

// openSQLite 打开 SQLite 数据库并统一应用连接池与 PRAGMA 配置。
// 相对路径会解析为绝对路径（基于进程工作目录），避免工作目录变化导致
// 打开出第二个数据库文件（报告 5341）。WAL 模式下限制单写连接，
// 规避多写连接并发导致的 SQLITE_BUSY。
func openSQLite(path string, logger gormlogger.Interface) (*gorm.DB, error) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	db, err := gorm.Open(sqlite.Open(path+sqlitePragmas), &gorm.Config{
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	return db, nil
}

// OpenSQLite 打开 SQLite 数据库，使用默认 GORM 配置。
// 自动启用 WAL 模式和 busy_timeout 以防止并发锁死。
func OpenSQLite(path string) (*gorm.DB, error) {
	return openSQLite(path, nil)
}

// OpenSQLiteWithLogger 打开 SQLite 数据库并指定 GORM logger。
// 自动启用 WAL 模式和 busy_timeout 以防止并发锁死。
func OpenSQLiteWithLogger(path string, logger gormlogger.Interface) (*gorm.DB, error) {
	return openSQLite(path, logger)
}
