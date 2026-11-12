package bot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/mcp"
	"github.com/kasuganosora/thinkbot/toolperm"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// runBrowserMCPStdioFixture 是本地假 MCP（stdio JSON-RPC）。
// 环境变量控制慢握手、前 N 次挂起、以及把一段文本写到 stderr。
// 进程把自身序号写进调用结果，用来区分重连后是不是新进程。
func runBrowserMCPStdioFixture() {
	if msg := os.Getenv("TB_BMCP_STDERR"); msg != "" {
		_, _ = os.Stderr.WriteString(msg + "\n")
		if os.Getenv("TB_BMCP_STDERR_ONLY") == "1" {
			os.Exit(2)
		}
	}
	n := bumpBrowserFixtureCounter(os.Getenv("TB_BMCP_COUNTER"))
	if path := os.Getenv("TB_BMCP_PIDFILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	hangUntil, _ := strconv.Atoi(os.Getenv("TB_BMCP_HANG_UNTIL"))
	if n <= hangUntil {
		go io.Copy(io.Discard, os.Stdin)
		select {}
	}
	if d, err := time.ParseDuration(os.Getenv("TB_BMCP_DELAY")); err == nil && d > 0 {
		time.Sleep(d)
	}
	tools := []string{"navigate", "get_text"}
	if raw := strings.TrimSpace(os.Getenv("TB_BMCP_TOOLS")); raw != "" {
		tools = strings.Split(raw, ",")
	}

	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "browser-fixture", "version": "0"},
			}
		case "tools/list":
			list := make([]any, 0, len(tools))
			for _, name := range tools {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				list = append(list, map[string]any{
					"name":        name,
					"description": "fixture " + name,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			result = map[string]any{"tools": list}
		case "tools/call":
			result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": strconv.Itoa(n)}},
			}
		default:
			continue
		}
		raw, mErr := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result":  result,
		})
		if mErr != nil {
			return
		}
		_, _ = os.Stdout.Write(append(raw, '\n'))
	}
}

func bumpBrowserFixtureCounter(path string) int {
	if path == "" {
		return 1
	}
	lock := path + ".lock"
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = f.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer os.Remove(lock)

	n := 0
	if b, err := os.ReadFile(path); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
	return n
}

func TestBrowserBackoffIsCappedAndJittered(t *testing.T) {
	base := time.Second
	cap := 30 * time.Second
	if got := browserBackoff(1, base, cap, 0); got != 500*time.Millisecond {
		t.Fatalf("attempt 1 floor = %s", got)
	}
	if got := browserBackoff(1, base, cap, 1); got != time.Second {
		t.Fatalf("attempt 1 ceil = %s", got)
	}
	if got := browserBackoff(2, base, cap, 1); got != 2*time.Second {
		t.Fatalf("attempt 2 ceil = %s", got)
	}
	if got := browserBackoff(10, base, cap, 1); got != cap {
		t.Fatalf("attempt 10 must cap at %s, got %s", cap, got)
	}
	if got := browserBackoff(2, base, 1500*time.Millisecond, 1); got != 1500*time.Millisecond {
		t.Fatalf("cap below the exponential step = %s", got)
	}
	floor := browserBackoff(3, base, cap, 0)
	ceil := browserBackoff(3, base, cap, 1)
	if floor >= ceil || floor < 2*time.Second {
		t.Fatalf("jitter range = [%s, %s]", floor, ceil)
	}
}

func TestSanitizeBrowserErrStripsSecrets(t *testing.T) {
	const secret = "super-secret-token"
	err := errors.New(`context deadline exceeded: Get "https://api.example.test/bot123456789:AAEaaabbbcccdddeeefffggghhhiiijjkkll/mcp?token=` + secret + `"`)
	got := sanitizeBrowserErr(err, []string{secret})
	if strings.Contains(got, secret) || strings.Contains(got, "AAEaaabbbcccdddeeefffggghhhiiijjkkll") {
		t.Fatalf("sanitized error still has secrets: %s", got)
	}
	if !strings.Contains(got, "context deadline exceeded") {
		t.Fatalf("sanitized error dropped the cause: %s", got)
	}
}

