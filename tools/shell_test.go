package tools

import (
	"context"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/llm"
)

// fakeWorkspace 实现 WorkspaceExecutor，记录调用并返回预设结果。
type fakeWorkspace struct {
	lastCmd     string
	lastWorkDir string
	entries     []WsFileEntry
}

func (f *fakeWorkspace) WorkDir() string { return "/workspace" }

func (f *fakeWorkspace) Exec(ctx context.Context, req WsExecRequest) (*WsExecResult, error) {
	f.lastCmd = req.Command
	f.lastWorkDir = req.WorkDir
	return &WsExecResult{ExitCode: 0, Stdout: "hello\n", Stderr: ""}, nil
}

func (f *fakeWorkspace) ListDir(ctx context.Context, path string) ([]WsFileEntry, error) {
	return f.entries, nil
}

func TestShellProvider_RoutesToWorkspace(t *testing.T) {
	fw := &fakeWorkspace{}
	p := &shellToolProvider{resolve: func(botID string) (WorkspaceExecutor, error) {
		if botID != "bot-1" {
			t.Fatalf("unexpected botID %q", botID)
		}
		return fw, nil
	}}

	toolsList, err := p.Tools(context.Background(), &agenttools.ToolSessionContext{BotID: "bot-1"})
	if err != nil {
		t.Fatalf("Tools err: %v", err)
	}
	if len(toolsList) != 1 || toolsList[0].Name != "shell" {
		t.Fatalf("expected one shell tool, got %+v", toolsList)
	}

	out, err := toolsList[0].Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"command": "echo hello", "workdir": "sub"},
	)
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if fw.lastCmd != "echo hello" || fw.lastWorkDir != "sub" {
		t.Fatalf("command not routed correctly: cmd=%q wd=%q", fw.lastCmd, fw.lastWorkDir)
	}
	res := out.(map[string]any)
	if res["stdout"] != "hello\n" || res["exitCode"] != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestShellProvider_NoBotID(t *testing.T) {
	p := &shellToolProvider{resolve: func(string) (WorkspaceExecutor, error) { return &fakeWorkspace{}, nil }}
	toolsList, err := p.Tools(context.Background(), &agenttools.ToolSessionContext{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(toolsList) != 0 {
		t.Fatalf("expected no tools when BotID empty, got %d", len(toolsList))
	}
}

func TestListFilesProvider_RoutesToWorkspace(t *testing.T) {
	fw := &fakeWorkspace{entries: []WsFileEntry{
		{Name: "a.txt", IsDir: false, Size: 3},
		{Name: "dir", IsDir: true},
	}}
	p := &listFilesToolProvider{resolve: func(string) (WorkspaceExecutor, error) { return fw, nil }}

	toolsList, err := p.Tools(context.Background(), &agenttools.ToolSessionContext{BotID: "bot-1"})
	if err != nil {
		t.Fatalf("Tools err: %v", err)
	}
	if len(toolsList) != 1 || toolsList[0].Name != "list_files" {
		t.Fatalf("expected one list_files tool, got %+v", toolsList)
	}

	out, err := toolsList[0].Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"path": "."},
	)
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	res := out.(map[string]any)
	if res["count"].(int) != 2 {
		t.Fatalf("expected 2 entries, got %+v", res)
	}
}

func TestRegisterTools_WithResolver_RegistersShell(t *testing.T) {
	promptReg := prompt.NewRegistry()
	mgr := agenttools.NewToolManager(promptReg, nil, nil)
	err := RegisterTools(mgr, Config{
		TimezoneResolver:  func(string) string { return "UTC" },
		WorkspaceResolver: func(string) (WorkspaceExecutor, error) { return &fakeWorkspace{}, nil },
	})
	if err != nil {
		t.Fatalf("RegisterTools err: %v", err)
	}
	resolved, err := mgr.ResolveTools(context.Background(), &agenttools.ToolSessionContext{BotID: "bot-1"})
	if err != nil {
		t.Fatalf("ResolveTools err: %v", err)
	}
	var hasShell, hasListFiles bool
	for _, tl := range resolved {
		if tl.Name == "shell" {
			hasShell = true
		}
		if tl.Name == "list_files" {
			hasListFiles = true
		}
	}
	if !hasShell {
		t.Fatalf("shell tool not registered when WorkspaceResolver provided")
	}
	if !hasListFiles {
		t.Fatalf("list_files tool not registered")
	}
}
