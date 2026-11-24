package bot

import (
	"fmt"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/mcp"
)

// 启动期不应对禁用/草稿条目发起连接，且连接失败必须回写持久化状态。
// 修复前：每次启动都会对所有 enabled 条目重试一遍并刷 WARN，失败后列表
// 还停留在上一次的 running，模型会误以为这个能力可用。
func TestSetupUserMCP_SkipsDisabledAndPersistsConnectFailure(t *testing.T) {
	chdirTemp(t)
	now := mcp.NowRFC3339()
	botID := "bot-startup-status"
	unreachable := "http://127.0.0.1:1"
	entries := []mcp.BotServerEntry{
		{ID: "1", Name: "broken", Type: "http", URL: unreachable, Env: map[string]string{}, Headers: map[string]string{}, Enabled: true, Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "2", Name: "turned-off", Type: "http", URL: unreachable, Env: map[string]string{}, Headers: map[string]string{}, Enabled: false, Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "3", Name: "draft", Type: "stdio", Command: "", Env: map[string]string{}, Headers: map[string]string{}, Enabled: true, Status: "running", CreatedAt: now, UpdatedAt: now},
	}
	if err := mcp.SaveBotServers(botID, entries); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	b := &Bot{ID: botID, logger: logger}
	setupUserMCP(b, BotParams{
		ID:          botID,
		ToolManager: tools.NewToolManager(prompt.NewRegistry(), nil, logger),
	})
	t.Cleanup(func() {
		if b.userMCP != nil {
			_ = b.userMCP.Close()
		}
	})

	got, err := mcp.LoadBotServers(botID)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, e := range got {
		status[e.Name] = e.Status
	}
	if status["broken"] != "error" {
		t.Errorf("failed server status = %q, want error", status["broken"])
	}
	if status["turned-off"] != "disabled" {
		t.Errorf("disabled server status = %q, want disabled", status["turned-off"])
	}
	if status["draft"] != "draft" {
		t.Errorf("not-ready server status = %q, want draft", status["draft"])
	}

	// 只有真正尝试过连接的条目才允许出现 connect failed WARN。
	failures := logs.FilterMessage("mcp server connect failed at startup").All()
	if len(failures) != 1 {
		t.Fatalf("expected exactly 1 connect failure, got %d", len(failures))
	}
	if name := failures[0].ContextMap()["server"]; name != "broken" {
		t.Fatalf("connect failure for %v, want broken only", name)
	}
	if got := len(logs.FilterMessage("mcp server skipped at startup").All()); got != 2 {
		t.Fatalf("expected 2 skipped entries, got %d", got)
	}
	if b.UserMCP() == nil {
		t.Fatal("manager must still be wired when every server is unavailable")
	}
	if b.UserMCP().IsServerConnected("turned-off") || b.UserMCP().IsServerConnected("draft") {
		t.Fatal("disabled/draft servers must not be connected")
	}
}

// 已处于正确状态的条目不应被反复重写（避免每次启动都产生一次无谓落盘）。
func TestSetupUserMCP_LeavesCorrectStatusesUntouched(t *testing.T) {
	chdirTemp(t)
	now := mcp.NowRFC3339()
	botID := "bot-startup-nochurn"
	entries := []mcp.BotServerEntry{
		{ID: "1", Name: "off", Type: "http", URL: "http://127.0.0.1:1", Env: map[string]string{}, Headers: map[string]string{}, Enabled: false, Status: "disabled", CreatedAt: now, UpdatedAt: now},
	}
	if err := mcp.SaveBotServers(botID, entries); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	b := &Bot{ID: botID, logger: logger}
	setupUserMCP(b, BotParams{
		ID:          botID,
		ToolManager: tools.NewToolManager(prompt.NewRegistry(), nil, logger),
	})
	t.Cleanup(func() {
		if b.userMCP != nil {
			_ = b.userMCP.Close()
		}
	})

	got, err := mcp.LoadBotServers(botID)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Status != "disabled" || got[0].UpdatedAt != now {
		t.Fatalf("unchanged entry was rewritten: %+v", got[0])
	}
	wired := logs.FilterMessage("user mcp wired").All()
	if len(wired) != 1 {
		t.Fatalf("expected one wired log, got %d", len(wired))
	}
	if updates := wired[0].ContextMap()["status_updates"]; fmt.Sprintf("%v", updates) != "0" {
		t.Fatalf("expected 0 status updates, got %v (%T)", updates, updates)
	}
}