func TestBrowserMCPStartDoesNotWaitForSlowHandshake(t *testing.T) {
	mgr, tm := newBrowserFixtureManager(t, map[string]string{
		"TB_BMCP_DELAY": "800ms",
	}, 5*time.Second)
	if err := tm.Register(staticTool("calculate", "add numbers")); err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	start := time.Now()
	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: zap.NewNop().Sugar(),
		BackoffBase: 20 * time.Millisecond, BackoffCap: 50 * time.Millisecond,
		WatchInterval: time.Second,
	})
	elapsed := time.Since(start)
	t.Cleanup(func() {
		s.Stop()
		_ = mgr.Close()
		waitGoroutines(t, before)
	})
	if elapsed > 250*time.Millisecond {
		t.Fatalf("start blocked for %s; browser MCP must not delay startup", elapsed)
	}
	st := s.Status()
	if st.State != BrowserMCPStatePending && st.State != BrowserMCPStateConnecting {
		t.Fatalf("state immediately after start = %q", st.State)
	}
	names := resolveNames(t, tm)
	if !names["calculate"] {
		t.Fatalf("other tools must be ready while browser MCP is still connecting: %v", names)
	}
	if names["browser__navigate"] || names["browser__get_text"] {
		t.Fatalf("browser tools registered before handshake finished: %v", names)
	}
}

func TestBrowserMCPRetryAfterHandshakeTimeoutRegistersOnce(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "counter")
	mgr, tm := newBrowserFixtureManager(t, map[string]string{
		"TB_BMCP_COUNTER":    counter,
		"TB_BMCP_HANG_UNTIL": "1",
	}, 350*time.Millisecond)
	before := runtime.NumGoroutine()
	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: zap.NewNop().Sugar(),
		BackoffBase: 160 * time.Millisecond, BackoffCap: 240 * time.Millisecond,
		WatchInterval: time.Second,
	})
	t.Cleanup(func() {
		s.Stop()
		_ = mgr.Close()
		waitGoroutines(t, before)
	})

	seenFail := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		st := s.Status()
		if st.State == BrowserMCPStateFailed && strings.Contains(st.LastError, "deadline") {
			seenFail = true
			if st.Attempts < 1 || st.NextRetry.IsZero() || !st.NextRetry.After(time.Now().Add(-time.Second)) {
				t.Fatalf("failed status = %+v", st)
			}
		}
		if st.State == BrowserMCPStateConnected && seenFail {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	st := s.Status()
	if !seenFail || st.State != BrowserMCPStateConnected {
		t.Fatalf("want timeout then connect, seenFail=%v status=%+v", seenFail, st)
	}
	if st.Attempts < 2 {
		t.Fatalf("attempts = %d, want at least 2", st.Attempts)
	}
	assertBrowserToolsOnce(t, tm)
	if s.installCount() != 1 {
		t.Fatalf("install count = %d, want 1", s.installCount())
	}
	if n := readFixtureCounter(counter); n != 2 {
		t.Fatalf("fixture processes = %d, want 2 (one timeout, one success)", n)
	}
}

func TestBrowserMCPConcurrentConnectRegistersOnce(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "counter")
	mgr, tm := newBrowserFixtureManager(t, map[string]string{
		"TB_BMCP_COUNTER": counter,
	}, time.Second)
	before := runtime.NumGoroutine()
	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: zap.NewNop().Sugar(),
		BackoffBase: 20 * time.Millisecond, BackoffCap: 40 * time.Millisecond,
		WatchInterval: time.Second,
	})
	t.Cleanup(func() {
		s.Stop()
		_ = mgr.Close()
		waitGoroutines(t, before)
	})

	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			errCh <- s.connectOnce(context.Background())
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent connect: %v", err)
		}
	}
	waitBrowserConnected(t, s, 3*time.Second)
	assertBrowserToolsOnce(t, tm)
	if s.installCount() != 1 {
		t.Fatalf("install count = %d, want 1", s.installCount())
	}
	if tm.ProviderCount() != 1 {
		t.Fatalf("providers = %d, want 1", tm.ProviderCount())
	}
	if n := readFixtureCounter(counter); n != 1 {
		t.Fatalf("fixture processes = %d, want 1", n)
	}
}

