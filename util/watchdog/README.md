# util/watchdog — 看门狗定时器

提供可复用的看门狗定时器，用于检测长时间无活动的事件循环（如 WebSocket 连接、long polling）。

## 核心类型

```go
// 创建看门狗（超时 30s）
wd := watchdog.New(30 * time.Second)

// 喂狗（重置定时器）
wd.Feed()

// 启动监控（返回 channel，超时触发）
<-wd.Watch(ctx) // 阻塞直到超时或 ctx 取消

// 停止
wd.Stop()
```

## 设计

- 单 goroutine + `time.Timer` 实现，零锁
- `Feed()` 重置计时器；超时后 `Watch()` channel 发送信号
- 典型用途：检测 Misskey WebSocket 连接是否静默断开
