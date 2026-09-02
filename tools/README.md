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
| **user_choice** | `user_choice.go` | 阻塞式向用户提问并等待选择（`question` + 1~8 个 `options` + `mode`），web 卡片 / Telegram inline keyboard / Misskey 原生 poll 三平台原生渲染，返回 `answered` / `timeout` 两态 JSON。仅 `private` scope 可用，详见下文专节 |

除 `now`、`web_fetch`、`web_search`、`calculate`、`user_choice` 外，其余工具均标记 `DeferredLoad`，
初始只向模型暴露名称与描述，减少上下文开销。

`prompt.go` 不定义工具，而是提供提示词段落 `common_tools`（order 320），
经由一个永不出现在工具列表中的占位工具 `__common_tools_meta` 挂载。

## user_choice

阻塞式向用户提问并等待选择。工具调用会**阻塞**直到用户作答或超时，
问题经 `internal/interaction` 注册表注册后随 progress 事件下发渲染，
应答回填同一注册表唤醒等待方。

> 仅注册到 `private` scope（有明确对话对象的场景）；
> 群聊 / 子代理提问无人应答，不开放。

### 参数

| 参数 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `question` | 是 | — | 问题正文，展示给用户 |
| `options` | 是 | — | 1~8 个选项，元素为 `{label, description?}`（`label` 必填，`description` 可选）。无需提供“自由输入”占位项，输入框由各平台自动渲染 |
| `mode` | 是 | — | `single`（恰好选一个）/ `multi`（任选多个） |
| `input_hint` | 否 | 空 | 自由输入框的引导文案，如“或直接输入你的答案” |
| `timeout_secs` | 否 | 600 | 等待用户应答的超时秒数；0 或负值按 600 处理 |

### 返回（两态）

用户作答（`status: "answered"`）：

```json
{
  "status": "answered",
  "selected": [1],
  "selected_labels": ["B 选项文案"],
  "custom_input": "",
  "via": "web"
}
```

| 字段 | 说明 |
|------|------|
| `selected` | 选中的选项下标数组（0 起）；仅自由输入时为 `[]` |
| `selected_labels` | 与 `selected` 对应的选项文案，省一次下标→文案换算 |
| `custom_input` | 用户自由输入的原文，未输入为空串 |
| `via` | 应答来源平台：`web` / `telegram` / `misskey` |

等待超时（`status: "timeout"`）：

```json
{
  "status": "timeout",
  "message": "等待用户应答超时。请基于已有信息给出合理的默认处理，或稍后再问；不要反复重试本工具。"
}
```

**超时后 LLM 应自行降级继续**：基于已有信息给出合理的默认处理，
或稍后再问；不要循环重试本工具、也不要卡住等待。

### 三平台渲染与交互

| 平台 | 渲染 | 用户交互 | 回填 |
|------|------|----------|------|
| web | `tool_progress` SSE 事件携带 `UserChoiceEventPayload`，前端 `ChoiceCard.vue` 渲染内联卡片 | 点选选项；multi 模式可多选并确认；底部输入框自由输入 | `POST /api/user-choice/{questionId}/answer`，body `{selectedIds, freeText}` |
| telegram | channel 发 `InlineKeyboardMarkup`（`RegisterPollCreator`） | 单选：点按钮即回填；多选：toggle 后点「确认」 | `callback_query` → `interaction.ResolveFrom` |
| misskey | channel 发原生 poll（至少 2 个选项） | 在 Misskey UI 点选；多选累计后 debounce ~3s 回填 | WS `pollVoted` → `interaction.ResolveFrom` |

web 路径发 progress 事件供前端渲染。telegram / misskey 走 `PollCreator` 原生控件，不经 SSE 卡片。

### 调用示例

```json
{
  "question": "这份周报要以什么口径发送？",
  "options": [
    { "label": "正式版", "description": "完整数据 + 结论，发给全组" },
    { "label": "精简版", "description": "只保留关键指标" },
    { "label": "暂不发送", "description": "我再看一遍" }
  ],
  "mode": "single",
  "input_hint": "或直接输入你的要求",
  "timeout_secs": 300
}
```

用户点选“精简版”后返回：

```json
{
  "status": "answered",
  "selected": [1],
  "selected_labels": ["精简版"],
  "custom_input": "",
  "via": "web"
}
```

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
