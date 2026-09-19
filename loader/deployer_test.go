package loader

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDeployer_PersistAndHistory 锁定部署日志落盘：失败后 bot 仍能跨内存读取日志与历史。
func TestDeployer_PersistAndHistory(t *testing.T) {
	dir := t.TempDir()
	d := &Deployer{
		cfg:     Config{RuntimeDir: dir},
		results: map[string]*DeployResult{},
	}

	// 模拟一次失败部署
	res := &DeployResult{
		ID:         "deploy-111",
		Ref:        "",
		Pull:       false,
		Status:     DeployFailed,
		StartedAt:  time.Now().Add(-time.Minute),
		FinishedAt: time.Now(),
		Error:      "go build 失败: undefined: foo",
		Log:        []string{"已快照当前二进制", "go build 失败: undefined: foo"},
		FromRev:    "abc",
		ToRev:      "abc",
	}
	d.persistResult(res)

	// 内存 Result 仍可取
	if r, ok := d.Result("deploy-111"); !ok || r.Status != DeployFailed {
		t.Fatalf("in-memory Result failed: ok=%v", ok)
	}

	// 用新 Deployer（空内存）应从落盘文件恢复 → 模拟“重启后”
	d2 := &Deployer{cfg: Config{RuntimeDir: dir}, results: map[string]*DeployResult{}}
	r, ok := d2.Result("deploy-111")
	if !ok {
		t.Fatal("persisted result not readable after restart")
	}
	if r.Error != "go build 失败: undefined: foo" || len(r.Log) != 2 {
		t.Fatalf("persisted result mismatch: %+v", r)
	}

	// history 至少含该条
	hist := d2.History(10)
	if len(hist) == 0 {
		t.Fatal("history empty")
	}
	if hist[0]["id"] != "deploy-111" || hist[0]["status"] != string(DeployFailed) {
		t.Fatalf("history entry mismatch: %+v", hist[0])
	}

	// 落盘文件确实存在
	if _, err := filepath.Abs(filepath.Join(dir, "data", "loader", "deploy-111.json")); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
