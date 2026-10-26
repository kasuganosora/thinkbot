package bot

import (
	"context"
	"os"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/mcp"
)

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
