# util/idgen — 唯一 ID 生成器

基于 `crypto/rand` 的安全随机 ID 生成工具。生成格式为 `{prefix}-{24 hex chars}` 的唯一标识，用于消息 ID、记忆条目 ID、笔记 ID 等场景。

## 用法

```go
import "github.com/kasuganosora/thinkbot/util/idgen"

// 带前缀（推荐，便于日志排查来源）
msgID   := idgen.New("msg")      // → "msg-a3f1b2c4d5e6f7a8b9c0d1e2"
memID   := idgen.New("mem")      // → "mem-9e8d7c6b5a4f3e2d1c0b9a8f"
noteID  := idgen.New("note")     // → "note-7c6b5a4f3e2d1c0b9a8f7e6d"

// 无前缀
rawID := idgen.New("")           // → "a3f1b2c4d5e6f7a8b9c0d1e2"
```

## 设计细节

| 特性 | 说明 |
|------|------|
| 随机源 | `crypto/rand`（密码学安全，非 `math/rand`） |
| 随机字节数 | 12 字节 = 96 位随机空间（碰撞概率 ≈ 2⁻⁹⁶） |
| 编码 | hex（24 字符，全小写） |
| 格式 | `{prefix}-{hex}`，prefix 为空时不含 `-` |
| 失败回退 | `crypto/rand` 极端失败时回退到 `{prefix}-{unix-nano}` |

## 前缀命名规范（约定）

项目中常见的前缀：

| 前缀 | 场景 |
|------|------|
| `msg` | 消息 ID |
| `mem` | 记忆条目 ID |
| `note` | 笔记 ID |
| `web` | Web 来源 |
| `tg` | Telegram 来源 |
| `misskey` | Misskey 来源 |

前缀不影响唯一性保证，纯粹用于日志可读性和排查。
