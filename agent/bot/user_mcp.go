package bot

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/mcp"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// setupUserMCP 为本 bot 接入 Admin UI / 工具配置的 per-bot MCP 服务器
// （data/mcp/{botId}/servers.json）。
//
// 即便当前没有任何服务器，也会创建一个空 Manager 并 RegisterTools，以便后续
// API / mcp_server 工具热挂载时无需重启。失败不致命（记 WARN）。
func setupUserMCP(b *Bot, params BotParams) {
	if params.ToolManager == nil {
		return
	}
	logger := b.logger.With("component", "user_mcp")
	mgr := mcp.NewManager(logger)

	entries, err := mcp.LoadBotServers(params.ID)
	if err != nil {
		logger.Warnw("load bot mcp servers failed", "err", err)
	}

	ctx := context.Background()
	connected := 0
	for _, e := range entries {
		cfg := e.ToServerConfig()
		if cfg.Name == "" {
			continue
		}
		if err := mgr.UpsertServer(ctx, cfg); err != nil {
			logger.Warnw("mcp server connect failed at startup",
				"server", cfg.Name, "err", err)
			continue
		}
		if cfg.Enabled && mgr.IsServerConnected(cfg.Name) {
			connected++
		}
	}

	if err := mcp.RegisterTools(params.ToolManager, mgr); err != nil {
		logger.Warnw("register mcp tools failed", "err", err)
		_ = mgr.Close()
		return
	}

	// 注册 bot 可调用的 MCP 管理工具（敏感：需 toolperm 显式 allow）。
	if regErr := params.ToolManager.Register(NewMCPServerTool(params.ID, mgr, logger)); regErr != nil {
		logger.Warnw("register mcp_server tool failed", "err", regErr)
	}

	b.userMCP = mgr
	logger.Infow("user mcp wired",
		"bot_id", params.ID,
		"configured", len(entries),
		"connected", connected)
}

// UserMCP 返回本 bot 的用户 MCP Manager（Admin UI / mcp_server 工具共用）。
// 未接线时返回 nil。
func (b *Bot) UserMCP() *mcp.Manager {
	if b == nil {
		return nil
	}
	return b.userMCP
}

// NewMCPServerTool 返回让 bot 自行增删/启用 MCP 服务器的工具。
//
// 权限：名称前缀 mcp_ → toolperm.RiskSensitive，默认需管理员在「工具权限」里显式 allow。
// 额外硬约束：拒绝子代理与系统会话（cron/心跳/梦境），避免无人值守路径拉起任意进程。
func NewMCPServerTool(botID string, mgr *mcp.Manager, logger *zap.SugaredLogger) tools.ToolDef {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return tools.ToolDef{
		Tool: llm.Tool{
			Name: "mcp_server",
			Description: "Manage this bot's MCP (Model Context Protocol) servers. " +
				"Use action 'list' to see configured servers and connection status. " +
				"Use action 'add' to register a new MCP server (stdio command or http/sse URL), " +
				"persist it, and connect immediately so its tools appear without restart. " +
				"Use action 'remove' to disconnect and delete a server by name. " +
				"Use action 'enable' / 'disable' to toggle an existing server. " +
				"Tool names from an MCP server appear as `<server>__<tool>`. " +
				"Reserved name 'browser' is not allowed. " +
				"This is a sensitive capability — only use when the user explicitly asks to add or change MCP servers.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"description": "Operation: list | add | remove | enable | disable",
						"enum":        []string{"list", "add", "remove", "enable", "disable"},
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Server name (unique tool-name prefix). Required for add/remove/enable/disable.",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "Transport type for add: stdio | http | sse (default stdio if command set, else http if url set)",
						"enum":        []string{"stdio", "http", "sse"},
					},
					"command": map[string]any{
						"type":        "string",
						"description": "stdio: executable command (e.g. npx)",
					},
					"args": map[string]any{
						"type":        "array",
						"description": "stdio: command arguments",
						"items":       map[string]any{"type": "string"},
					},
					"env": map[string]any{
						"type":                 "object",
						"description":          "stdio: environment variables as key→value string map",
						"additionalProperties": map[string]any{"type": "string"},
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "stdio: working directory",
					},
					"url": map[string]any{
						"type":        "string",
						"description": "http/sse: server URL",
					},
					"headers": map[string]any{
						"type":                 "object",
						"description":          "http/sse: request headers as key→value string map",
						"additionalProperties": map[string]any{"type": "string"},
					},
					"enabled": map[string]any{
						"type":        "boolean",
						"description": "For add: whether to connect immediately (default true)",
					},
				},
				"required": []string{"action"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				return executeMCPServerTool(ctx, botID, mgr, logger, input)
			}),
		},
		Category: "mcp",
		// 主会话私聊/群聊可用，子代理不暴露。
		//
		// 2026-10-26 放开群聊：管理员在群里让 bot 挂 MCP 是最自然的用法，
		// 群聊场景此前被 private 一刀切掉，导致「在群里让 bot 加 MCP」永远
		// 搜不到这个工具（只能改回单聊或走 Web）。风险由 toolperm 规则收敛：
		// mcp_server 命中 mcp_ 敏感前缀，企微已进入白名单模式，必须显式 allow
		// 才放行；子代理仍被 executeMCPServerTool 硬拒。
		Scopes: []string{"private", "group"},
	}
}

