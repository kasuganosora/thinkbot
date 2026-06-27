# util/strutil — 字符串工具

提供零依赖的字符串处理辅助函数，覆盖安全截断、LLM JSON 提取和 Map 键提取场景。

## 函数列表

### Truncate — 安全截断

按 Unicode 码点（rune）计数截断，超出时追加 `...`。适用于日志和错误消息。

```go
short := strutil.Truncate("你好世界Hello World", 5)
// → "你好世界H..."

// 不超长时原样返回
same := strutil.Truncate("短文本", 10)
// → "短文本"
```

### ExtractJSON — LLM JSON 提取

从可能包含额外文本（markdown 代码块、说明文字）的字符串中提取并解析 JSON。

```go
var cfg MyConfig

// 处理 LLM 返回的 markdown 代码块包裹的 JSON
raw := "好的，配置如下：\n```json\n{\"name\": \"test\", \"value\": 42}\n```\n"
err := strutil.ExtractJSON(raw, &cfg)
// → cfg = {Name: "test", Value: 42}

// 直接 JSON 字符串
err := strutil.ExtractJSON(`{"a": 1}`, &result)

// JSON 数组
err := strutil.ExtractJSON("[1, 2, 3]", &list)
```

提取策略（按顺序尝试）：

1. 去除 markdown 代码块标记（` ```json ` / ` ``` `）
2. 直接解析整个字符串
3. 提取第一个 `{` 到最后一个 `}` 的子串
4. 提取第一个 `[` 到最后一个 `]` 的子串
5. 全部失败时返回原始 `json.Unmarshal` 错误

### MapKeys — Map 键提取

返回 `map[string]V` 的键列表（无序）。用于审计日志等场景。

```go
m := map[string]int{"a": 1, "b": 2, "c": 3}
keys := strutil.MapKeys(m) // → ["a", "b", "c"]（顺序不保证）

empty := strutil.MapKeys(map[string]int{}) // → nil
```

泛型约束：`V` 为任意类型，`K` 固定为 `string`。

## 设计

- 全部函数纯标准库实现，无第三方依赖
- 泛型函数 `MapKeys[V any]` 支持任意值类型的 map
- `ExtractJSON` 专为 LLM 输出场景设计（容错 markdown 包裹）
