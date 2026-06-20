# tools — 内建工具集

提供开箱即用的工具实现，可在任何 Bot 中注册使用。

## 可用工具

| 工具 | 文件 | 说明 |
|------|------|------|
| **calc** | `calc.go` | 数学表达式计算 |
| **datetime** | `datetime.go` | 获取当前日期/时间 |
| **now** | `now.go` | 时间戳查询（简化版） |
| **http** | `http.go` | HTTP 请求（GET/POST） |
| **web_search** | `web_search.go` | 网络搜索 |
| **text_tools** | `text_tools.go` | 文本处理（统计/分割/替换） |
| **prompt** | `prompt.go` | 提示词段落（系统工具说明） |

## 使用示例

```go
import "github.com/kasuganosora/thinkbot/tools"

// 注册全部内建工具
for _, t := range tools.Defaults() {
    toolMgr.Register(t.Name, t.Description, t.Parameters, t.Execute)
}

// 或单独注册
toolMgr.Register("calc", "计算器", tools.CalcSchema, tools.CalcExecute)
```

每个工具遵循 `llm.Tool` 结构，包含 `Name`、`Description`、`Parameters`（JSON Schema）和 `Execute` 函数。
