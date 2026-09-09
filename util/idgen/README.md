# util/idgen — 唯一 ID 生成器

基于 `crypto/rand` 的安全随机 ID 生成工具。生成格式为 `{prefix}-{24 hex chars}` 的唯一标识，用于消息 ID、记忆条目 ID、笔记 ID 等场景。

## 用法

```go
import "github.com/kasuganosora/thinkbot/util/idgen"

// 带前缀（推荐，便于日志排查来源）
msgID  := idgen.New("msg")      // → "msg-a3f1b2c4d5e6f7a8b9c0d1e2"
memID  := idgen.New("mem")      // → "mem-9e8d7c6b5a4f3e2d1c0b9a8f"
noteID := idgen.New("note")     // → "note-7c6b5a4f3e2d1c0b9a8f7e6d"

// 空前缀（分隔符 "-" 仍会保留）
rawID := idgen.New("")          // → "-a3f1b2c4d5e6f7a8b9c0d1e2"
```

## 设计细节

| 特性 | 说明 |
|------|------|
| 随机源 | `crypto/rand`（密码学安全，非 `math/rand`） |
| 随机字节数 | 12 字节 = 96 位随机空间（碰撞概率 ≈ 2⁻⁹⁶） |
| 编码 | hex（24 字符，全小写） |
| 格式 | `{prefix}-{hex}`，前缀与随机段之间恒定使用 `-` 连接 |
| 失败回退 | `crypto/rand` 极端失败时回退到 `{prefix}-{unix-nano}` |

## 前缀命名规范（约定）

项目中实际在用的前缀（按使用频次大致排列）：

| 前缀 | 场景 | 主要调用方 |
|------|------|-----------|
| `mem` | 记忆条目 ID | `agent/memory`、`agent/storage` |
| `bc` | 浏览器 Cookie 条目 | `api/handler_bot_browser`、`api/botservice` |
| `msg` | 入站消息 ID | `agent/inbound` |
| `note` | 笔记 ID | `agent/outbound` |
| `web` | Web 会话 trace ID | `api/handler_chat` |
| `bot` / `mcp` / `skill` / `sp` | 机器人 / MCP / 技能 / 搜索提供商配置 | `api/handler_bot*` |
| `tool` / `uc` | 工具调用 ID / 用户选择问题 ID | `api/handler_chat`、`tools/user_choice` |
| `wf` / `compact` / `cron` / `dream` / `ws` / `tp` | 工作流 / 历史压缩 / 定时任务 / 做梦阶段 / 沙箱工作空间 / 工具权限 | 各自模块 |

新增 ID 场景时请沿用两到四个小写字母的短前缀；前缀不影响唯一性保证，纯粹用于日志可读性和排查。

