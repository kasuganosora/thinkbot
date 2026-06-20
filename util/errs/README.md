# util/errs — 结构化错误处理

提供带 HTTP 状态码映射的错误类型，统一 API 层的错误响应格式。

## 核心函数

```go
// 创建错误
err := errs.BadRequest("invalid input")
err := errs.Unauthorized("not logged in")
err := errs.NotFound("user not found")
err := errs.Internal("unexpected failure")
err := errs.Newf("job %q not found", jobID)

// 包装
err := errs.Wrap(dbErr, "failed to query user")

// 提取 HTTP 状态码
code := errs.GetCode(err) // 返回 400/401/404/500 等
```

## 错误码映射

| 构造函数 | HTTP 状态码 |
|----------|------------|
| `BadRequest()` | 400 |
| `Unauthorized()` | 401 |
| `Forbidden()` | 403 |
| `NotFound()` | 404 |
| `Internal()` | 500 |
| `Newf()` | 400（默认） |
| `Wrap()` | 继承被包装错误的码 |
