package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/watchdog"
)

// ============================================================================
// SSE Event
// ============================================================================

// SSEEvent 表示一个 Server-Sent Event 事件。
type SSEEvent struct {
	// Event 事件类型（event: 行），为空时表示默认 "message"。
	Event string
	// Data 数据内容（data: 行，多行用 \n 连接）。
	Data string
	// ID 事件 ID（id: 行）。
	ID string
	// Retry 服务器建议的重连间隔（retry: 行，毫秒）。0 表示未设置。
	Retry int
}

// JSON 尝试将 Data 按 JSON 解析到 v。
func (e *SSEEvent) JSON(v any) error {
	return json.Unmarshal([]byte(e.Data), v)
}

// ============================================================================
// SSE 配置
// ============================================================================

// SSEConfig 配置 SSE 连接行为。
type SSEConfig struct {
	// Watchdog 流式看门狗：每当收到事件时自动 Feed。
	// 用于监控 SSE 流是否有持续数据传入。
	Watchdog *watchdog.Watchdog

	// WatchdogTimeout 如果设置，且 Watchdog 为 nil，则自动创建一个
	// 以此超时的看门狗（从 Request 的 context 派生）。
	// 请求结束后自动 Stop。
	WatchdogTimeout time.Duration

	// RetryConfig 重试配置（可选），复用 retry.Config。
	// 仅在看门狗超时（非用户主动取消）时生效。
	// 如果 ShouldRetry 为 nil，使用 DefaultStreamShouldRetry。
	// 注意：使用外部 Watchdog 时重试不可用（每次重试需要全新的看门狗）。
	RetryConfig *retry.Config

	// OnConnect 连接建立后的回调（可选）。
	OnConnect func(resp *http.Response)

	// OnEvent 每收到一个事件的回调（可选）。
	// 返回 error 可以中断流（中断时返回该 error）。
	OnEvent func(event SSEEvent) error

	// OnError 流读取遇到错误时的回调（可选）。
	OnError func(err error)
}

// ============================================================================
// SSE 执行
// ============================================================================

// DoSSE 以 SSE 方式执行请求，持续读取事件流直到连接关闭或 context 取消。
//
// 如果设置了 RetryConfig，看门狗超时会自动重试（仅当未收到任何数据时，或 ShouldRetry 返回 true 时）。
// 用户主动取消（context 取消）不会触发重试。
//
// 返回值：
//   - nil: 流正常结束（服务端关闭连接）
//   - *WatchdogTimeoutError: 看门狗超时（且未重试或重试已耗尽）
//   - context.Canceled: 用户主动取消
//   - 其他 error: 连接错误、回调错误等
func (r *Request) DoSSE(cfg SSEConfig) error {
	// 无重试配置 → 直接执行
	if cfg.RetryConfig == nil || cfg.RetryConfig.MaxRetries == 0 {
		return r.doSSEInternal(cfg)
	}
	// 外部 Watchdog 不支持重试（每次重试需要全新看门狗）
	if cfg.Watchdog != nil {
		log.Logger.Warnw("sse retry not supported with external watchdog, ignoring retry config",
			"url", r.url)
		return r.doSSEInternal(cfg)
	}
	return r.doSSEWithRetry(cfg)
}

// DoSSEStream 以 SSE 方式执行请求，通过 channel 持续输出事件。
//
// 返回的 channel 在流结束或出错时关闭。
// 如果设置了 SSEConfig.Watchdog 或 WatchdogTimeout，每收到事件会自动喂狗。
//
// 注意：channel 模式不支持自动重试（已发送的事件无法撤回）。
// 如果需要重试，请使用 DoSSE + OnEvent 回调。
func (r *Request) DoSSEStream(cfg SSEConfig) (<-chan SSEEvent, error) {
	ch := make(chan SSEEvent, 64)

	cfg.OnEvent = func(event SSEEvent) error {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case ch <- event:
			return nil
		}
	}

	go func() {
		defer close(ch)
		// 错误静默丢弃（此变体不返回 error channel）
		_ = r.doSSEInternal(cfg)
	}()

	return ch, nil
}

// DoSSEStreamWithErr 与 DoSSEStream 相同，但额外返回一个 error channel。
// error channel 在流结束时收到最终错误（nil 表示正常结束），然后关闭。
//
// 用于区分"正常结束"vs"连接失败"vs"看门狗超时"。
func (r *Request) DoSSEStreamWithErr(cfg SSEConfig) (<-chan SSEEvent, <-chan error) {
	ch := make(chan SSEEvent, 64)
	errCh := make(chan error, 1)

	cfg.OnEvent = func(event SSEEvent) error {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case ch <- event:
			return nil
		}
	}

	go func() {
		defer close(ch)
		defer close(errCh)
		err := r.doSSEInternal(cfg)
		errCh <- err
	}()

	return ch, errCh
}

// ============================================================================
// SSE 重试循环
// ============================================================================

