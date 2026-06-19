# MCP (Model Context Protocol) 客户端

连接外部 MCP 服务器，将其工具自动注入到 Agent 的工具列表中。

## 架构

```
mcp/
├── protocol.go    JSON-RPC 2.0 + MCP 协议类型
├── transport.go   传输层（stdio + Streamable HTTP）
├── client.go      单服务器客户端（Initialize/ListTools/CallTool）
├── manager.go     多服务器生命周期管理
├── provider.go    ToolProvider 适配器 → 自动注入 ToolManager
└── config.go      从 config.Store 加载配置
```

依赖方向：`mcp → agent/tools`（单向，无循环依赖）。

## 配置

在 `.env` 或数据库中配置：

```bash
# 全局开关
mcp.enabled = true

# stdio 模式（启动子进程）
mcp.filesystem = {"transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","."],"enabled":true}

# HTTP 模式（连接远程服务器）
mcp.remote = {"transport":"http","url":"https://example.com/mcp","headers":{"Authorization":"Bearer xxx"},"enabled":true}

# 暂时禁用某个服务器
mcp.experiment = {"transport":"stdio","command":"node","args":["server.js"],"enabled":false}
```

## 使用方式

### 自动集成（推荐）

```go
import "github.com/kasuganosora/thinkbot/mcp"

// 在 Bot 初始化时从配置加载
mcpMgr, err := mcp.SetupFromConfig(ctx, configStore, toolMgr, logger)
if err != nil {
    logger.Warnw("mcp setup failed", "err", err)
}
defer func() { _ = mcpMgr.Close() }()

// toolMgr 已自动包含所有 MCP 工具
// 无需额外操作 — LLM 调用时自动解析
```

### 手动使用

```go
mgr := mcp.NewManager(logger)
mgr.AddServer(mcp.ServerConfig{
    Name:      "filesystem",
    Transport: "stdio",
    Command:   "npx",
    Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "."},
    Enabled:   true,
})
mgr.Connect(ctx)
defer mgr.Close()

// 注册到 ToolManager
mcp.RegisterTools(toolMgr, mgr)
```

## 工具命名

MCP 工具名格式：`<server_name>__<tool_name>`

例如服务器 `filesystem` 的 `read_file` 工具 → `filesystem__read_file`

这避免了不同 MCP 服务器之间的工具名冲突。

## SubAgent 隔离

MCP 工具在 SubAgent 场景下自动隐藏（`Provider.Tools` 检查 `IsSubagent`），防止子 Agent 调用外部工具。