func TestBrowserMCPDisconnectReconnectReregistersOnce(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	pidfile := filepath.Join(dir, "pid")
	mgr, tm := newBrowserFixtureManager(t, map[string]string{
		"TB_BMCP_COUNTER": counter,
		"TB_BMCP_PIDFILE": pidfile,
	}, time.Second)
	before := runtime.NumGoroutine()
	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: zap.NewNop().Sugar(),
		BackoffBase: 250 * time.Millisecond, BackoffCap: 400 * time.Millisecond,
		WatchInterval: 30 * time.Millisecond,
	})
	t.Cleanup(func() {
		s.Stop()
		_ = mgr.Close()
		waitGoroutines(t, before)
	})
	waitBrowserConnected(t, s, 3*time.Second)
	if s.installCount() != 1 {
		t.Fatalf("installs after first connect = %d", s.installCount())
	}
	first := toolByName(t, tm, "browser__navigate")
	execCtx := &llm.ToolExecContext{Context: context.Background()}
	out, err := first.Execute(execCtx, map[string]any{})
	if err != nil || fmt.Sprint(out) != "1" {
		t.Fatalf("first call = %v err=%v", out, err)
	}

	pid := readFixturePID(t, pidfile)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill fixture: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var disconnected bool
	for time.Now().Before(deadline) {
		names := resolveNames(t, tm)
		if !names["browser__navigate"] {
			disconnected = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !disconnected {
		t.Fatal("tools stayed registered after the fixture process was killed")
	}
	if _, err := first.Execute(execCtx, map[string]any{}); err == nil || !strings.Contains(err.Error(), "disconnect") {
		t.Fatalf("stale tool must return a disconnect error, got %v", err)
	}

	waitBrowserConnected(t, s, 3*time.Second)
	assertBrowserToolsOnce(t, tm)
	if s.installCount() != 2 {
		t.Fatalf("install count after reconnect = %d, want 2", s.installCount())
	}
	if tm.ProviderCount() != 1 {
		t.Fatalf("providers = %d, want 1", tm.ProviderCount())
	}
	second := toolByName(t, tm, "browser__navigate")
	out, err = second.Execute(execCtx, map[string]any{})
	if err != nil || fmt.Sprint(out) != "2" {
		t.Fatalf("call after reconnect = %v err=%v, want the new process", out, err)
	}
}

func TestBrowserMCPStopDuringBackoff(t *testing.T) {
	const secret = "super-secret-token"
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	mgr := mcp.NewManager(zap.NewNop().Sugar())
	mgr.AddServer(mcp.ServerConfig{
		Name: "browser", Transport: "stdio", Command: secret, Enabled: true,
		HandshakeTimeout: 200 * time.Millisecond,
	})
	tm := tools.NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	before := runtime.NumGoroutine()
	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: logger,
		BackoffBase: 5 * time.Second, BackoffCap: 5 * time.Second,
		WatchInterval: time.Second,
		Redact:        []string{secret},
	})
	waitBrowserState(t, s, BrowserMCPStateFailed, 3*time.Second)
	st := s.Status()
	if strings.Contains(st.LastError, secret) {
		t.Fatalf("status leaked secret: %s", st.LastError)
	}
	if st.NextRetry.IsZero() {
		t.Fatal("failed state missing next retry time")
	}
	start := time.Now()
	s.Stop()
	if time.Since(start) > time.Second {
		t.Fatalf("stop during backoff took %s", time.Since(start))
	}
	_ = mgr.Close()
	waitGoroutines(t, before)
	if logsContain(logs, secret) {
		t.Fatal("logs leaked the command secret")
	}
}

func TestBrowserMCPStopDuringHandshake(t *testing.T) {
	mgr, tm := newBrowserFixtureManager(t, map[string]string{
		"TB_BMCP_HANG_UNTIL": "100",
	}, 30*time.Second)
	before := runtime.NumGoroutine()
	started := make(chan *browserMCPSession, 1)
	go func() {
		started <- startBrowserMCP(browserMCPConfig{
			Manager: mgr, Tools: tm, Logger: zap.NewNop().Sugar(),
			BackoffBase: time.Second, BackoffCap: time.Second,
			WatchInterval: time.Second,
		})
	}()
	var s *browserMCPSession
	select {
	case s = <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("start blocked on the in-flight handshake")
	}
	waitBrowserState(t, s, BrowserMCPStateConnecting, 2*time.Second)
	begin := time.Now()
	s.Stop()
	if time.Since(begin) > time.Second {
		t.Fatalf("stop during handshake took %s", time.Since(begin))
	}
	_ = mgr.Close()
	waitGoroutines(t, before)
}