func executeMCPServerTool(ctx *llm.ToolExecContext, botID string, mgr *mcp.Manager, logger *zap.SugaredLogger, input any) (any, error) {
	if mgr == nil {
		return nil, fmt.Errorf("mcp_server: mcp manager not available")
	}
	args, ok := input.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mcp_server: invalid input type")
	}
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		return nil, fmt.Errorf("mcp_server: action is required")
	}

	// 硬约束：系统会话 / 子代理不得改 MCP（即便权限表误放行）。
	if ctx != nil {
		if sctx, ok := tools.SessionContextFromContext(ctx); ok {
			if sctx.IsSystem {
				return nil, fmt.Errorf("mcp_server: refused in system sessions")
			}
			if sctx.IsSubagent {
				return nil, fmt.Errorf("mcp_server: refused for subagents")
			}
		}
	}

	switch action {
	case "list":
		return listMCPServersForTool(botID, mgr)

	case "add":
		return addMCPServerForTool(ctx, botID, mgr, logger, args)

	case "remove":
		name, _ := args["name"].(string)
		name = strings.TrimSpace(name)
		if err := mcp.ValidateServerName(name); err != nil {
			return nil, fmt.Errorf("mcp_server: %w", err)
		}
		entries, err := mcp.LoadBotServers(botID)
		if err != nil {
			return nil, fmt.Errorf("mcp_server: load: %w", err)
		}
		found := false
		kept := entries[:0]
		for _, e := range entries {
			if e.Name == name {
				found = true
				continue
			}
			kept = append(kept, e)
		}
		if !found {
			return nil, fmt.Errorf("mcp_server: server %q not found", name)
		}
		if err := mcp.SaveBotServers(botID, kept); err != nil {
			return nil, fmt.Errorf("mcp_server: save: %w", err)
		}
		mgr.RemoveServer(name)
		logger.Infow("mcp_server tool removed server", "bot_id", botID, "server", name)
		return map[string]any{"action": "remove", "name": name, "ok": true}, nil

	case "enable", "disable":
		name, _ := args["name"].(string)
		name = strings.TrimSpace(name)
		if err := mcp.ValidateServerName(name); err != nil {
			return nil, fmt.Errorf("mcp_server: %w", err)
		}
		entries, err := mcp.LoadBotServers(botID)
		if err != nil {
			return nil, fmt.Errorf("mcp_server: load: %w", err)
		}
		idx := -1
		for i, e := range entries {
			if e.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("mcp_server: server %q not found", name)
		}
		wantEnable := action == "enable"
		entries[idx].Enabled = wantEnable
		entries[idx].UpdatedAt = mcp.NowRFC3339()
		cfg := entries[idx].ToServerConfig()
		cfg.Enabled = wantEnable && entries[idx].ReadyToConnect()
		var connectErr error
		if goCtx := toolExecContext(ctx); goCtx != nil {
			connectErr = mgr.UpsertServer(goCtx, cfg)
		} else {
			connectErr = mgr.UpsertServer(context.Background(), cfg)
		}
		entries[idx].Status = mcp.DeriveStatus(wantEnable, entries[idx].ReadyToConnect(), mgr.IsServerConnected(name), connectErr)
		if err := mcp.SaveBotServers(botID, entries); err != nil {
			return nil, fmt.Errorf("mcp_server: save: %w", err)
		}
		out := map[string]any{
			"action":    action,
			"name":      name,
			"enabled":   wantEnable,
			"status":    entries[idx].Status,
			"connected": mgr.IsServerConnected(name),
		}
		if connectErr != nil {
			out["error"] = connectErr.Error()
		}
		return out, nil

	default:
		return nil, fmt.Errorf("mcp_server: unknown action %q (list|add|remove|enable|disable)", action)
	}
}

