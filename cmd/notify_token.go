package main

import (
	"context"
	"fmt"
	"os"

	"github.com/kasuganosora/thinkbot/db"
	"github.com/kasuganosora/thinkbot/notify"
)

// runNotifyTokenCLI 处理 `thinkbot notify-token ...` 子命令（管理 notify 接口 token），
// 不启动服务本体。见 docs/notify.md。
func runNotifyTokenCLI(dbPath string, args []string) int {
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "database %s not accessible: %v (set DB_PATH)\n", dbPath, err)
		return 1
	}
	database, err := db.OpenSQLite(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		return 1
	}
	if sqlDB, err := database.DB(); err == nil {
		defer sqlDB.Close()
	}
	return notify.RunCLI(context.Background(), database, args, os.Stdout, os.Stderr)
}
