# util/watchdog — 看门狗定时器

提供可复用的看门狗定时器，用于检测长时间无活动的事件循环（如 WebSocket 连接、long polling、SSE 流）。超时后自动取消其管理的 context。

## 快速开始

```go
import "github.com/kasuganosora/thinkbot/util/watchdog"

// 创建看门狗（继承 parent context）
wd := watchdog.New(ctx, 30*time.Second)

// 获取受管理的 context
wdCtx := wd.Context()

// 在事件循环中定期喂狗
for {
    select {
    case msg := <-ch:
        processData(wdCtx, msg)
        wd.Feed()           // 收到数据，重置计时器
    case <-wdCtx.Done():
        if wd.TimedOut() {
            log.Warn("connection stalled, reconnecting...")
        }
        return
    }
}

// 结束时停止
wd.Stop(true) // true = 同时取消 context
```

## 构造函数

| 函数 | 说明 |
|------|------|
| `New(parent, timeout)` | 基础构造，parent 为 nil 时使用 `context.Background()` |
| `NewWithName(parent, timeout, name)` | 带名称，用于日志标识 |
| `NewWithCallback(parent, timeout, onTimeout)` | 超时触发时执行回调 |
| `NewWithNameAndCallback(parent, timeout, name, onTimeout)` | 全参数构造 |

```go
// 带回调：超时时执行重连逻辑
wd := watchdog.NewWithCallback(ctx, 60*time.Second, func() {
    log.Warn("watchdog fired, triggering reconnect")
    reconnect()
})

// 带名称：日志中区分多个看门狗
wd := watchdog.NewWithName(ctx, 30*time.Second, "misskey-ws")
```

## 核心方法

| 方法 | 说明 |
|------|------|
| `Context()` | 返回受管理的 context（超时/Stop 后被取消） |
| `Feed()` | 重置计时器（收到活动时调用） |
| `FeedWithTimeout(d)` | 重置计时器并动态修改超时时长 |
| `Stop(cancel)` | 停止看门狗（cancel=true 同时取消 context） |
| `TimedOut()` | 是否因超时触发（区分外部取消） |
| `Timeout()` | 当前配置的超时时长 |
| `Name()` | 看门狗名称 |

## 超时判断

`TimedOut()` 用于区分 context 被取消的原因：

| `TimedOut()` | 含义 | 典型处理 |
|-------------|------|---------|
| `true` | 数据流卡住，看门狗超时 | 重连 |
| `false` | 外部取消（用户主动 / parent context 取消） | 退出 |

```go
case <-wdCtx.Done():
    if wd.TimedOut() {
        // 数据流卡住 → 重连
        reconnect()
    } else {
        // 外部取消 → 退出
        return
    }
```

## Feed 行为细节

- `Feed()` 仅在未 Stop 时生效
- 如果 context 因**超时**已取消，`Feed()` 不会自动恢复（保持 `TimedOut() == true`）
- 如果 context 因 **parent 取消**（非超时）失效，`Feed()` 会重建 context
- `FeedWithTimeout()` 可以动态调整超时（如首次连接给更长超时，稳定后缩短）

## Parent Context 传播

看门狗自动监听 parent context：

- parent 取消 → 看门狗 context 自动取消 + 日志记录
- 确保不会因 parent 取消而泄漏 goroutine

## 日志集成

- Logger 从 parent context 派生，自动携带 `trace_id`
- 关键事件日志：
  - `watchdog started`（创建时）
  - `watchdog fed`（每次 Feed，Debug 级别）
  - `watchdog timeout! context canceled`（超时触发）
  - `watchdog parent context canceled`（parent 取消传播）
  - `watchdog stopped`（Stop 调用）

## 典型应用场景

### Misskey WebSocket 心跳

```go
wd := watchdog.NewWithName(ctx, 60*time.Second, "misskey-ws")

go func() {
    for {
        // 收到心跳消息
        <-heartbeatCh
        wd.Feed()
    }
}()

select {
case <-wd.Context().Done():
    if wd.TimedOut() {
        reconnect() // 60s 无心跳 → 重连
    }
}
```

### SSE 流式响应

```go
wd := watchdog.NewWithName(ctx, 30*time.Second, "sse")

for event := range sseCh {
    processEvent(event)
    wd.Feed() // 每收到一个事件重置
}
```

## 文件结构

| 文件 | 职责 |
|------|------|
| `watchdog.go` | `Watchdog` 类型、构造函数、Feed/Stop/TimedOut、定时器管理、parent context 传播 |
