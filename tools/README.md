# tools — 通用工具集

提供开箱即用的通用 LLM 工具。这些工具不依赖特定 bot 的工作空间，适用于所有场景。

> 注意：shell 执行与文件读写类工具（`sandbox_exec` / `sandbox_read_file` 等）
> 由 `sandbox` 包通过 `BotWorkspaceManager` 注册，不在本包内。

## 注册的工具

| 工具 | 文件 | 说明 |
|------|------|------|
| **now** | `now.go` | 获取当前日期/时间，返回 datetime、date、time、weekday、timezone、utc、unix、iso8601、isWeekend。通过 `ToolProvider` 动态提供，按 bot 解析时区 |
| **web_fetch** | `http.go` | 获取 URL 内容 / 发送 HTTP 请求（`url` 必填，可选 `method`/`headers`/`body`），返回状态码、Content-Type、截断后的正文 |
| **web_search** | `web_search.go` | 搜索互联网，返回标题/URL/摘要。后端支持 DuckDuckGo（默认）与 SearXNG |
| **calculate** | `calc.go` | 安全数学表达式求值（递归下降解析器）。支持 `+ - * / % ^`、括号，函数 `sqrt/abs/round/floor/ceil/sin/cos/tan/ln/log/log10/exp/min/max`，常量 `pi`、`e` |
| **random** | `calc.go` | 生成随机整数/浮点数（`min`/`max`/`type`/`count`，上限 1000），或从 `choices` 列表中随机选择 |
| **uuid** | `calc.go` | 生成 UUID v4（基于 `crypto/rand`，`count` 上限 100） |
| **datetime_calc** | `datetime.go` | 日期时间计算，`operation` 取 `add`/`diff`/`weekday`/`format` |
| **text_hash** | `text_tools.go` | 计算文本哈希（`md5` / `sha256`，默认 sha256） |
| **text_encode** | `text_tools.go` | Base64 `encode` / `decode` |
| **text_diff** | `text_tools.go` | 基于 LCS 的行级差异比较（每侧最多 5000 行） |
| **text_stats** | `text_tools.go` | 统计行数、词数、字符数、段落数、估算 token |

除 `now`、`web_fetch`、`web_search`、`calculate` 外，其余工具均标记 `DeferredLoad`，
初始只向模型暴露名称与描述，减少上下文开销。

`prompt.go` 不定义工具，而是提供提示词段落 `common_tools`（order 320），
经由一个永不出现在工具列表中的占位工具 `__common_tools_meta` 挂载。

## 注册

```go
import (
    agenttools "github.com/kasuganosora/thinkbot/agent/tools"
    "github.com/kasuganosora/thinkbot/tools"
)

err := tools.RegisterTools(toolMgr, tools.Config{
    TimezoneResolver: func(botID string) string {
        return cfgBuilder.GetBotTimezone(botID) // now 工具的 per-bot 时区
    },
})
```

### Config

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `TimezoneResolver func(botID string) string` | nil | 按 botID 返回 IANA 时区；为 nil 时 `now` 使用 UTC |
| `HTTPTimeout time.Duration` | 30s | `web_fetch` 请求超时 |
| `MaxFetchSize int` | 1MB | `web_fetch` 返回的最大 body 字节数 |
| `UserAgent string` | `ThinkbotBot/1.0` | HTTP User-Agent |
| `SearchConfig *SearchConfig` | nil | 为 nil 时使用 `DefaultSearchConfig()`（DuckDuckGo） |

### 搜索配置

```go
tools.RegisterSearchTools(toolMgr, tools.SearchConfig{
    Engine:     "searxng",              // "duckduckgo"（默认）或 "searxng"
    SearXNGURL: "https://searx.example",
    Timeout:    15 * time.Second,       // 默认 15s
    MaxResults: 5,                      // 默认 5
    UserAgent:  "ThinkbotBot/1.0",
})
```

`DefaultSearchConfig()` 返回 DuckDuckGo + 15s + 5 条结果的默认配置。
单条结果类型为 `SearchResult{Title, URL, Snippet}`。

## 其他导出

| 符号 | 说明 |
|------|------|
| `RegisterTools(mgr, cfg) error` | 注册全部通用工具 |
| `RegisterSearchTools(mgr, cfg) error` | 仅注册 `web_search` |
| `Config` / `SearchConfig` / `DefaultSearchConfig()` / `SearchResult` | 配置与结果类型 |
| `ParseTimezone(tz string) *time.Location` | 解析 IANA 时区，失败回退 `time.UTC` |
