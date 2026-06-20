# util/strutil — 字符串工具

提供常用的字符串处理辅助函数。

## 核心函数

```go
// 截断字符串（带省略号）
short := strutil.Truncate("很长的文本...", 10) // → "很长的文本…"

// 提取 map 的 key 列表（用于审计日志）
keys := strutil.MapKeys(m) // → ["key1", "key2", "key3"]

// 安全 JSON 解析
val, ok := strutil.ParseJSON[MyType](rawJSON)

// 指针工具
s := strutil.Ptr("hello") // → *string
n := strutil.PtrOr(42, nil) // → 42
```

## 设计

- 所有函数零依赖，纯标准库实现
- 泛型函数用于 map key 提取和 JSON 解析
