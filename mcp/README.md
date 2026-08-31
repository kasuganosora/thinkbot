# MCP (Model Context Protocol) 客户端

连接外部 MCP 服务器，将其工具自动注入到 Agent 的工具列表中。

协议版本：`2025-03-26`（JSON-RPC 2.0）。

## 架构

```
mcp/
├── protocol.go    JSON-RPC 2.0 + MCP 协议类型（内部类型）
├── transport.go   传输层（stdio 子进程 + Streamable HTTP / SSE）
├── client.go      单服务器客户端（Initialize/ListTools/CallTool/Close/IsHealthy）
├── manager.go     多服务器生命周期管理 + 断线自动重连
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

说明：

- 键名 `mcp.<name>` 中的 `<name>` 即服务器名；含额外 `.` 的键（如 `mcp.a.b`）会被 `LoadServers` 跳过。
- stdio 额外支持 `env`（`["KEY=VALUE"]`）；未指定 `transport` 时默认按 `stdio` 处理。
- HTTP 传输超时 120s，最大响应体 10MB，自动携带服务器返回的 `Mcp-Session-Id`，并可解析 `text/event-stream` 响应。

## 使用方式

### 自动集成（推荐）

```go
import "github.com/kasuganosora/thinkbot/mcp"

// 在 Bot 初始化时从配置加载（mcp.enabled=false 或无服务器时返回 nil Manager）
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

## 主要 API

**Manager**

| 方法 | 说明 |
| --- | --- |
| `NewManager(logger)` | 创建管理器 |
| `AddServer(ServerConfig)` / `RemoveServer(name)` | 注册（不立即连接）/ 移除并关闭 |
| `Connect(ctx)` | 连接所有 `Enabled` 的服务器，部分失败会汇总为 error |
| `EnableServer(ctx, name)` / `DisableServer(name)` | 运行时开关单个服务器，幂等，触发缓存失效回调 |
| `IsServerEnabled(name)` / `IsServerConnected(name)` | 状态查询 |
| `ListServers() []ServerStatus` | 返回 `{Name, Transport, Enabled, Connected}` |
| `CallTool(ctx, server, tool, args)` | 调用工具，连接失效时自动重连并重试一次 |
| `ListAllTools(ctx)` | 按服务器分组列出工具（单个服务器失败时跳过） |
| `GetClient(name)` / `ConnectedServers()` / `ServerCount()` | 客户端与统计信息 |
| `SetOnServerChange(fn)` | 设置服务器状态变更回调 |
| `Close()` | 关闭全部连接 |

**Client**：`Initialize(ctx)`、`ListTools(ctx)`（自动翻页 cursor）、`CallTool(ctx, name, args)`、`IsHealthy()`、`Name()`、`Close()`。Client 由 Manager 创建，一般不需手动构造。

**其他**：`LoadServers(store) []ServerConfig`、`SetupFromConfig(...)`、`NewProvider(mgr) *Provider`、`RegisterTools(toolMgr, mgr)`。

## 工具命名与加载

MCP 工具名格式：`<server_name>__<tool_name>`

例如服务器 `filesystem` 的 `read_file` 工具 → `filesystem__read_file`，避免不同服务器之间的工具名冲突。

MCP 工具统一标记 `DeferredLoad = true`：初始只向模型暴露名称与描述，完整 input schema 在需要时（如 `tool_search` 或模型直接引用）再加载，以节省 token 并降低工具选择错误率。

`RegisterTools` 还会注册一个名为 `mcp_rules` 的提示词段落（Order 305），告知模型 MCP 工具的存在与结果为纯文本。

## 缓存与断线重连

- `Provider` 缓存工具列表，避免每次 `Tools()` 都请求 MCP 服务器；`InvalidateCache()` 可手动失效。`RegisterTools` 会把它挂到 `Manager.SetOnServerChange`，服务器启用/禁用/重连后自动刷新。
- `Client.IsHealthy()` 依托传输层探活：stdio 通过向子进程发送信号 0 判断存活；HTTP 恒为健康，由请求失败驱动重连。
- `Manager.CallTool` 在连接失效时会按服务器粒度加锁重建连接并重试一次，使 MCP 服务器崩溃或重启后工具可自愈。

## 测试

集成测试需要真实凭据，参见 `.env.test.example`：

```bash
cp mcp/.env.test.example mcp/.env.test   # 填入凭据
go test -v -run TestIntegration ./mcp/ -timeout 180s
```

未配置凭据时相关用例会自动 Skip。
