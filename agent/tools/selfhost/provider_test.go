package selfhost

import (
	"path/filepath"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/config"
	"go.uber.org/zap"
)

func newStore(t *testing.T, kvs map[string]string) *config.Store {
	t.Helper()
	s := config.NewStore(nil)
	for k, v := range kvs {
		s.SetTemporary(k, v)
	}
	return s
}

func TestProvider_Gating(t *testing.T) {
	// 关闭时不注册任何工具
	p := NewProvider(newStore(t, map[string]string{"loader.enabled": "false"}), zap.NewNop().Sugar())
	list, err := p.Tools(t.Context(), &tools.ToolSessionContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if list != nil {
		t.Fatalf("expected nil tools when disabled, got %d", len(list))
	}

	// 开启时注册全部 9 个工具
	p = NewProvider(newStore(t, map[string]string{"loader.enabled": "true"}), zap.NewNop().Sugar())
	list, err = p.Tools(t.Context(), &tools.ToolSessionContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(list) != 9 {
		t.Fatalf("expected 9 tools when enabled, got %d", len(list))
	}
	want := map[string]bool{
		"tb_loader_status": true, "tb_deploy": true, "tb_deploy_status": true,
		"tb_deploy_history": true, "tb_logs": true, "tb_read_source": true,
		"tb_write_source": true, "tb_rollback": true, "tb_restart": true,
	}
	for _, tl := range list {
		if !want[tl.Name] {
			t.Errorf("unexpected tool %s", tl.Name)
		}
		delete(want, tl.Name)
	}
	if len(want) > 0 {
		t.Errorf("missing tools: %v", want)
	}
}

func TestProvider_SafePath(t *testing.T) {
	dir := t.TempDir()
	p := NewProvider(newStore(t, map[string]string{
		"loader.enabled": "true",
		"loader.git_dir": dir,
	}), zap.NewNop().Sugar())

	// 正常相对路径（允许 .git 读）
	fp, err := p.safePath("agent/x.go", true)
	if err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if fp != filepath.Join(dir, "agent", "x.go") {
		t.Fatalf("unexpected resolved path %q", fp)
	}

	// 穿越到父目录之外 → 拒绝
	if _, err := p.safePath("../../etc/passwd", true); err == nil {
		t.Fatal("path traversal should be rejected")
	}

	// 写操作禁止进入 .git
	if _, err := p.safePath(".git/config", false); err == nil {
		t.Fatal("writing into .git should be rejected")
	}
}
