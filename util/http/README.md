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
- [流式错误与重试策略](#流式错误与重试策略)
- [代理支持](#代理支持)
- [Clone 共享连接池](#clone-共享连接池)
- [Dump 调试](#dump-调试)
- [Trace ID 集成](#trace-id-集成)
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
        MaxRetries: 3,
        Backoff: &retry.Backoff{
            Strategy: retry.StrategyExponential,
            Initial:  time.Second,
            Max:      30 * time.Second,
        },
    }),
    httputil.WithRetrySimple(3, 2*time.Second),        // 简单重试（固定间隔）
    httputil.WithMaxBodySize(50*1024*1024),            // 响应体上限（默认 10MB，-1=无限）
    httputil.WithProxy("socks5://127.0.0.1:1080"),     // 代理
    httputil.WithProxyFromEnv(),                       // 从 HTTP_PROXY/HTTPS_PROXY 读取
    httputil.WithDump(),                               // 全局开启 dump 日志
    httputil.WithHTTPClient(customHTTPClient),         // 自定义底层 http.Client
)

// 全默认配置（30s 超时，无 baseURL / 无重试）
client := httputil.DefaultClient()
```

> `WithMaxBodySize` 超限时不会报错，而是截断 Body 并打印一条 warn 日志。
> 流式接口（SSE / Stream / WS）不受该上限约束，并使用零超时的客户端副本，
> 因此 `WithTimeout` 不会中断长连接。

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

配置重试后，对 **可重试状态码**（429 / 5xx，其中 502/503/504 显式命中）和网络错误（无状态码）自动重试：

```go
client := httputil.New(
    // 完整配置
    httputil.WithRetry(retry.Config{
        MaxRetries: 3,
        Backoff: &retry.Backoff{
            Strategy: retry.StrategyExponential,
            Initial:  time.Second,
            Max:      30 * time.Second,
        },
        // ShouldRetry: 可选自定义（默认按上述状态码判断）
    }),
    // 或快捷方式：固定次数 + 固定间隔
    httputil.WithRetrySimple(3, 2*time.Second),
)
```

**Retry-After 头支持**：收到 429 时自动解析 `Retry-After` 响应头（秒数或 HTTP-date），
与退避计算出的间隔取较大值作为下次重试的等待时间。若自行提供了 `GetRetryDelay`，同样取二者较大值。

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

**重试限制**：仅 `DoSSE`（回调模式）支持自动重试；channel 模式（`DoSSEStream` / `DoSSEStreamWithErr`）
不支持，因为已发出的事件无法撤回。此外传入外部 `Watchdog` 时重试也会被忽略并打印警告
（每次重试需要全新的看门狗）。默认 `ShouldRetry` 为 `DefaultStreamShouldRetry`。

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

`BufferSize` 控制 chunk 模式的读取缓冲（默认 32KB）；行模式固定使用 64KB 的 `bufio.Reader`。

Channel 变体：`DoStreamChunks`、`DoStreamChunksWithErr`、`DoStreamLines`、`DoStreamLinesWithErr`
（与 SSE 一样，channel 变体不支持自动重试）。

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
| `URL()` | 连接的 URL |
| `Watchdog()` | 关联的看门狗（可能为 nil） |
| `Underlying()` | 获取底层 `*websocket.Conn` |

### 手动模式

`DialWS(cfg)` 只建立连接并返回 `*WSConn`，读取循环由调用方自行控制：

```go
conn, err := client.Get("/ws").DialWS(httputil.WSConfig{
    WatchdogTimeout: 120 * time.Second,
    ReadLimit:       1 << 20,
})
defer conn.Close()
```

### 常量与辅助函数

消息类型常量与 gorilla/websocket 对齐：`WSTextMessage`、`WSBinaryMessage`、
`WSCloseMessage`、`WSPingMessage`、`WSPongMessage`。

| 函数 | 说明 |
|------|------|
| `FormatWSCloseMessage(code, text)` | 构造关闭帧负载 |
| `IsWSCloseError(err, codes...)` | 是否为指定关闭码的关闭错误 |
| `IsWSUnexpectedCloseError(err)` | 是否为意外关闭 |
| `WSCloseCode(err)` | 提取关闭码，非关闭错误返回 -1 |

其他行为：

- URL 协议自动转换：`http://` → `ws://`、`https://` → `wss://`（已是 `ws(s)://` 则原样使用）
- 默认注册 Pong handler：收到 Pong 自动 Feed 看门狗
- 后台监听 context：用户取消或看门狗超时会自动发送 Close 帧并关闭连接，使阻塞的读取立即返回
- `HandshakeTimeout` 为 0 时由 gorilla 使用其默认值（45s）

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

`WatchdogTimeoutError` 包含诊断信息：`URL`、`ItemsReceived`、`BytesReceived`、`Elapsed`、`WatchdogName`。
它的 `Unwrap()` 返回 `watchdog.ErrWatchdogTimeout`，因此 `errors.Is(err, watchdog.ErrWatchdogTimeout)` 同样成立。

---

## 流式错误与重试策略

### StreamHTTPError

流式连接在**建立阶段**收到非预期状态码时返回（SSE 要求严格 200，Stream 允许 2xx）。
保留原始错误响应体（最多 64KB），便于上层 SDK 解析为具体的 API 错误：

```go
var se *httputil.StreamHTTPError
if errors.As(err, &se) {
    log.Printf("status=%d body=%s", se.StatusCode, se.Body)
}
```

字段：`StatusCode` / `Body` / `Headers` / `URL`。

### 可复用的 ShouldRetry / GetRetryDelay

| 函数 | 策略 |
|------|------|
| `DefaultStreamShouldRetry` | 仅当「看门狗超时且本次连接未收到任何数据」时重试；其他一律不重试 |
| `StreamShouldRetry` | 在上者基础上：context 取消不重试；`StreamHTTPError` 状态码 ∈ {408,429,500,502,503,504,529} 重试（429/403 且 body 命中额度耗尽特征时除外），其余 4xx 不重试；其他网络层错误默认重试 |
| `StreamGetRetryDelay` | 从 `StreamHTTPError` 的 `Retry-After-MS`（优先）/ `Retry-After`（秒数或 HTTP-date）解析建议延迟 |

```go
cfg := retry.Config{
    MaxRetries:    3,
    ShouldRetry:   httputil.StreamShouldRetry,
    GetRetryDelay: httputil.StreamGetRetryDelay,
}
```

### 配额耗尽识别（quota exhausted）

429 有两种语义：瞬时限流（可退避重试）与配额/额度耗尽（重置前重试必然失败）。
`StreamShouldRetry` 对后者自动短路不重试，由以下函数支撑：

| 函数 | 说明 |
|------|------|
| `IsQuotaExhausted(err)` | 仅对 `*StreamHTTPError` 且状态码为 429/403 生效；body 命中额度耗尽特征（如 `insufficient_quota`、`"code":"1308"`、`使用上限`、`余额不足` 等）时返回 true |
| `IsQuotaExhaustedLoose(err)` | 纯文本兜底：错误链中拿不到 `*StreamHTTPError` 时，从 `err.Error()` 文本判断（需同时出现 429/403 与额度措辞） |
| `QuotaResetAt(err)` | 尽力解析配额恢复时刻：优先 `Retry-After` 响应头，其次 body 中的绝对时间戳；拿不到或已过期返回 `ok=false`（不编造默认值） |
| `QuotaResetAtLoose(err)` | `QuotaResetAt` 的纯文本兜底版本 |

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

// Clone 为浅拷贝：共享底层 *http.Client（Transport / 连接池 / TLS），
// 但 headers 是独立副本，baseURL / retryCfg / maxBodySize / dumpEnabled 各自持有
c1 := base.Clone()
c2 := base.Clone()
```

`Clone()` 不接受 Option 参数；如需修改 baseURL 等配置，请在 `New(opts...)` 时一次性指定。

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

如果 context 中包含 Trace ID（通过 `util/traceid` 包），构建请求时会自动注入到请求头
`X-Trace-ID`（即 `traceid.HeaderKey`），实现全链路追踪。若已手动设置该头则不覆盖。

```go
ctx = traceid.WithTraceID(ctx, "4bf92f3577b34da6a3ce929d0e0e4736")
// 后续通过此 ctx 发出的所有请求自动携带 X-Trace-ID
resp, err := client.Get("/data").SetContext(ctx).Do()
```

请求/响应日志同样通过 `traceid.L(ctx)` 输出，自动带上 `trace_id` 字段。

---

## 文件结构

```
util/http/
├── client.go       # Client + Option + Request 链式构造 + Response + 便捷方法 + Dump
├── errors.go       # WatchdogTimeoutError / IsWatchdogTimeout + DefaultStreamShouldRetry
│                   # + StreamShouldRetry + StreamGetRetryDelay + 配额耗尽识别（IsQuotaExhausted 等）
├── sse.go          # SSE 事件流：DoSSE / DoSSEStream / DoSSEStreamWithErr
├── stream.go       # 原始流式响应：DoStream / DoStreamChunks / DoStreamLines
├── stream_conn.go  # 流式连接共用逻辑：streamConnect + StreamHTTPError + classifyStreamError
├── ws.go           # WebSocket：DialWS / DoWS / DoWSMessages + WSConn + WSConfig
└── multipart.go    # MultipartForm 表单构造器
```
