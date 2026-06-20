# util/http — HTTP 客户端工具

提供带重试、超时、JSON 解码的 HTTP 客户端封装。

## 核心函数

```go
// 带 JSON 请求体和 JSON 响应解析
resp, err := http.PostJSON[ResponseType](
    ctx, url, requestBody, headers,
)

// 简单 GET + JSON 解码
resp, err := http.GetJSON[ResponseType](
    ctx, url, headers,
)

// 通用请求
resp, err := http.Request(ctx, http.MethodPost, url, body, headers)
```

## 特性

- 泛型支持：`http.GetJSON[T]` 自动解码到目标类型
- 内置重试：网络错误自动重试（可配置次数和退避）
- Context 传播：所有请求支持 `context.Context` 取消
- 默认 User-Agent 和超时设置