func TestBrowserMCPStderrSecretNotLogged(t *testing.T) {
	const secret = "proxy-password-value"
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(zap.NewNop().Sugar())
	mgr.AddServer(mcp.ServerConfig{
		Name: "browser", Transport: "stdio", Command: bin,
		Args:    []string{browserMCPStdioFixtureArg},
		Env:     []string{"TB_BMCP_STDERR=" + secret, "TB_BMCP_STDERR_ONLY=1"},
		Enabled: true, HandshakeTimeout: time.Second,
		Stderr: &mcpStdioStderr{log: logger, redact: []string{secret}},
	})
	tm := tools.NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: logger,
		BackoffBase: 5 * time.Second, BackoffCap: 5 * time.Second,
		Redact: []string{secret},
	})
	t.Cleanup(func() {
		s.Stop()
		_ = mgr.Close()
	})
	waitBrowserState(t, s, BrowserMCPStateFailed, 3*time.Second)
	if logsContain(logs, secret) {
		t.Fatal("stderr log leaked the proxy secret")
	}
	if !logsContain(logs, "browser-mcp stderr") {
		t.Fatal("expected a sanitized stderr log line")
	}
}

func TestBrowserMCPHandshakeTimeoutReapsChildAndKeepsBackoff(t *testing.T) {
	const secret = "super-secret-token"
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core).Sugar()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(logger)
	mgr.AddServer(mcp.ServerConfig{
		Name: "browser", Transport: "stdio", Command: bin,
		Args: []string{browserMCPStdioFixtureArg},
		Env: []string{
			"TB_BMCP_PIDFILE=" + pidfile,
			"TB_BMCP_HANG_UNTIL=100",
			"TB_BMCP_STDERR=slow-start password=" + secret,
		},
		Enabled:          true,
		HandshakeTimeout: 300 * time.Millisecond,
		Redact:           []string{secret},
		Stderr:           &mcpStdioStderr{log: logger, redact: []string{secret}},
	})
	tm := tools.NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	before := runtime.NumGoroutine()
	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: logger,
		BackoffBase: 20 * time.Second, BackoffCap: 20 * time.Second,
		WatchInterval: time.Second,
		Redact:        []string{secret},
	})
	t.Cleanup(func() {
		s.Stop()
		_ = mgr.Close()
		waitGoroutines(t, before)
	})

	pid := readFixturePID(t, pidfile)
	waitBrowserState(t, s, BrowserMCPStateFailed, 3*time.Second)
	st := s.Status()
	if st.Attempts < 1 || st.NextRetry.Before(time.Now().Add(5*time.Second)) {
		t.Fatalf("timeout should still schedule backoff, status=%+v", st)
	}
	if strings.Contains(st.LastError, secret) {
		t.Fatalf("status leaked secret: %s", st.LastError)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still alive after handshake timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if logsContain(logs, secret) {
		t.Fatal("logs leaked stderr secret")
	}
	if !logsContain(logs, "slow-start") {
		t.Fatal("expected redacted stderr tail after handshake timeout")
	}
}

