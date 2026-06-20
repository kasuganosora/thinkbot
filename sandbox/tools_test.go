package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// newTestBotMgr 创建用于工具测试的 BotWorkspaceManager（local 后端）。
func newTestBotMgr(t *testing.T) (*BotWorkspaceManager, string, func()) {
	t.Helper()
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "workspaces")
	mgr, err := NewBotWorkspaceManager(baseDir, Config{
		Backend:      "local",
		Timeout:      10 * time.Second,
		MaxOutput:    1 << 16,
		MaxFileWrite: 1 << 20,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create bot workspace manager: %v", err)
	}
	botID := "test-bot"
	return mgr, botID, func() { mgr.Close() }
}

// getBotWS 获取测试 bot 工作空间。
func getBotWS(t *testing.T, mgr *BotWorkspaceManager, botID string) Workspace {
	t.Helper()
	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}
	return ws
}

func TestTool_Exec(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "echo hello_tools",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello_tools") {
		t.Errorf("stdout = %q", result.Stdout)
	}
}

func TestTool_ReadFile_WithLineNumbers(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	content := "line1\nline2\nline3\nline4\nline5"
	err := ws.WriteFile(context.Background(), "test.txt", []byte(content))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	data, err := ws.ReadFile(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
	if lines[2] != "line3" {
		t.Errorf("line 3 = %q", lines[2])
	}
}

func TestTool_ReplaceInFile(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	original := "package main\n\nfunc hello() {\n\tprintln(\"hello\")\n}\n"
	err := ws.WriteFile(context.Background(), "main.go", []byte(original))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	data, err := ws.ReadFile(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	oldStr := `println("hello")`
	newStr := `fmt.Println("hello, world")`

	count := strings.Count(string(data), oldStr)
	if count != 1 {
		t.Fatalf("old_str count = %d, want 1", count)
	}

	newContent := strings.Replace(string(data), oldStr, newStr, 1)
	err = ws.WriteFile(context.Background(), "main.go", []byte(newContent))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, _ := ws.ReadFile(context.Background(), "main.go")
	if !strings.Contains(string(got), `fmt.Println("hello, world")`) {
		t.Errorf("replacement not found")
	}
	if strings.Contains(string(got), `println("hello")`) {
		t.Errorf("old content still present")
	}
}

func TestTool_ReplaceInFile_NotFound(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	err := ws.WriteFile(context.Background(), "a.txt", []byte("hello world"))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	data, _ := ws.ReadFile(context.Background(), "a.txt")
	count := strings.Count(string(data), "nonexistent")
	if count != 0 {
		t.Errorf("expected 0 matches, got %d", count)
	}
}

func TestTool_ReplaceInFile_MultipleMatches(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	err := ws.WriteFile(context.Background(), "dup.txt", []byte("foo\nfoo\nfoo"))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	data, _ := ws.ReadFile(context.Background(), "dup.txt")
	count := strings.Count(string(data), "foo")
	if count != 3 {
		t.Errorf("expected 3 matches, got %d", count)
	}
}

func TestTool_DeleteFile(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	err := ws.WriteFile(context.Background(), "todelete.txt", []byte("bye"))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "rm -rf -- 'todelete.txt'",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("rm exit code = %d", result.ExitCode)
	}

	_, err = ws.ReadFile(context.Background(), "todelete.txt")
	if err == nil {
		t.Error("file should be deleted")
	}
}

func TestTool_MoveFile(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	err := ws.WriteFile(context.Background(), "old_name.txt", []byte("content"))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "mv -- 'old_name.txt' 'new_name.txt'",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("mv exit code = %d: %s", result.ExitCode, result.Stderr)
	}

	got, err := ws.ReadFile(context.Background(), "new_name.txt")
	if err != nil {
		t.Fatalf("ReadFile new_name failed: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("content = %q", string(got))
	}

	_, err = ws.ReadFile(context.Background(), "old_name.txt")
	if err == nil {
		t.Error("old file should not exist")
	}
}

func TestTool_SearchContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("grep not available on Windows local backend")
	}
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	ws.WriteFile(context.Background(), "a.txt", []byte("hello world\nfoo bar\nHELLO again"))
	ws.WriteFile(context.Background(), "sub/b.txt", []byte("hello from sub\nnothing here"))

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "grep -rni -- 'hello' '.' 2>/dev/null || true",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Stdout), "hello") {
		t.Errorf("grep output should contain 'hello': %q", result.Stdout)
	}
}