// doSSEWithRetry 带重试执行 SSE 请求。
//
// 每次重试创建全新的看门狗和 HTTP 连接。
// 用户主动取消不会触发重试（retry.Do 会检测 context 取消并立即返回）。
// 重试时自动携带 Last-Event-ID 请求头（如果之前的连接收到过事件 ID）。
func (r *Request) doSSEWithRetry(cfg SSEConfig) error {
	retryCfg := *cfg.RetryConfig // copy
	origCtx := r.ctx

	// 默认 ShouldRetry：仅对看门狗超时（未收到数据）重试
	if retryCfg.ShouldRetry == nil {
		retryCfg.ShouldRetry = DefaultStreamShouldRetry
	}

	name := fmt.Sprintf("SSE %s %s", r.method, r.url)

	res := retry.Do(origCtx, name, retryCfg, func(ctx context.Context) error {
		r.ctx = origCtx // 每次重试使用原始 context（doSSEInternal 会创建新看门狗）
		// 如果有 Last-Event-ID，在重试时自动携带（SSE 规范断点续传）
		if r.sseLastEventID != "" {
			r.headers["Last-Event-ID"] = r.sseLastEventID
		}
		return r.doSSEInternal(cfg)
	})

	return res.Err
}

// ============================================================================
// SSE 核心读取逻辑
// ============================================================================

// doSSEInternal 单次执行 SSE 请求。
//
// 返回值：
//   - nil: 流正常结束
//   - *WatchdogTimeoutError: 看门狗超时
//   - context.Canceled: 用户主动取消（非看门狗）
//   - 其他 error: 连接错误、回调错误等
func (r *Request) doSSEInternal(cfg SSEConfig) error {
	// --- 设置 SSE 必需头 ---
	r.headers["Accept"] = "text/event-stream"
	r.headers["Cache-Control"] = "no-cache"

	// --- 建立连接（共用逻辑）---
	conn, err := r.streamConnect("SSE", cfg.Watchdog, cfg.WatchdogTimeout, true, cfg.OnError)
	if err != nil {
		return err
	}

	resp := conn.resp
	wd := conn.wd
	reqURL := conn.reqURL
	origCtx := conn.origCtx
	start := conn.start

	// 恢复原始 context（streamConnect 中已设置，但这里显式管理）
	defer func() { r.ctx = origCtx }()

	if conn.wdOwned {
		defer wd.Stop(true)
	}
	defer resp.Body.Close()

	if cfg.OnConnect != nil {
		cfg.OnConnect(resp)
	}

	// --- 读取事件流 ---
	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024) // 1MB max

	var event SSEEvent
	eventStarted := false
	eventsReceived := 0
	bytesReceived := 0

	for scanner.Scan() {
		line := scanner.Text()
		bytesReceived += len(line) + 1 // +1 for the newline

		// 空行 = 事件分隔符
		if line == "" {
			if eventStarted {
				// 喂狗
				if wd != nil {
					wd.Feed()
				}
				// 设置默认事件类型
				if event.Event == "" {
					event.Event = "message"
				}
				// 去掉 Data 末尾多余换行
				event.Data = strings.TrimSuffix(event.Data, "\n")

				eventsReceived++

				// 回调
				if cfg.OnEvent != nil {
					if err := cfg.OnEvent(event); err != nil {
						log.Logger.Debugw("sse stream interrupted by callback",
							"url", reqURL, "err", err)
						return err
					}
				}
			}
			// 重置
			event = SSEEvent{}
			eventStarted = false
			continue
		}

		eventStarted = true

		// 解析字段
		field, value, ok := splitSSELine(line)
		if !ok {
			continue // 注释行或无效行
		}

		switch field {
		case "event":
			event.Event = value
		case "data":
			if event.Data != "" {
				event.Data += "\n"
			}
			event.Data += value
		case "id":
			event.ID = value
			// 记录最后一个事件 ID（用于自动重连 Last-Event-ID）
			r.sseLastEventID = value
		case "retry":
			if ms, err := strconv.Atoi(value); err == nil && ms >= 0 {
				event.Retry = ms
			}
		}
	}

	// 处理流中最后一个未以空行结尾的事件
	if eventStarted {
		if wd != nil {
			wd.Feed()
		}
		if event.Event == "" {
			event.Event = "message"
		}
		event.Data = strings.TrimSuffix(event.Data, "\n")
		eventsReceived++
		if cfg.OnEvent != nil {
			if err := cfg.OnEvent(event); err != nil {
				return err
			}
		}
	}

	// --- 判断流结束原因 ---
	if scanErr := scanner.Err(); scanErr != nil {
		return classifyStreamError(scanErr, origCtx, wd, reqURL, start, eventsReceived, bytesReceived, "sse")
	}

	log.Logger.Debugw("sse stream ended normally",
		"url", reqURL, "events_received", eventsReceived,
		"elapsed", time.Since(start))
	return nil
}

// ============================================================================
// 内部工具
// ============================================================================

// splitSSELine 将 SSE 行拆分为字段名和值。
func splitSSELine(line string) (field, value string, ok bool) {
	if strings.HasPrefix(line, ":") {
		return "", "", false
	}

	field, value, found := strings.Cut(line, ":")
	if !found {
		return line, "", true
	}

	value = strings.TrimPrefix(value, " ")
	return field, value, true
}