func listMCPServersForTool(botID string, mgr *mcp.Manager) (any, error) {
	entries, err := mcp.LoadBotServers(botID)
	if err != nil {
		return nil, fmt.Errorf("mcp_server: load: %w", err)
	}
	live := map[string]mcp.ServerStatus{}
	for _, s := range mgr.ListServers() {
		live[s.Name] = s
	}
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		item := map[string]any{
			"id":      e.ID,
			"name":    e.Name,
			"type":    e.Type,
			"enabled": e.Enabled,
			"status":  e.Status,
		}
		if st, ok := live[e.Name]; ok {
			item["connected"] = st.Connected
			item["runtime_enabled"] = st.Enabled
		} else {
			item["connected"] = false
		}
		items = append(items, item)
	}
	return map[string]any{"action": "list", "servers": items, "count": len(items)}, nil
}

func addMCPServerForTool(ctx *llm.ToolExecContext, botID string, mgr *mcp.Manager, logger *zap.SugaredLogger, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if err := mcp.ValidateServerName(name); err != nil {
		return nil, fmt.Errorf("mcp_server: %w", err)
	}

	tp, _ := args["type"].(string)
	command, _ := args["command"].(string)
	url, _ := args["url"].(string)
	cwd, _ := args["cwd"].(string)
	if tp == "" {
		if strings.TrimSpace(url) != "" {
			tp = "http"
		} else {
			tp = "stdio"
		}
	}
	enabled := true
	if v, ok := args["enabled"].(bool); ok {
		enabled = v
	}

	entry := mcp.BotServerEntry{
		ID:        idgen.New("mcp"),
		Name:      name,
		Type:      tp,
		Command:   command,
		Args:      stringSliceArg(args["args"]),
		Env:       stringMapArg(args["env"]),
		Cwd:       cwd,
		URL:       url,
		Headers:   stringMapArg(args["headers"]),
		Enabled:   enabled,
		CreatedAt: mcp.NowRFC3339(),
		UpdatedAt: mcp.NowRFC3339(),
	}
	if entry.Args == nil {
		entry.Args = []string{}
	}
	if entry.Env == nil {
		entry.Env = map[string]string{}
	}
	if entry.Headers == nil {
		entry.Headers = map[string]string{}
	}

	entries, err := mcp.LoadBotServers(botID)
	if err != nil {
		return nil, fmt.Errorf("mcp_server: load: %w", err)
	}
	for _, e := range entries {
		if e.Name == name {
			return nil, fmt.Errorf("mcp_server: server %q already exists", name)
		}
	}

	cfg := entry.ToServerConfig()
	var connectErr error
	goCtx := toolExecContext(ctx)
	if goCtx == nil {
		goCtx = context.Background()
	}
	connectErr = mgr.UpsertServer(goCtx, cfg)
	entry.Status = mcp.DeriveStatus(entry.Enabled, entry.ReadyToConnect(), mgr.IsServerConnected(name), connectErr)

	entries = append(entries, entry)
	if err := mcp.SaveBotServers(botID, entries); err != nil {
		// 回滚运行时，避免文件与 Manager 不一致
		mgr.RemoveServer(name)
		return nil, fmt.Errorf("mcp_server: save: %w", err)
	}

	logger.Infow("mcp_server tool added server",
		"bot_id", botID, "server", name, "status", entry.Status, "connected", mgr.IsServerConnected(name))

	out := map[string]any{
		"action":    "add",
		"id":        entry.ID,
		"name":      entry.Name,
		"type":      entry.Type,
		"enabled":   entry.Enabled,
		"status":    entry.Status,
		"connected": mgr.IsServerConnected(name),
		"hint":      "New tools use names like " + name + "__<tool>. They appear on the next turn after cache refresh.",
	}
	if connectErr != nil {
		out["error"] = connectErr.Error()
	}
	return out, nil
}

func stringSliceArg(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringMapArg(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		if sm, ok := v.(map[string]string); ok {
			return sm
		}
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

// toolExecContext 从 llm.ToolExecContext 取出可取消的 context.Context。
// ToolExecContext 嵌入 context.Context；若类型变化则退回 Background。
func toolExecContext(ctx *llm.ToolExecContext) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
