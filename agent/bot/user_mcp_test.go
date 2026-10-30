package bot

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/mcp"
	"github.com/kasuganosora/thinkbot/secret"
)

const userMCPStdioFixtureArg = "--user-mcp-stdio-fixture"

func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if arg == userMCPStdioFixtureArg {
			runUserMCPStdioFixture()
			return
		}
	}
	os.Exit(m.Run())
}

// runUserMCPStdioFixture 是一个最小 MCP stdio 服务器：应答 initialize，然后一直活着。
func runUserMCPStdioFixture() {
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
		if req.Method != "initialize" {
			continue
		}
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result": map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fixture", "version": "0"},
			},
		}
		raw, mErr := json.Marshal(resp)
		if mErr != nil {
			return
		}
		_, _ = os.Stdout.Write(append(raw, '\n'))
	}
}

func TestMCPServerTool_AddListRemove_PersistsAndHotMounts(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	logger := zap.NewNop().Sugar()
	mgr := mcp.NewManager(logger)
	called := 0
	mgr.SetOnServerChange(func() { called++ })

	tool := NewMCPServerTool("bot-x", mgr, logger)
	execCtx := &llm.ToolExecContext{Context: context.Background()}
	sctx := tools.ToolSessionContext{BotID: "bot-x", ChatType: "private", UserID: "u1"}
	execCtx.Context = tools.ContextWithSessionContext(execCtx.Context, sctx)

	out, err := tool.Execute(execCtx, map[string]any{
		"action":  "add",
		"name":    "demo",
		"type":    "http",
		"url":     "https://example.invalid/mcp",
		"enabled": true,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok || m["name"] != "demo" {
		t.Fatalf("unexpected out: %#v", out)
	}
	entries, err := mcp.LoadBotServers("bot-x")
	if err != nil || len(entries) != 1 || entries[0].Name != "demo" {
		t.Fatalf("persist failed: %v %+v", err, entries)
	}
	if called < 1 {
		t.Fatalf("expected onServerChange after add, got %d", called)
	}

	listed, err := tool.Execute(execCtx, map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	lm := listed.(map[string]any)
	if lm["count"].(int) != 1 {
		t.Fatalf("list count: %+v", lm)
	}

	if _, err := tool.Execute(execCtx, map[string]any{"action": "remove", "name": "demo"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	entries, _ = mcp.LoadBotServers("bot-x")
	if len(entries) != 0 {
		t.Fatalf("expected empty after remove, got %+v", entries)
	}
}

// TestMCPServerTool_Scopes 固化 mcp_server 的适用场景：私聊与群聊都要暴露，
// 但子代理绝不暴露。
//
// 回归背景（2026-10-26）：Scopes 曾只写 "private"，管理员在群里让 bot 加 MCP
// 时工具列表里根本没有 mcp_server，tool_search 一律 no match，而工具本身注册
// 正常 —— 静默失效，只能靠翻规则表反推。可用性门槛必须被测试钉住。
func TestMCPServerTool_Scopes(t *testing.T) {
	tool := NewMCPServerTool("bot-x", mcp.NewManager(zap.NewNop().Sugar()), zap.NewNop().Sugar())

	has := func(scope string) bool {
		for _, s := range tool.Scopes {
			if s == scope {
				return true
			}
		}
		return false
	}
	if !has("private") {
		t.Error("mcp_server must be available in private chats")
	}
	if !has("group") {
		t.Error("mcp_server must be available in group chats")
	}
	if has("subagent") {
		t.Error("mcp_server must NOT be exposed to subagents")
	}
}

func TestMCPServerTool_RejectsSystemAndSubagent(t *testing.T) {
	mgr := mcp.NewManager(zap.NewNop().Sugar())
	tool := NewMCPServerTool("bot-x", mgr, zap.NewNop().Sugar())

	sysCtx := &llm.ToolExecContext{Context: context.Background()}
	sysCtx.Context = tools.ContextWithSessionContext(sysCtx.Context, tools.ToolSessionContext{IsSystem: true})
	if _, err := tool.Execute(sysCtx, map[string]any{"action": "list"}); err == nil {
		t.Fatal("system session must be refused")
	}

	saCtx := &llm.ToolExecContext{Context: context.Background()}
	saCtx.Context = tools.ContextWithSessionContext(saCtx.Context, tools.ToolSessionContext{IsSubagent: true})
	if _, err := tool.Execute(saCtx, map[string]any{"action": "list"}); err == nil {
		t.Fatal("subagent must be refused")
	}
}

func chdirTemp(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
}

// TestSetupUserMCPStdioSurvivesStartupBudget 验证 setupUserMCP 返回、启动预算
// ctx 被 cancel 之后，已经连上的 stdio MCP 子进程仍然存活。
func TestSetupUserMCPStdioSurvivesStartupBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("requires subprocess")
	}
	chdirTemp(t)

	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	now := mcp.NowRFC3339()
	if err := mcp.SaveBotServers("bot-stdio", []mcp.BotServerEntry{{
		ID:        "mcp_fix",
		Name:      "fixture",
		Type:      "stdio",
		Command:   bin,
		Args:      []string{userMCPStdioFixtureArg},
		Env:       map[string]string{},
		Headers:   map[string]string{},
		Enabled:   true,
		Status:    "running",
		CreatedAt: now,
		UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	logger := zap.NewNop().Sugar()
	b := &Bot{ID: "bot-stdio", logger: logger}
	setupUserMCP(b, BotParams{
		ID:          "bot-stdio",
		ToolManager: tools.NewToolManager(prompt.NewRegistry(), nil, logger),
	})
	t.Cleanup(func() {
		if b.userMCP != nil {
			_ = b.userMCP.Close()
		}
	})

	mgr := b.UserMCP()
	if mgr == nil || !mgr.IsServerConnected("fixture") {
		t.Fatal("expected stdio fixture to be connected after setup")
	}
	client, ok := mgr.GetClient("fixture")
	if !ok {
		t.Fatal("missing fixture client")
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !client.IsHealthy() {
			t.Fatal("stdio MCP died after setupUserMCP returned; startup budget cancel must not kill the child")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSetupUserMCPBudgetSkipMarksStatus 验证启动预算耗尽时，被跳过的服务器
// 名字会记入日志，且持久化状态不再是过期的 running。
func TestSetupUserMCPBudgetSkipMarksStatus(t *testing.T) {
	chdirTemp(t)
	prev := mcpStartupBudget
	mcpStartupBudget = -time.Millisecond
	t.Cleanup(func() { mcpStartupBudget = prev })

	now := mcp.NowRFC3339()
	if err := mcp.SaveBotServers("bot-budget", []mcp.BotServerEntry{
		{ID: "1", Name: "alpha", Type: "stdio", Command: "sleep", Args: []string{"30"}, Env: map[string]string{}, Headers: map[string]string{}, Enabled: true, Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "2", Name: "beta", Type: "http", URL: "http://127.0.0.1:1", Env: map[string]string{}, Headers: map[string]string{}, Enabled: true, Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "3", Name: "off", Type: "stdio", Command: "sleep", Env: map[string]string{}, Headers: map[string]string{}, Enabled: false, Status: "disabled", CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core).Sugar()
	b := &Bot{ID: "bot-budget", logger: logger}
	setupUserMCP(b, BotParams{
		ID:          "bot-budget",
		ToolManager: tools.NewToolManager(prompt.NewRegistry(), nil, logger),
	})
	t.Cleanup(func() {
		if b.userMCP != nil {
			_ = b.userMCP.Close()
		}
	})

	got, err := mcp.LoadBotServers("bot-budget")
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, e := range got {
		status[e.Name] = e.Status
	}
	if status["alpha"] != "error" || status["beta"] != "error" {
		t.Fatalf("skipped servers should not stay running: %+v", status)
	}
	if status["off"] != "disabled" {
		t.Fatalf("disabled server status changed: %+v", status)
	}
	if b.UserMCP() != nil && (b.UserMCP().IsServerConnected("alpha") || b.UserMCP().IsServerConnected("beta")) {
		t.Fatal("budget-skipped servers must not be connected")
	}

	var skipped []string
	for _, entry := range logs.All() {
		if !strings.Contains(entry.Message, "skipping remaining") {
			continue
		}
		skipped = append(skipped, fieldStrings(entry.ContextMap()["skipped"])...)
	}
	if !hasAll(skipped, "alpha", "beta") {
		t.Fatalf("skipped server names = %v", skipped)
	}
}

func fieldStrings(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

func hasAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, s := range got {
		set[s] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// withMCPSecretVault 用一个内存 Vault 替换 Default，测试结束恢复。
func withMCPSecretVault(t *testing.T, name, value string) {
	t.Helper()
	v, err := secret.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Register(name, value, "mcp 敏感字段测试"); err != nil {
		t.Fatal(err)
	}
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
}

// TestValidateMCPSecretRefs 覆盖 P1-a 的三条分支：裸明文拒绝、已登记占位符
// （经 Resolve 还原为真实值）放行、非敏感键放行；并断言错误文本不回显明文。
func TestValidateMCPSecretRefs(t *testing.T) {
	const realValue = "REAL_MCP_TOKEN_VALUE_abcdefghijklmnop"
	withMCPSecretVault(t, "mcp.token", realValue)

	// 模拟咽喉点 Resolve：合规占位符 {{secret:mcp.token}} 已还原为真实值。
	t.Run("registered placeholder resolved value allowed", func(t *testing.T) {
		err := validateMCPSecretRefs(
			map[string]string{"API_TOKEN": realValue},
			map[string]string{"Authorization": "Bearer " + realValue},
		)
		if err != nil {
			t.Fatalf("registered (resolved) value should be allowed: %v", err)
		}
	})

	// 未登记裸明文：Redact(v)==v → 拒绝，且错误文本不得含明文。
	t.Run("bare plaintext rejected without leaking value", func(t *testing.T) {
		const bare = "BARE_UNREGISTERED_SECRET_zzzzzzzz"
		err := validateMCPSecretRefs(
			map[string]string{},
			map[string]string{"Authorization": bare},
		)
		if err == nil {
			t.Fatal("bare plaintext on sensitive key must be rejected")
		}
		if strings.Contains(err.Error(), bare) {
			t.Fatalf("error text must not leak the plaintext value: %v", err)
		}
	})

	// 非敏感键的任意明文放行（不会承载票据）。
	t.Run("non-sensitive keys allowed", func(t *testing.T) {
		err := validateMCPSecretRefs(
			map[string]string{"NODE_ENV": "production", "PATH": "/usr/bin"},
			map[string]string{"Content-Type": "application/json"},
		)
		if err != nil {
			t.Fatalf("non-sensitive keys should be allowed: %v", err)
		}
	})

	// 敏感键空值放行。
	t.Run("empty sensitive value allowed", func(t *testing.T) {
		err := validateMCPSecretRefs(
			map[string]string{"API_TOKEN": ""},
			map[string]string{"Authorization": ""},
		)
		if err != nil {
			t.Fatalf("empty sensitive values should be allowed: %v", err)
		}
	})
}

// TestMCPServerTool_AddRejectsBarePlaintext 端到端：通过工具 add 动作传入
// 敏感键裸明文时，addMCPServerForTool 在执行前拒绝，且不落盘。
func TestMCPServerTool_AddRejectsBarePlaintext(t *testing.T) {
	chdirTemp(t)
	// Vault 里登记的值与被测明文不同 → 被测明文 Redact 命中不了 → 必被拒。
	withMCPSecretVault(t, "other.secret", "AN_UNRELATED_REGISTERED_VALUE_88888888")

	logger := zap.NewNop().Sugar()
	mgr := mcp.NewManager(logger)
	tool := NewMCPServerTool("bot-x", mgr, logger)

	execCtx := &llm.ToolExecContext{Context: context.Background()}
	sctx := tools.ToolSessionContext{BotID: "bot-x", ChatType: "private", UserID: "u1"}
	execCtx.Context = tools.ContextWithSessionContext(execCtx.Context, sctx)

	_, err := tool.Execute(execCtx, map[string]any{
		"action":  "add",
		"name":    "leaky",
		"type":    "http",
		"url":     "https://example.invalid/mcp",
		"headers": map[string]any{"Authorization": "BARE_PLAINTEXT_TOKEN_99999999"},
		"enabled": false,
	})
	if err == nil {
		t.Fatal("add with bare plaintext Authorization must be rejected")
	}
	if strings.Contains(err.Error(), "BARE_PLAINTEXT_TOKEN_99999999") {
		t.Fatalf("error must not leak the plaintext: %v", err)
	}
	entries, _ := mcp.LoadBotServers("bot-x")
	if len(entries) != 0 {
		t.Fatalf("rejected add must not persist anything, got %+v", entries)
	}
}
