package db

import (
	"os"
	"sync"

	"gorm.io/gorm"
)

// VecPath 是 sqlite-vec 扩展路径。空则尝试 THINKBOT_SQLITE_VEC，再尝试镜像默认路径。
func VecPath() string {
	if p := os.Getenv("THINKBOT_SQLITE_VEC"); p != "" {
		return p
	}
	return "/usr/lib/sqlite-vec/vec0"
}

var (
	vecOnce sync.Once
	vecOn   bool
)

// TryEnableVec 探测 sqlite-vec。已链进二进制，或能 load_extension 时返回 true。
// 失败不报错：调用方退回 scope 子串检索。
func TryEnableVec(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	vecOnce.Do(func() {
		var version string
		if err := db.Raw("SELECT vec_version()").Scan(&version).Error; err == nil && version != "" {
			vecOn = true
			return
		}
		path := VecPath()
		if path == "" {
			return
		}
		if err := db.Exec("SELECT load_extension(?)", path).Error; err != nil {
			return
		}
		if err := db.Raw("SELECT vec_version()").Scan(&version).Error; err == nil && version != "" {
			vecOn = true
		}
	})
	return vecOn
}

// VecEnabled 报告本次进程是否已成功启用 sqlite-vec。
func VecEnabled() bool { return vecOn }
