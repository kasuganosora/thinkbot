package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// shell 工具 — 在 bot 的 sandbox 工作空间内执行 shell 命令
//
// 执行路由完全交给 sandbox 兼容层（WorkspaceExecutor.Exec）：
//   - 有 Docker 环境：命令在该 bot 的隔离容器内执行，宿主机不受影响；
//   - 无 Docker 环境：命令在宿主机本地进程执行（工作目录为 bot 工作空间）。
// 工具本身完全不感知底层后端，与 list_files、终端面板走同一条兼容层。
// ============================================================================

// WsExecRequest 是 shell/list_files 工具下达给兼容层的执行请求。
type WsExecRequest struct {
	Command string
	WorkDir string
	Timeout time.Duration
}

// WsExecResult 是兼容层返回的执行结果。
type WsExecResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
}

// WsFileEntry 是目录条目（供 list_files 使用）。
type WsFileEntry struct {
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// WorkspaceExecutor 是 shell / list_files 工具依赖的最小兼容层接口。
// sandbox.Workspace 通过装配层的薄适配器满足本接口，tools 包不直接依赖 sandbox，
// 保持解耦。
type WorkspaceExecutor interface {
	// WorkDir 返回工作空间的工作目录（用于提示 LLM）。
	WorkDir() string
	// Exec 在工作空间中执行一条 shell 命令。
	Exec(ctx context.Context, req WsExecRequest) (*WsExecResult, error)
	// ListDir 列出工作空间中指定目录的内容。
	ListDir(ctx context.Context, path string) ([]WsFileEntry, error)
}

// shellToolProvider 按 sctx.BotID 动态解析该 bot 的工作空间执行器，
// 从而使 shell 工具在每次调用时都路由到正确 bot 的沙箱。
type shellToolProvider struct {
	resolve func(botID string) (WorkspaceExecutor, error)
}

func (p *shellToolProvider) Tools(ctx context.Context, sctx *agenttools.ToolSessionContext) ([]llm.Tool, error) {
	if p.resolve == nil || sctx == nil || sctx.BotID == "" {
		return nil, nil
	}
	botID := sctx.BotID
	resolve := p.resolve
	return []llm.Tool{buildShellTool(botID, resolve)}, nil
}

func buildShellTool(botID string, resolve func(string) (WorkspaceExecutor, error)) llm.Tool {
	return llm.Tool{
		Name: "shell",
		Description: "在当前 bot 的沙箱工作空间内执行 shell 命令（sh -c）。" +
			"命令在隔离环境中运行（有 Docker 时为容器内，否则为宿主机工作目录），" +
			"可用于运行脚本、python、git、node 等。返回 stdout、stderr 和退出码。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "要执行的完整 shell 命令，例如 'ls -la' 或 'python3 main.py'。",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "命令的工作目录（相对于工作空间根）。留空使用工作空间根目录。",
				},
				"timeout_seconds": map[string]any{
					"type":        "number",
					"description": "执行超时秒数。留空使用沙箱默认超时。",
				},
			},
			"required": []string{"command"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			command, _ := m["command"].(string)
			if strings.TrimSpace(command) == "" {
				return nil, fmt.Errorf("command is required")
			}
			workdir, _ := m["workdir"].(string)

			ws, err := resolve(botID)
			if err != nil {
				return nil, fmt.Errorf("resolve workspace: %w", err)
			}

			req := WsExecRequest{Command: command, WorkDir: workdir}
			if v, ok := m["timeout_seconds"].(float64); ok && v > 0 {
				req.Timeout = time.Duration(v) * time.Second
			}

			res, err := ws.Exec(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("exec failed: %w", err)
			}
			return map[string]any{
				"exitCode":  res.ExitCode,
				"stdout":    res.Stdout,
				"stderr":    res.Stderr,
				"truncated": res.Truncated,
				"workdir":   ws.WorkDir(),
			}, nil
		}),
	}
}

// ============================================================================
// list_files（sandbox 版）— 通过兼容层列出 bot 工作空间目录
//
// 有 Docker 时读容器内 /workspace，无则读宿主机 bot 工作目录，均经 sandbox 路由，
// 不再直读任意宿主机路径。
// ============================================================================

// listFilesToolProvider 按 sctx.BotID 动态解析工作空间执行器，提供走沙箱的 list_files。
type listFilesToolProvider struct {
	resolve func(botID string) (WorkspaceExecutor, error)
}

func (p *listFilesToolProvider) Tools(ctx context.Context, sctx *agenttools.ToolSessionContext) ([]llm.Tool, error) {
	if p.resolve == nil || sctx == nil || sctx.BotID == "" {
		return nil, nil
	}
	botID := sctx.BotID
	resolve := p.resolve
	return []llm.Tool{buildSandboxListFilesTool(botID, resolve)}, nil
}

func buildSandboxListFilesTool(botID string, resolve func(string) (WorkspaceExecutor, error)) llm.Tool {
	return llm.Tool{
		Name: "list_files",
		Description: "列出当前 bot 沙箱工作空间中指定目录下的文件和子目录。" +
			"返回名称、类型、大小、修改时间。这是经沙箱路由的只读操作（有 Docker 时读容器内），" +
			"不会访问宿主机上工作空间以外的路径。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "目录路径（相对于工作空间根）。留空使用工作空间根目录。",
				},
			},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			path := "."
			if m, ok := input.(map[string]any); ok {
				if p, ok := m["path"].(string); ok && strings.TrimSpace(p) != "" {
					path = p
				}
			}

			ws, err := resolve(botID)
			if err != nil {
				return nil, fmt.Errorf("resolve workspace: %w", err)
			}
			entries, err := ws.ListDir(ctx, path)
			if err != nil {
				return nil, fmt.Errorf("list dir failed: %w", err)
			}

			items := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				item := map[string]any{
					"name":  e.Name,
					"isDir": e.IsDir,
					"size":  e.Size,
				}
				if !e.ModTime.IsZero() {
					item["modTime"] = e.ModTime.UTC().Format(time.RFC3339)
				}
				items = append(items, item)
			}
			return map[string]any{
				"path":    path,
				"entries": items,
				"count":   len(items),
			}, nil
		}),
	}
}
