package db

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// vecOnce 是进程级的，成功一次之后同进程里的失败用例会被缓存结果污染。
// 父测试只负责拉起子进程；真正打开库的是子进程。
func TestSQLiteVecLoad(t *testing.T) {
	if os.Getenv("THINKBOT_VEC_CHILD") == "1" {
		switch os.Getenv("THINKBOT_VEC_CASE") {
		case "missing":
			childVecMissing(t)
		case "ok":
			childVecOK(t)
		default:
			t.Fatalf("unknown vec case %q", os.Getenv("THINKBOT_VEC_CASE"))
		}
		return
	}

	t.Run("missing", func(t *testing.T) {
		runVecChild(t, "missing", "/tmp/sqlite-vec/does-not-exist")
	})
	t.Run("ok", func(t *testing.T) {
		runVecChild(t, "ok", "/tmp/sqlite-vec/vec0")
	})
}

func runVecChild(t *testing.T, cas, vecPath string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteVecLoad$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(),
		"THINKBOT_VEC_CHILD=1",
		"THINKBOT_VEC_CASE="+cas,
		"THINKBOT_SQLITE_VEC="+vecPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child %s failed: %v\n%s", cas, err, out)
	}
}

func childVecMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	gdb, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if TryEnableVec(gdb) {
		t.Fatal("TryEnableVec = true, want false when extension file is missing")
	}
	var n int
	if err := gdb.Raw("SELECT 1").Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("db unusable after failed vec load: n=%d err=%v", n, err)
	}
}

func childVecOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vec.db")
	gdb, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if !TryEnableVec(gdb) {
		t.Fatal("TryEnableVec = false, want true")
	}
	var version string
	if err := gdb.Raw("SELECT vec_version()").Scan(&version).Error; err != nil || version == "" {
		t.Fatalf("vec_version: %q err=%v", version, err)
	}
	if err := gdb.Exec("CREATE VIRTUAL TABLE memory_vec USING vec0(embedding float[8])").Error; err != nil {
		t.Fatalf("create vec0 table: %v", err)
	}
}
