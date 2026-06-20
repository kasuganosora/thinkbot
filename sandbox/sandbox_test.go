package sandbox

import (
	"context"
	"encoding/base64"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// validatePath 测试
// ============================================================================

func TestValidatePath_NormalRelative(t *testing.T) {
	got, err := validatePath("/workspace", "src/main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/workspace/src/main.go" {
		t.Errorf("got %q, want /workspace/src/main.go", got)
	}
}

func TestValidatePath_EmptyPath(t *testing.T) {
	got, err := validatePath("/workspace", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/workspace" {
		t.Errorf("got %q, want /workspace", got)
	}
}

func TestValidatePath_DotPath(t *testing.T) {
	got, err := validatePath("/workspace", ".")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/workspace" {
		t.Errorf("got %q, want /workspace", got)
	}
}

func TestValidatePath_AbsoluteStripped(t *testing.T) {
	// 绝对路径应被去掉前导 / ，限制在 root 内
	got, err := validatePath("/workspace", "/etc/passwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/workspace/etc/passwd" {
		t.Errorf("got %q, want /workspace/etc/passwd", got)
	}
}

func TestDefaultConfig_HasTimezone(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Timezone != "UTC" {
		t.Errorf("expected default timezone 'UTC', got %q", cfg.Timezone)
	}
}

func TestFillDefaults_FillsTimezone(t *testing.T) {
	cfg := Config{Backend: "local"}
	filled := fillDefaults(cfg)
	if filled.Timezone != "UTC" {
		t.Errorf("expected filled timezone 'UTC', got %q", filled.Timezone)
	}
}

func TestFillDefaults_PreservesTimezone(t *testing.T) {
	cfg := Config{Backend: "local", Timezone: "Asia/Shanghai"}
	filled := fillDefaults(cfg)
	if filled.Timezone != "Asia/Shanghai" {
		t.Errorf("expected preserved timezone 'Asia/Shanghai', got %q", filled.Timezone)
	}
}

func TestValidatePath_BackslashNormalized(t *testing.T) {
	got, err := validatePath("/workspace", "src\\main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/workspace/src/main.go" {
		t.Errorf("got %q, want /workspace/src/main.go", got)
	}
}

func TestValidatePath_TraversalRejected(t *testing.T) {
	_, err := validatePath("/workspace", "../etc/passwd")
	if err == nil {
		t.Error("expected error for path traversal, got nil")
	}
}

func TestValidatePath_DeepTraversalRejected(t *testing.T) {
	_, err := validatePath("/workspace", "src/../../etc/passwd")
	if err == nil {
		t.Error("expected error for deep path traversal, got nil")
	}
}

func TestValidatePath_TraversalOnlyRejected(t *testing.T) {
	_, err := validatePath("/workspace", "..")
	if err == nil {
		t.Error("expected error for .. path, got nil")
	}
}

// ============================================================================
// Local 后端集成测试
// ============================================================================

func newTestLocalSandbox(t *testing.T) (Sandbox, func()) {
	t.Helper()
	sb, err := newLocalSandbox(Config{
		Backend:      "local",
		Timeout:      10 * time.Second,
		MaxOutput:    1024,
		MaxFileWrite: 1 << 20,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create local sandbox: %v", err)
	}
	return sb, func() { _ = sb.Close() }
}

func TestLocalSandbox_BackendType(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()
	if sb.Backend() != "local" {
		t.Errorf("Backend() = %q, want local", sb.Backend())
	}
}

func TestLocalSandbox_CreateAndClose(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	ws, err := sb.Create("test-create-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	if ws.ID() != "test-create-1" {
		t.Errorf("ID() = %q, want test-create-1", ws.ID())
	}
	if ws.WorkDir() == "" {
		t.Error("WorkDir() should not be empty")
	}
}

func TestLocalSandbox_ExecSuccess(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	ws, err := sb.Create("test-exec-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "echo hello_world",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello_world") {
		t.Errorf("Stdout = %q, should contain hello_world", result.Stdout)
	}
}

func TestLocalSandbox_ExecNonZeroExit(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	ws, err := sb.Create("test-exec-exit")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "exit 42",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", result.ExitCode)
	}
}

func TestLocalSandbox_ExecTimeout(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	ws, err := sb.Create("test-exec-timeout")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	// Windows: ping -n 10 127.0.0.1 >nul 是跨平台兼容的延迟方式
	// Unix: sleep 10
	cmd := "sleep 10"
	if runtime.GOOS == "windows" {
		cmd = "ping -n 10 127.0.0.1 >nul"
	}

	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: cmd,
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (timeout)", result.ExitCode)
	}
}

func TestLocalSandbox_WriteAndReadFile(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	ws, err := sb.Create("test-io-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	// Write
	data := []byte("test file content\nline 2")
	err = ws.WriteFile(context.Background(), "dir1/test.txt", data)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Read
	got, err := ws.ReadFile(context.Background(), "dir1/test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("Read content mismatch: got %q, want %q", string(got), string(data))
	}
}

func TestLocalSandbox_WriteFileTooLarge(t *testing.T) {
	sb, err := newLocalSandbox(Config{
		Backend:      "local",
		MaxFileWrite: 10,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create local sandbox: %v", err)
	}
	defer func() { _ = sb.Close() }()

	ws, err := sb.Create("test-write-limit")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	bigData := make([]byte, 100)
	err = ws.WriteFile(context.Background(), "big.txt", bigData)
	if err == nil {
		t.Error("expected error for oversized write, got nil")
	}
}

func TestLocalSandbox_ListDir(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	ws, err := sb.Create("test-list-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	// 准备文件和目录
	if err := ws.WriteFile(context.Background(), "file1.txt", []byte("a")); err != nil {
		t.Fatalf("WriteFile file1.txt failed: %v", err)
	}
	if err := ws.WriteFile(context.Background(), "file2.txt", []byte("bb")); err != nil {
		t.Fatalf("WriteFile file2.txt failed: %v", err)
	}
	if err := ws.WriteFile(context.Background(), "sub/file3.txt", []byte("ccc")); err != nil {
		t.Fatalf("WriteFile sub/file3.txt failed: %v", err)
	}

	// List 根目录
	entries, err := ws.ListDir(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
}

func TestLocalSandbox_PathTraversalRejected(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	ws, err := sb.Create("test-traversal")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = ws.Close() }()

	// ReadFile should reject ..
	_, err = ws.ReadFile(context.Background(), "../secret")
	if err == nil {
		t.Error("expected error reading ../secret")
	}

	// WriteFile should reject ..
	err = ws.WriteFile(context.Background(), "../evil.txt", []byte("bad"))
	if err == nil {
		t.Error("expected error writing ../evil.txt")
	}
}

// ============================================================================
// SandboxManager 测试
// ============================================================================

func TestSandboxManager_GetOrCreate(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	mgr := NewSandboxManager(sb, nil, 0)
	defer func() { _ = mgr.Close() }()

	key1 := SessionKey{BotID: "bot1", Channel: "ch1", UserID: "user1"}
	ws1, err := mgr.GetOrCreate(key1)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}

	// 再次获取应返回相同的工作空间
	ws1Again, err := mgr.GetOrCreate(key1)
	if err != nil {
		t.Fatalf("second GetOrCreate failed: %v", err)
	}
	if ws1.ID() != ws1Again.ID() {
		t.Error("expected same workspace instance for same session key")
	}

	if mgr.Count() != 1 {
		t.Errorf("Count = %d, want 1", mgr.Count())
	}
}

func TestSandboxManager_SessionIsolation(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	mgr := NewSandboxManager(sb, nil, 0)
	defer func() { _ = mgr.Close() }()

	key1 := SessionKey{BotID: "bot1", Channel: "ch1", UserID: "user1"}
	key2 := SessionKey{BotID: "bot1", Channel: "ch1", UserID: "user2"}

	ws1, err := mgr.GetOrCreate(key1)
	if err != nil {
		t.Fatalf("GetOrCreate key1 failed: %v", err)
	}
	ws2, err := mgr.GetOrCreate(key2)
	if err != nil {
		t.Fatalf("GetOrCreate key2 failed: %v", err)
	}

	if ws1.ID() == ws2.ID() {
		t.Error("expected different workspace IDs for different sessions")
	}
	if mgr.Count() != 2 {
		t.Errorf("Count = %d, want 2", mgr.Count())
	}

	// ws1 的文件不应在 ws2 中可见
	_ = ws1.WriteFile(context.Background(), "marker.txt", []byte("from-ws1"))
	_, err = ws2.ReadFile(context.Background(), "marker.txt")
	if err == nil {
		t.Error("file written in ws1 should not be readable in ws2")
	}
}

func TestSandboxManager_CloseWorkspace(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	mgr := NewSandboxManager(sb, nil, 0)

	key1 := SessionKey{BotID: "bot1", Channel: "ch1", UserID: "user1"}
	_, err := mgr.GetOrCreate(key1)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}
	if mgr.Count() != 1 {
		t.Fatalf("Count = %d, want 1", mgr.Count())
	}

	mgr.CloseWorkspace(key1)
	if mgr.Count() != 0 {
		t.Errorf("Count after CloseWorkspace = %d, want 0", mgr.Count())
	}
}

// ============================================================================
// Factory 测试
// ============================================================================

func TestNewSandbox_ForcedLocal(t *testing.T) {
	sb, err := NewSandbox(Config{Backend: "local"}, nil)
	if err != nil {
		t.Fatalf("NewSandbox failed: %v", err)
	}
	defer func() { _ = sb.Close() }()
	if sb.Backend() != "local" {
		t.Errorf("Backend = %q, want local", sb.Backend())
	}
}

func TestNewSandbox_InvalidBackend(t *testing.T) {
	_, err := NewSandbox(Config{Backend: "invalid"}, nil)
	if err == nil {
		t.Error("expected error for invalid backend")
	}
}

// 无 Docker 时 auto 模式应降级到 local（不设 RequireDocker）
func TestNewSandbox_AutoFallbackToLocal(t *testing.T) {
	// auto 模式：Docker 不可用时应降级到 local
	sb, err := NewSandbox(Config{Backend: "auto"}, nil)
	if err != nil {
		t.Fatalf("auto mode should not fail when Docker unavailable: %v", err)
	}
	defer func() { _ = sb.Close() }()
	// 可能是 docker 也可能是 local，取决于环境
	// 关键是：不报错
}

// RequireDocker 为 true 且 Docker 不可用时应报错
func TestNewSandbox_RequireDockerNoDocker(t *testing.T) {
	// 如果当前环境恰好有 Docker，这个测试验证不了降级逻辑
	// 用 docker backend 强制模式验证错误处理
	// 这里用 auto + RequireDocker：如果没有 Docker 就应该报错
	// 但无法确定当前环境是否有 Docker，所以只验证 RequireDocker 字段被读取
	cfg := Config{Backend: "auto", RequireDocker: true}
	_, err := NewSandbox(cfg, nil)
	if dockerAvailable() && err != nil {
		t.Fatalf("Docker available but RequireDocker returned error: %v", err)
	}
	if !dockerAvailable() && err == nil {
		t.Error("expected error when RequireDocker=true and Docker not available")
	}
}

// ============================================================================
// SetupSandbox 组合根测试
// ============================================================================

func TestSetupSandbox_Local(t *testing.T) {
	result, err := SetupSandbox(WireConfig{
		Config: Config{Backend: "local"},
	})
	if err != nil {
		t.Fatalf("SetupSandbox failed: %v", err)
	}
	defer func() { _ = result.Close() }()

	if result.Manager == nil {
		t.Fatal("Manager should not be nil")
	}
	if result.Manager.Backend() != "local" {
		t.Errorf("Backend = %q, want local", result.Manager.Backend())
	}
}

// ============================================================================
// parseListOutput 测试
// ============================================================================

func TestParseListOutput(t *testing.T) {
	input := "file1.txt\tf\t100\nsubdir\td\t0\nfile2.txt\tf\t2048\n"
	entries := parseListOutput([]byte(input))

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Name != "file1.txt" || entries[0].IsDir || entries[0].Size != 100 {
		t.Errorf("entry 0 unexpected: %+v", entries[0])
	}
	if entries[1].Name != "subdir" || !entries[1].IsDir {
		t.Errorf("entry 1 unexpected: %+v", entries[1])
	}
	if entries[2].Size != 2048 {
		t.Errorf("entry 2 size = %d, want 2048", entries[2].Size)
	}
}

func TestParseListOutput_Empty(t *testing.T) {
	entries := parseListOutput([]byte(""))
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

// ============================================================================
// 工具执行测试（通过 Manager 验证端到端）
// ============================================================================

func TestSandboxTool_ExecViaManager(t *testing.T) {
	sb, cleanup := newTestLocalSandbox(t)
	defer cleanup()

	mgr := NewSandboxManager(sb, nil, 0)
	defer func() { _ = mgr.Close() }()

	key := SessionKey{BotID: "bot1", Channel: "test", UserID: "user1"}
	ws, err := mgr.GetOrCreate(key)
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}

	// 写入文件
	err = ws.WriteFile(context.Background(), "hello.txt", []byte("hello from manager"))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// 通过 exec 读取
	result, err := ws.Exec(context.Background(), ExecRequest{
		Command: "cat hello.txt",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello from manager") {
		t.Errorf("Stdout = %q, should contain file content", result.Stdout)
	}
}

// ============================================================================
// context session key 测试
// ============================================================================

func TestContextWithSessionKey(t *testing.T) {
	key := SessionKey{BotID: "bot1", Channel: "ch1", UserID: "u1"}
	ctx := ContextWithSessionKey(context.Background(), key)

	got := SessionKeyFromContext(ctx)
	if got != key {
		t.Errorf("SessionKeyFromContext = %+v, want %+v", got, key)
	}
}

func TestSessionKeyFromContext_Empty(t *testing.T) {
	got := SessionKeyFromContext(context.Background())
	if got.BotID != "" || got.Channel != "" || got.UserID != "" {
		t.Error("expected empty SessionKey from plain context")
	}
}

// 辅助：base64 编码验证（确保工具层的编码逻辑正确）
func TestBase64RoundTrip(t *testing.T) {
	original := []byte("test content with 中文 and symbols !@#")
	encoded := base64.StdEncoding.EncodeToString(original)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if string(decoded) != string(original) {
		t.Error("base64 round trip mismatch")
	}
}
