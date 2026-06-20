# util/idgen — 分布式 ID 生成器

基于 `crypto/rand` 的安全随机 ID 生成工具，用于生成 Trace ID、会话 ID 等。

## 核心函数

```go
// 生成带前缀的随机 ID（格式：{prefix}-{24 hex chars}）
id := idgen.New("web")     // → "web-a3f1b2c4d5e6f7a8b9c0d1e2"
id := idgen.New("tg")      // → "tg-9e8d7c6b5a4f3e2d1c0b9a8f"

// 生成纯随机 ID（无前缀）
id := idgen.New("")         // → "a3f1b2c4d5e6f7a8b9c0d1e2"
```

## 设计

- 使用 `crypto/rand` 而非 `math/rand`，保证密码学安全性
- 默认生成 12 字节（96 位）随机数据，编码为 24 个十六进制字符
- 前缀用于区分来源（如 `web-`、`tg-`、`misskey-`），便于日志排查
