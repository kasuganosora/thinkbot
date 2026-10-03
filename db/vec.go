package db

/*
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>

static void preloadLibm(void) {
#if defined(__linux__)
	// 官方 sqlite-vec 的 .so 用了 sqrtf 却没 NEEDED libm。
	// dlopen 默认解析不到主程序里的 libm 时会报 undefined symbol: sqrtf，
	// 扩展等于没装上。先把 libm 放进全局符号表。
	dlopen("libm.so.6", RTLD_NOW | RTLD_GLOBAL);
#endif
}
*/
import "C"

import (
	"database/sql"
	"os"
	"sync"

	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
)

// sqliteDriverName 是带 sqlite-vec ConnectHook 的驱动名。
// 默认的 "sqlite3" 不会调用 sqlite3_enable_load_extension，SQL load_extension()
// 因此返回 not authorized。
const sqliteDriverName = "sqlite3_with_vec"

// vecEntry 是 sqlite-vec 官方导出的入口。vec0.so 没有 sqlite3_extension_init，
// 只传路径时 SQLite 找不到默认符号。
const vecEntry = "sqlite3_vec_init"

func init() {
	C.preloadLibm()
	sql.Register(sqliteDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			// 缺文件或加载失败不能让数据库打不开：召回会退回 scope 子串。
			loadVec(conn)
			return nil
		},
	})
}

// VecPath 是 sqlite-vec 扩展路径。空则尝试 THINKBOT_SQLITE_VEC，再尝试镜像默认路径。
func VecPath() string {
	if p := os.Getenv("THINKBOT_SQLITE_VEC"); p != "" {
		return p
	}
	return "/usr/lib/sqlite-vec/vec0"
}

func loadVec(conn *sqlite3.SQLiteConn) {
	if conn == nil {
		return
	}
	path := VecPath()
	if path == "" {
		return
	}
	// 重复加载同一扩展是无害失败；连接池新建连接时必须再挂一次。
	_ = conn.LoadExtension(path, vecEntry)
}

var (
	vecOnce sync.Once
	vecOn   bool
)

// TryEnableVec 探测 sqlite-vec。连接打开时 ConnectHook 已尝试加载；
// 这里只确认 vec_version() 可用。失败不报错：调用方退回 scope 子串检索。
func TryEnableVec(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	vecOnce.Do(func() {
		var version string
		if err := db.Raw("SELECT vec_version()").Scan(&version).Error; err == nil && version != "" {
			vecOn = true
		}
	})
	return vecOn
}

// VecEnabled 报告本次进程是否已成功启用 sqlite-vec。
func VecEnabled() bool { return vecOn }
