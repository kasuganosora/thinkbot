# util/http — HTTP / WebSocket 客户端工具包

统一封装 HTTP 请求、SSE 事件流、原始流式响应和 WebSocket 连接，集成重试、看门狗超时、代理、Trace ID 和结构化日志。

---

## 目录

- [快速开始](#快速开始)
- [核心概念](#核心概念)
- [Client 配置](#client-配置)
- [Request 链式构造](#request-链式构造)
- [重试机制](#重试机制)
- [SSE 事件流](#sse-事件流)
- [原始流式响应](#原始流式响应)
- [WebSocket](#websocket)
- [Multipart 表单](#multipart-表单)
- [看门狗超时](#看门狗超时)
- [代理支持](#代理支持)
- [Clone 共享连接池](#clone-共享连接池)
- [Dump 调试](#dump-调试)
- [文件结构](#文件结构)

---

## 快速开始

```go
import httputil "github.com/kasuganosora/thinkbot/util/http"

// 创建 Client
client := httputil.New(
    httputil.WithBaseURL("https://api.example.com"),
    httputil.WithTimeout(30*time.Second),
    httputil.WithHeader("Authorization", "Bearer sk-xxx"),
)

// GET + JSON 解码
var result ResponseType
err := client.GetJSON(ctx, "/users/123", &result)

// POST JSON
var created CreateResult
err = client.PostJSON(ctx, "/users", body, &created)

// 链式 API（更灵活）
resp, err := client.Post("/v1/chat").
    SetJSONBody(params).
    BearerToken(token).
    SetContext(ctx).
    Do()
if err != nil {
    panic(err)
}
var result ChatResult
resp.JSON(&result)
```

---

## 核心概念

| 类型 | 说明 |
|------|------|
| `Client` | HTTP 客户端封装，持有 baseURL、默认 headers、重试配置、Transport |
| `Request` | 链式请求构造器，由 `Client.Get/Post/Put/Patch/Delete` 创建 |
| `Response` | HTTP 响应，包含 StatusCode / Headers / Body，提供 `JSON()` 和 `IsSuccess()` |
| `SSEEvent` | SSE 事件（Event / Data / ID / Retry），提供 `JSON()` |
| `WSMessage` | WebSocket 消息（Type / Data），提供 `Text()` 和 `JSON()` |
| `WSConn` | WebSocket 连接封装，线程安全的写入 + 自动 Ping + 看门狗 |

---

## Client 配置

```go
client := httputil.New(
    httputil.WithBaseURL("https://api.example.com"),  // 基础 URL，请求路径会拼接其后
    httputil.WithTimeout(60*time.Second),              // HTTP 超时（默认 30s）
    httputil.WithHeader("Authorization", "Bearer xxx"),// 默认请求头（每次请求携带）
    httputil.WithHeaders(map[string]string{            // 批量设置默认头
        "X-Custom": "value",
    }),
    httputil.WithRetry(retry.Config{                   // 重试配置
        MaxRetries:    3,
        BaseDelay:     time.Second,
        MaxDelay:      30 * time.Second,
    }),
    httputil.WithRetrySimple(3, 2*time.Second),        // 简单重试（固定间隔）
    httputil.WithMaxBodySize(50*1024*1024),            // 响应体上限（默认 10MB，-1=无限）
    httputil.WithProxy("socks5://127.0.0.1:1080"),     // 代理
    httputil.WithProxyFromEnv(),                       // 从 HTTP_PROXY/HTTPS_PROXY 读取
    httputil.WithDump(),                               // 全局开启 dump 日志
    httputil.WithHTTPClient(customHTTPClient),         // 自定义底层 http.Client
)
```

---

## Request 链式构造

```go
resp, err := client.Post("/api/messages").
    SetJSONBody(map[string]any{           // JSON 请求体（自动设 Content-Type）
        "role": "user",
        "content": "hello",
    }).
    SetHeader("X-Request-ID", "abc123").   // 请求级别 header
    SetQuery("stream", "false").           // 查询参数
    BearerToken("sk-xxx").                 // 快捷 Bearer 认证
    BasicAuth("user", "pass").             // 或 Basic 认证
    SetContext(ctx).                       // context（支持取消）
    SetRetry(retry.Config{                 // 请求级别重试覆盖
        MaxRetries: 5,
    }).
    Dump().                                // 本请求打印 dump
    Do()                                   // 执行
```

### Response 操作

```go
resp.StatusCode  // int
resp.Headers     // http.Header
resp.Body        // []byte
resp.String()    // string
resp.IsSuccess() // bool (2xx)
resp.JSON(&v)    // JSON 反序列化
```

---

## 重试机制

配置重试后，对 **可重试状态码**（429 / 5xx）和网络错误自动重试：

```go
client := httputil.New(
    httputil.WithRetry(retry.Config{
        MaxRetries:    3,
        BaseDelay:     time.Second,
        MaxDelay:      30 * time.Second,
        // ShouldRetry: 可选自定义（默认 429/5xx/网络错误）
    }),
)
```

**Retry-After 头支持**：收到 429 时自动解析 `Retry-After` 响应头（秒数或 HTTP-date），作为下次重试的最小等待时间。

**per-request 覆盖**：

```go
resp, err := client.Get("/important").
    SetRetry(retry.Config{MaxRetries: 5}).
    Do()
```

---

## SSE 事件流

三种使用模式：

### 回调模式（支持自动重试）

```go
err := client.Get("/events").
    BearerToken(token).
    DoSSE(httputil.SSEConfig{
        WatchdogTimeout: 60 * time.Second,  // 60s 无数据 → 超时
        RetryConfig: &retry.Config{         // 看门狗超时自动重试
            MaxRetries: 3,
        },
        OnConnect: func(resp *http.Response) {
            log.Println("SSE connected")
        },
        OnEvent: func(event httputil.SSEEvent) error {
            fmt.Printf("[%s] %s\n", event.Event, event.Data)
            return nil  // 返回 error 中断流
        },
        OnError: func(err error) {
            log.Printf("SSE error: %v", err)
        },
    })
```

### Channel 模式

```go
ch, err := client.Get("/events").DoSSEStream(httputil.SSEConfig{
    WatchdogTimeout: 60 * time.Second,
})
for event := range ch {
    fmt.Println(event.Data)
}
```

### Channel + Error 模式

```go
ch, errCh := client.Get("/events").DoSSEStreamWithErr(httputil.SSEConfig{
    WatchdogTimeout: 60 * time.Second,
})
for event := range ch {
    fmt.Println(event.Data)
}
if err := <-errCh; err != nil {
    log.Printf("stream ended with: %v", err)
}
```

**Last-Event-ID 自动重连**：重试时自动携带 `Last-Event-ID` 请求头，支持 SSE 规范的断点续传。

---

## 原始流式响应

适用于非 SSE 的流式 HTTP 响应（如 chunked transfer、NDJSON）：

```go
// 按 chunk 读取
err := client.Post("/stream").DoStream(httputil.StreamConfig{
    WatchdogTimeout: 60 * time.Second,
    OnChunk: func(data []byte) error {
        fmt.Print(string(data))
        return nil
    },
})

// 按行读取
err := client.Post("/logs").DoStream(httputil.StreamConfig{
    LineMode: true,
    OnLine: func(line string) error {
        fmt.Println(line)
        return nil
    },
})
```

Channel 变体：`DoStreamChunks`、`DoStreamChunksWithErr`、`DoStreamLines`、`DoStreamLinesWithErr`。

---

## WebSocket

### 回调模式

```go
err := client.Get("/ws").DoWS(httputil.WSConfig{
    WatchdogTimeout:  120 * time.Second,   // 120s 无消息 → 超时断开
    PingInterval:     30 * time.Second,    // 自动 Ping 保活
    EnableCompression: true,
    OnConnect: func(conn *httputil.WSConn) {
        log.Println("WS connected")
    },
    OnText: func(text string) error {
        fmt.Println(text)
        return nil
    },
    OnBinary: func(data []byte) error {
        fmt.Printf("binary: %d bytes\n", len(data))
        return nil
    },
    OnClose: func(code int, text string) {
        log.Printf("WS closed: %d %s", code, text)
    },
})
```

### Channel 模式（可同时读写）

```go
ch, conn, err := client.Get("/ws").DoWSMessages(httputil.WSConfig{
    WatchdogTimeout: 120 * time.Second,
})
defer conn.Close()

// 读
go func() {
    for msg := range ch {
        fmt.Println(msg.Text())
    }
}()

// 写（线程安全）
conn.WriteText("hello")
conn.WriteJSON(map[string]any{"type": "ping"})
conn.WriteBinary([]byte{0x01, 0x02})
```

### WSConn 写入方法

| 方法 | 说明 |
|------|------|
| `WriteText(text)` | 发送文本消息 |
| `WriteJSON(v)` | JSON 序列化后发送为文本消息 |
| `WriteBinary(data)` | 发送二进制消息 |
| `WriteMessage(type, data)` | 发送原始消息 |
| `Ping()` | 发送 Ping 帧 |
| `Close()` / `CloseWithCode(code, text)` | 优雅关闭 |
| `IsClosed()` | 连接是否已关闭 |
| `Underlying()` | 获取底层 `*websocket.Conn` |

URL 协议自动转换：`http://` → `ws://`、`https://` → `wss://`。

---

## Multipart 表单

文件上传和 multipart 表单：

```go
form := httputil.NewMultipartForm().
    AddFile("file", "report.pdf", strings.NewReader(pdfData)).
    AddFileWithMIME("image", "photo.jpg", "image/jpeg", imageReader).
    AddField("purpose", "vision")

resp, err := client.Post("/upload").
    SetMultipart(form).
    Do()
```

---

## 看门狗超时

所有流式连接（SSE / Stream / WebSocket）都支持看门狗超时检测：

```go
// 方式一：自动创建（推荐）
config := SSEConfig{
    WatchdogTimeout: 60 * time.Second,  // 60s 无数据 → 超时
}

// 方式二：传入外部看门狗
wd := watchdog.NewWithName(ctx, 60*time.Second, "my-wd")
config := SSEConfig{
    Watchdog: wd,  // 外部管理生命周期
}
```

**错误分类**：超时返回 `*WatchdogTimeoutError`，用户取消返回 `context.Canceled`：

```go
if httputil.IsWatchdogTimeout(err) {
    // 数据流卡住了，可以重试
} else if errors.Is(err, context.Canceled) {
    // 用户主动取消，不要重试
}
```

`WatchdogTimeoutError` 包含诊断信息：URL、收到的事件/数据块数、字节数、耗时。

---

## 代理支持

```go
// 直接指定
client := httputil.New(httputil.WithProxy("socks5://127.0.0.1:1080"))

// 从环境变量
client := httputil.New(httputil.WithProxyFromEnv())
```

支持的格式：

| 格式 | 说明 |
|------|------|
| `http://host:port` | HTTP 代理 |
| `https://host:port` | HTTPS 代理 |
| `socks5://host:port` | SOCKS5 代理（本地 DNS） |
| `socks5h://host:port` | SOCKS5 代理（DNS 也走代理） |

---

## Clone 共享连接池

多个 Client 共享底层 Transport / 连接池，但拥有独立的 baseURL / headers / 重试配置：

```go
base := httputil.New(
    httputil.WithTimeout(60*time.Second),
    httputil.WithRetry(retry.Config{MaxRetries: 3}),
)

// Clone 后共享 Transport，但 header/baseURL 独立
openai := base.Clone()
openai.SetBaseURL // ... 通过 New(opts...) 或直接修改

// 实际用法：llm 包的 WithSharedClient
sharedHTTP := httputil.New(httputil.WithTimeout(60*time.Second))
prov1 := openai.New(openai.WithSharedClient(sharedHTTP))
prov2 := anthropic.New(anthropic.WithSharedClient(sharedHTTP))
```

---

## Dump 调试

打印完整的请求/响应信息到日志（Authorization 头自动脱敏）：

```go
// 全局开启
client := httputil.New(httputil.WithDump())

// 单请求开启
resp, err := client.Post("/api").Dump().Do()
```

文本类响应体（JSON / XML / text / SSE）完整打印，二进制响应体（图片 / 音视频）仅显示 Content-Length。

---

## Trace ID 集成

如果 context 中包含 Trace ID（通过 `util/traceid` 包），会自动注入到请求头 `X-Trace-ID` 中（可配置 header name），实现全链路追踪。

```go
ctx = traceid.With(ctx, "req-abc-123")
// 后续通过此 ctx 发出的所有请求自动携带 X-Trace-ID: req-abc-123
resp, err := client.Get("/data").SetContext(ctx).Do()
```

---

## 文件结构

```
util/http/
├── client.go       # Client + Option + Request 链式构造 + Response + 便捷方法 + Dump
├── errors.go       # WatchdogTimeoutError + DefaultStreamShouldRetry + IsWatchdogTimeout
├── sse.go          # SSE 事件流：DoSSE / DoSSEStream / DoSSEStreamWithErr
├── stream.go       # 原始流式响应：DoStream / DoStreamChunks / DoStreamLines
├── stream_conn.go  # 流式连接共用逻辑：streamConnect + StreamHTTPError + classifyStreamError
├── ws.go           # WebSocket：DialWS / DoWS / DoWSMessages + WSConn + WSConfig
└── multipart.go    # MultipartForm 表单构造器
```
