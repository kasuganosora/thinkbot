# util/errs — 结构化错误处理

提供项目统一的错误类型 `Error`，携带 HTTP 状态码、调用堆栈和结构化上下文字段，与标准库 `errors` 包完全兼容。

## 快速开始

```go
import "github.com/kasuganosora/thinkbot/util/errs"

// HTTP 快捷构造
err := errs.BadRequest("invalid input")
err := errs.NotFound("user %q not found", userID)

// 自定义状态码
err := errs.HTTPErrorf(409, "conflict: version mismatch")

// 包装底层错误 + 追加上下文
err := errs.Wrap(dbErr, "failed to query user").
    With("user_id", userID).
    With("operation", "lookup")
```

## 核心类型

### `Error` 结构

| 字段 | 说明 |
|------|------|
| `message` | 错误描述（格式化后的） |
| `cause` | 原始错误（`Unwrap()` 返回值，可为 nil） |
| `code` | HTTP 状态码（0 表示未设置） |
| `stack` | 创建时的调用堆栈快照 |
| `context` | 结构化上下文字段（`With()` 追加） |

实现了标准库 `error` 接口 + `Unwrap()`，支持 `errors.Is` / `errors.As` 链式解包。

## 构造函数

| 函数 | 签名 | 说明 |
|------|------|------|
| `New(msg)` | `string → *Error` | 创建错误，自动捕获堆栈 |
| `Newf(fmt, args...)` | `→ *Error` | 格式化消息创建错误 |
| `Wrap(err, msg)` | `error, string → *Error` | 包装底层错误（err 为 nil 时返回 nil） |
| `Wrapf(err, fmt, args...)` | `→ *Error` | 格式化包装 |
| `HTTPError(code, msg)` | `int, string → *Error` | 指定 HTTP 状态码，msg 为空时取 `http.StatusText` |
| `HTTPErrorf(code, fmt, args...)` | `→ *Error` | 格式化消息 + HTTP 状态码 |

### HTTP 快捷构造

| 函数 | HTTP 状态码 |
|------|------------|
| `BadRequest(msg)` | 400 |
| `Unauthorized(msg)` | 401 |
| `Forbidden(msg)` | 403 |
| `NotFound(msg)` | 404 |
| `Conflict(msg)` | 409 |
| `Internal(msg)` | 500 |
| `ServiceUnavailable(msg)` | 503 |

## 链式操作

```go
// With 追加上下文字段（返回新实例，不改原对象）
err := errs.Internal("DB error").With("table", "users").With("query", sql)

// WithCode 覆盖状态码
err := errs.New("custom error").WithCode(422)
```

| 方法 | 说明 |
|------|------|
| `With(key, value)` | 追加结构化字段，返回新 `*Error` |
| `WithCode(code)` | 设置/覆盖 HTTP 状态码，返回新 `*Error` |
| `Code()` | 返回 HTTP 状态码 |
| `StackTrace()` | 返回格式化堆栈字符串 |
| `Context()` | 返回上下文字段副本 |

## 提取与判断

```go
// 从错误链提取 HTTP 状态码（遍历 Unwrap 链）
code := errs.GetCode(err) // 400/404/500...，未找到返回 0

// 提取堆栈
stack := errs.GetStackTrace(err)

// 提取最内层原始错误（兼容 pkg/errors 和标准库）
root := errs.Cause(err)

// 包级 Is / As 代理
ok := errs.Is(err, io.EOF)
var httpErr *errs.Error
errs.As(err, &httpErr)
```

## 日志集成

自动按 HTTP 状态码分级输出，并携带堆栈和上下文字段：

| 条件 | 日志级别 |
|------|---------|
| 4xx | `Warn`（客户端错误，预期内） |
| 5xx / 无状态码 | `Error`（服务端错误） |

```go
// 记录后返回原错误，便于链式
return errs.LogAndReturn(err)

// 带额外 context 字段
errs.LogWith(ctx, err, "request_id", reqID)

// 直接记录
errs.Log(err)
```

日志输出包含：`err`（错误消息）、`code`（HTTP 状态码）、`stack`（堆栈）、所有 `With()` 字段。

## 堆栈捕获

- 所有构造函数自动调用 `runtime.Callers` 捕获 32 帧调用堆栈
- 跳过 `runtime` 和本包内部帧
- `GetStackTrace()` 从错误链中提取（支持嵌套 Wrap）

## 文件结构

| 文件 | 职责 |
|------|------|
| `errs.go` | `Error` 类型、构造函数、堆栈捕获、日志集成 |
| `std.go` | 桥接标准库 `errors.Is` / `errors.As`，避免与本包同名函数冲突 |