func TestBrowserMCPLateToolsRespectPermissionGlob(t *testing.T) {
	mgr, tm := newBrowserFixtureManager(t, nil, time.Second)
	if err := tm.Register(staticTool("calculate", "add numbers")); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatal(err)
	}
	perm := toolperm.NewService(db, zap.NewNop().Sugar())
	tm.SetAccessEvaluator(perm.NewEvaluator())
	enabled := true
	sortAllow, sortDeny := 0, 10
	if _, err := perm.CreateRule("bot-late", toolperm.RuleReq{
		Tool: "browser__get_text", Platform: "telegram", UserIDs: []string{"*"},
		Decision: toolperm.DecisionAllow, Enabled: &enabled, Sort: &sortAllow,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := perm.CreateRule("bot-late", toolperm.RuleReq{
		Tool: "browser__*", Platform: "telegram", UserIDs: []string{"*"},
		Decision: toolperm.DecisionDeny, Enabled: &enabled, Sort: &sortDeny,
	}); err != nil {
		t.Fatal(err)
	}

	s := startBrowserMCP(browserMCPConfig{
		Manager: mgr, Tools: tm, Logger: zap.NewNop().Sugar(),
		BackoffBase: 20 * time.Millisecond, BackoffCap: 40 * time.Millisecond,
	})
	t.Cleanup(func() {
		s.Stop()
		_ = mgr.Close()
	})
	waitBrowserConnected(t, s, 3*time.Second)

	names := resolveNamesFor(t, tm, &tools.ToolSessionContext{
		BotID: "bot-late", Channel: "telegram", SourceChannelType: "telegram",
		ChatType: "private", UserID: "u1", UserIdentifiers: []string{"u1"},
	})
	if !names["calculate"] {
		t.Fatalf("basic tool should stay allowed: %v", names)
	}
	if !names["browser__get_text"] {
		t.Fatalf("explicit allow should permit browser__get_text: %v", names)
	}
	if names["browser__navigate"] {
		t.Fatalf("browser__* deny should hide browser__navigate: %v", names)
	}
}

func newBrowserFixtureManager(t *testing.T, extra map[string]string, handshake time.Duration) (*mcp.Manager, *tools.ToolManager) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	logger := zap.NewNop().Sugar()
	mgr := mcp.NewManager(logger)
	mgr.AddServer(mcp.ServerConfig{
		Name: "browser", Transport: "stdio", Command: bin,
		Args: []string{browserMCPStdioFixtureArg}, Env: env, Enabled: true,
		HandshakeTimeout: handshake,
	})
	tm := tools.NewToolManager(prompt.NewRegistry(), nil, logger)
	return mgr, tm
}

func staticTool(name, desc string) tools.ToolDef {
	return tools.ToolDef{
		Tool: llm.Tool{
			Name: name, Description: desc,
			Parameters: map[string]any{"type": "object"},
		},
		Category: "basic",
	}
}

func resolveNames(t *testing.T, tm *tools.ToolManager) map[string]bool {
	t.Helper()
	return resolveNamesFor(t, tm, &tools.ToolSessionContext{BotID: "bot-x", ChatType: "private"})
}

func resolveNamesFor(t *testing.T, tm *tools.ToolManager, sctx *tools.ToolSessionContext) map[string]bool {
	t.Helper()
	list, err := tm.ResolveTools(context.Background(), sctx)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(list))
	for _, tool := range list {
		if names[tool.Name] {
			t.Fatalf("duplicate tool %q", tool.Name)
		}
		names[tool.Name] = true
	}
	return names
}

func assertBrowserToolsOnce(t *testing.T, tm *tools.ToolManager) {
	t.Helper()
	names := resolveNames(t, tm)
	if !names["browser__navigate"] || !names["browser__get_text"] {
		t.Fatalf("browser tools = %v", names)
	}
}

func toolByName(t *testing.T, tm *tools.ToolManager, name string) llm.Tool {
	t.Helper()
	list, err := tm.ResolveTools(context.Background(), &tools.ToolSessionContext{BotID: "bot-x", ChatType: "private"})
	if err != nil {
		t.Fatal(err)
	}
	var found llm.Tool
	n := 0
	for _, tool := range list {
		if tool.Name == name {
			found = tool
			n++
		}
	}
	if n != 1 {
		t.Fatalf("tool %q count = %d", name, n)
	}
	return found
}

func waitBrowserConnected(t *testing.T, s *browserMCPSession, d time.Duration) {
	t.Helper()
	waitBrowserState(t, s, BrowserMCPStateConnected, d)
}

func waitBrowserState(t *testing.T, s *browserMCPSession, state string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last BrowserMCPStatus
	for time.Now().Before(deadline) {
		last = s.Status()
		if last.State == state {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("state = %q, want %q (last=%+v)", last.State, state, last)
}

func waitGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutine leak: before=%d after=%d\n%s", before, runtime.NumGoroutine(), buf[:n])
}

func readFixtureCounter(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func readFixturePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if n, conv := strconv.Atoi(strings.TrimSpace(string(b))); conv == nil && n > 0 {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pid file was not written")
	return 0
}

func logsContain(logs *observer.ObservedLogs, needle string) bool {
	for _, e := range logs.All() {
		if strings.Contains(e.Message, needle) {
			return true
		}
		for _, f := range e.Context {
			if strings.Contains(f.String, needle) || strings.Contains(fmt.Sprint(f.Interface), needle) {
				return true
			}
		}
	}
	return false
}