func TestTool_WriteFile_Overwrite(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	ws.WriteFile(context.Background(), "ow.txt", []byte("original content"))
	ws.WriteFile(context.Background(), "ow.txt", []byte("new content"))

	got, _ := ws.ReadFile(context.Background(), "ow.txt")
	if string(got) != "new content" {
		t.Errorf("content = %q, want 'new content'", string(got))
	}
}

func TestTool_WriteFile_AutoMkdir(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	err := ws.WriteFile(context.Background(), "deep/nested/dir/file.txt", []byte("deep"))
	if err != nil {
		t.Fatalf("WriteFile with nested path failed: %v", err)
	}

	got, _ := ws.ReadFile(context.Background(), "deep/nested/dir/file.txt")
	if string(got) != "deep" {
		t.Errorf("content = %q", string(got))
	}
}

func TestTool_ReadFile_Empty(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	ws.WriteFile(context.Background(), "empty.txt", []byte(""))

	got, err := ws.ReadFile(context.Background(), "empty.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(got))
	}
}

func TestTool_BotWorkspaceToolDefs_Count(t *testing.T) {
	mgr, _, cleanup := newTestBotMgr(t)
	defer cleanup()

	defs := botWorkspaceToolDefs(mgr, "test-bot")
	if len(defs) != 9 {
		t.Errorf("expected 9 tools, got %d", len(defs))
	}

	expectedNames := map[string]bool{
		"sandbox_exec":            true,
		"sandbox_read_file":       true,
		"sandbox_write_file":      true,
		"sandbox_replace_in_file": true,
		"sandbox_delete_file":     true,
		"sandbox_move_file":       true,
		"sandbox_list_dir":        true,
		"sandbox_search_content":  true,
		"sandbox_health":          true,
	}
	for _, tool := range defs {
		if !expectedNames[tool.Name] {
			t.Errorf("unexpected tool name: %s", tool.Name)
		}
		delete(expectedNames, tool.Name)
	}
	if len(expectedNames) > 0 {
		t.Errorf("missing tools: %v", expectedNames)
	}
}

func TestTool_toInt(t *testing.T) {
	tests := []struct {
		input any
		want  int
		ok    bool
	}{
		{42, 42, true},
		{int64(42), 42, true},
		{float64(42.0), 42, true},
		{"42", 0, false},
		{nil, 0, false},
	}
	for _, tt := range tests {
		got, ok := toInt(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Errorf("toInt(%v) = (%d, %v), want (%d, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

// --- BotWorkspaceManager 持久化测试 ---

func TestBotWorkspace_Persistence(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "workspaces")

	// 第一次创建：写入文件
	mgr1, err := NewBotWorkspaceManager(baseDir, Config{
		Backend: "local",
		Timeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager: %v", err)
	}

	ws1, err := mgr1.GetOrCreate("persist-bot")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	err = ws1.WriteFile(context.Background(), "SOUL.md", []byte("# My Soul\nI am persistent."))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mgr1.Close()

	// 验证文件在磁盘上存在
	soulPath := filepath.Join(baseDir, "persist-bot", "SOUL.md")
	data, err := os.ReadFile(soulPath)
	if err != nil {
		t.Fatalf("file should exist on disk after Close: %v", err)
	}
	if !strings.Contains(string(data), "I am persistent") {
		t.Errorf("content mismatch: %q", string(data))
	}

	// 第二次创建：读取文件，验证持久化
	mgr2, err := NewBotWorkspaceManager(baseDir, Config{
		Backend: "local",
		Timeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager (2nd): %v", err)
	}
	defer mgr2.Close()

	ws2, err := mgr2.GetOrCreate("persist-bot")
	if err != nil {
		t.Fatalf("GetOrCreate (2nd): %v", err)
	}

	data2, err := ws2.ReadFile(context.Background(), "SOUL.md")
	if err != nil {
		t.Fatalf("ReadFile after recreate: %v", err)
	}
	if !strings.Contains(string(data2), "I am persistent") {
		t.Errorf("content mismatch after recreate: %q", string(data2))
	}
}

func TestBotWorkspace_BotIsolation(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "workspaces")

	mgr, err := NewBotWorkspaceManager(baseDir, Config{
		Backend: "local",
		Timeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager: %v", err)
	}
	defer mgr.Close()

	// bot-a 和 bot-b 应该有各自独立的工作空间
	wsA, _ := mgr.GetOrCreate("bot-a")
	wsB, _ := mgr.GetOrCreate("bot-b")

	wsA.WriteFile(context.Background(), "secret.txt", []byte("bot-a secret"))
	wsB.WriteFile(context.Background(), "secret.txt", []byte("bot-b secret"))

	// bot-a 不能看到 bot-b 的文件
	dataA, _ := wsA.ReadFile(context.Background(), "secret.txt")
	if string(dataA) != "bot-a secret" {
		t.Errorf("bot-a content = %q", string(dataA))
	}

	dataB, _ := wsB.ReadFile(context.Background(), "secret.txt")
	if string(dataB) != "bot-b secret" {
		t.Errorf("bot-b content = %q", string(dataB))
	}

	// 验证目录隔离
	dirA := wsA.WorkDir()
	dirB := wsB.WorkDir()
	if dirA == dirB {
		t.Errorf("bot-a and bot-b should have different directories: %s", dirA)
	}
}

func TestBotWorkspace_BotDir(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "workspaces")

	mgr, err := NewBotWorkspaceManager(baseDir, Config{
		Backend: "local",
		Timeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewBotWorkspaceManager: %v", err)
	}
	defer mgr.Close()

	botDir, err := mgr.BotDir("soul-bot")
	if err != nil {
		t.Fatalf("BotDir: %v", err)
	}

	// 验证目录存在
	info, err := os.Stat(botDir)
	if err != nil {
		t.Fatalf("BotDir should exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("BotDir should be a directory")
	}

	// SOUL.md 路径
	soulPath := filepath.Join(botDir, "SOUL.md")
	if !strings.HasSuffix(soulPath, filepath.Join("workspaces", "soul-bot", "SOUL.md")) {
		t.Errorf("unexpected soul path: %s", soulPath)
	}
}

func TestBotWorkspace_GetOrCreate_Reuse(t *testing.T) {
	mgr, _, cleanup := newTestBotMgr(t)
	defer cleanup()

	// 同一 botID 多次调用应返回同一工作空间
	ws1, _ := mgr.GetOrCreate("reuse-bot")
	ws2, _ := mgr.GetOrCreate("reuse-bot")

	if ws1 != ws2 {
		t.Error("GetOrCreate should return the same workspace for same botID")
	}
}

// --- HealthCheck 测试 ---

func TestHealthCheck_BotWorkspace_Healthy(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	status := ws.HealthCheck(context.Background())
	if !status.Healthy {
		t.Errorf("expected healthy, got %+v", status)
	}
	if status.Backend != "local" {
		t.Errorf("expected backend 'local', got %q", status.Backend)
	}
	if status.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", status.Status)
	}
}

func TestHealthCheck_BotWorkspace_NotCreated(t *testing.T) {
	mgr, _, cleanup := newTestBotMgr(t)
	defer cleanup()

	status := mgr.HealthCheck(context.Background(), "nonexistent-bot")
	if status.Healthy {
		t.Error("expected unhealthy for non-created workspace")
	}
	if status.Status != "not-created" {
		t.Errorf("expected status 'not-created', got %q", status.Status)
	}
}

func TestHealthCheck_BotWorkspace_DirDeleted(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	ws := getBotWS(t, mgr, botID)

	// 删除工作空间目录模拟异常
	os.RemoveAll(ws.WorkDir())

	status := ws.HealthCheck(context.Background())
	if status.Healthy {
		t.Error("expected unhealthy after dir deletion")
	}
	if status.Status != "not-found" {
		t.Errorf("expected status 'not-found', got %q", status.Status)
	}
}

func TestHealthCheck_Manager_All(t *testing.T) {
	mgr, _, cleanup := newTestBotMgr(t)
	defer cleanup()

	// 创建两个 bot 工作空间
	mgr.GetOrCreate("bot-a")
	mgr.GetOrCreate("bot-b")

	all := mgr.HealthCheckAll(context.Background())
	if len(all) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(all))
	}

	for id, status := range all {
		if !status.Healthy {
			t.Errorf("bot %q unhealthy: %+v", id, status)
		}
	}
}

func TestHealthCheck_Tool(t *testing.T) {
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	getBotWS(t, mgr, botID)

	// 通过工具定义执行健康检查
	tool := buildHealthTool(mgr, botID)
	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{},
	)
	if err != nil {
		t.Fatalf("health tool failed: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if healthy, _ := m["healthy"].(bool); !healthy {
		t.Errorf("expected healthy=true, got %+v", m)
	}
	if backend, _ := m["backend"].(string); backend != "local" {
		t.Errorf("expected backend 'local', got %q", backend)
	}
}
