package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/watchdog"
)

// ============================================================================
// 消息类型常量（与 gorilla/websocket 对齐）
// ============================================================================

const (
	// WSTextMessage 文本消息。
	WSTextMessage = websocket.TextMessage
	// WSBinaryMessage 二进制消息。
	WSBinaryMessage = websocket.BinaryMessage
	// WSCloseMessage 关闭消息。
	WSCloseMessage = websocket.CloseMessage
	// WSPingMessage Ping 消息。
	WSPingMessage = websocket.PingMessage
	// WSPongMessage Pong 消息。
	WSPongMessage = websocket.PongMessage
)

// ============================================================================
// WSMessage
// ============================================================================

// WSMessage 表示一条 WebSocket 消息。
type WSMessage struct {
	// Type 消息类型（WSTextMessage / WSBinaryMessage 等）。
	Type int
	// Data 消息数据。
	Data []byte
}

// IsText 判断是否为文本消息。
func (m WSMessage) IsText() bool { return m.Type == WSTextMessage }

// IsBinary 判断是否为二进制消息。
func (m WSMessage) IsBinary() bool { return m.Type == WSBinaryMessage }

// Text 返回文本内容（仅适用于文本消息）。
func (m WSMessage) Text() string { return string(m.Data) }

// JSON 将消息数据按 JSON 解析到 v。
func (m WSMessage) JSON(v any) error { return json.Unmarshal(m.Data, v) }

// ============================================================================
// WSConfig
// ============================================================================

// WSConfig 配置 WebSocket 连接行为。
type WSConfig struct {
	// Watchdog 看门狗：每当收到消息时自动 Feed。
	// 用于检测连接是否"卡住"（长时间无数据）。
	Watchdog *watchdog.Watchdog

	// WatchdogTimeout 如果设置，且 Watchdog 为 nil，则自动创建一个
	// 以此超时的看门狗（从 Request 的 context 派生）。
	// 连接结束后自动 Stop。
	WatchdogTimeout time.Duration

	// Subprotocols 子协议列表（Sec-WebSocket-Protocol）。
	Subprotocols []string

	// HandshakeTimeout 握手超时时间。0 = 使用默认 45s。
	HandshakeTimeout time.Duration

	// ReadLimit 单条消息最大字节数。0 = 不限制。
	ReadLimit int64

	// EnableCompression 启用 permessage-deflate 压缩。
	EnableCompression bool

	// WriteTimeout 写操作超时。0 = 不超时。
	WriteTimeout time.Duration

	// PingInterval 自动发送 Ping 的间隔，用于保活。0 = 不自动 Ping。
	// 收到对端 Pong 时自动 Feed 看门狗。
	PingInterval time.Duration

	// OnConnect 连接建立后的回调（可选）。
	OnConnect func(conn *WSConn)

	// OnMessage 收到任何消息（text/binary）的回调（可选）。
	// 在 OnText / OnBinary 之前调用。
	OnMessage func(msg WSMessage) error

	// OnText 收到文本消息的回调（可选）。
	OnText func(text string) error

	// OnBinary 收到二进制消息的回调（可选）。
	OnBinary func(data []byte) error

	// OnClose 连接关闭的回调（可选）。
	OnClose func(code int, text string)

	// OnError 错误回调（可选）。
	OnError func(err error)
}

// ============================================================================
// WSConn
// ============================================================================

// WSConn 封装 gorilla/websocket.Conn，集成看门狗、日志和写锁。
type WSConn struct {
	conn         *websocket.Conn
	wd           *watchdog.Watchdog
	wdOwned      bool
	ctx          context.Context // 用户原始 context（用于判断 user cancel）
	url          string
	closed       atomic.Bool
	writeMu      sync.Mutex // 保护并发 Write
	writeTimeout time.Duration
	done         chan struct{} // 连接关闭后 close
}

// WriteText 发送文本消息。
func (c *WSConn) WriteText(text string) error {
	return c.WriteMessage(WSTextMessage, []byte(text))
}

// WriteJSON 将 v 序列化为 JSON 并发送为文本消息。
func (c *WSConn) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return errs.Wrap(err, "ws write json marshal")
	}
	return c.WriteMessage(WSTextMessage, data)
}

// WriteBinary 发送二进制消息。
func (c *WSConn) WriteBinary(data []byte) error {
	return c.WriteMessage(WSBinaryMessage, data)
}

// WriteMessage 发送原始 WebSocket 消息（线程安全）。
func (c *WSConn) WriteMessage(msgType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errs.New("ws connection already closed")
	}
	if c.writeTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	} else {
		_ = c.conn.SetWriteDeadline(time.Time{}) // no deadline
	}
	if err := c.conn.WriteMessage(msgType, data); err != nil {
		return errs.Wrap(err, "ws write message")
	}
	return nil
}

// Ping 发送 Ping 消息（携带可选 appData）。
func (c *WSConn) Ping() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errs.New("ws connection already closed")
	}
	if c.writeTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	if err := c.conn.WriteMessage(WSPingMessage, nil); err != nil {
		return errs.Wrap(err, "ws ping")
	}
	return nil
}

// Close 优雅关闭连接（发送 Close 帧）。
func (c *WSConn) Close() error {
	return c.CloseWithCode(websocket.CloseNormalClosure, "")
}

// CloseWithCode 发送指定关闭码和消息后关闭连接。
func (c *WSConn) CloseWithCode(code int, text string) error {
	c.writeMu.Lock()
	if c.closed.Swap(true) {
		c.writeMu.Unlock()
		return nil // already closed
	}
	_ = c.conn.WriteControl(
		WSCloseMessage,
		websocket.FormatCloseMessage(code, text),
		time.Now().Add(5*time.Second),
	)
	err := c.conn.Close()
	close(c.done) // 通知 context-watcher 停止监听
	c.writeMu.Unlock()

	// 停止看门狗
	if c.wdOwned && c.wd != nil {
		c.wd.Stop(true)
	}

	log.Logger.Debugw("ws connection closed", "url", c.url, "code", code, "text", text)
	if err != nil {
		return errs.Wrap(err, "ws close")
	}
	return nil
}

// Underlying 返回底层 *websocket.Conn（调用方自行处理并发）。
func (c *WSConn) Underlying() *websocket.Conn { return c.conn }

// URL 返回连接的 URL。
func (c *WSConn) URL() string { return c.url }

// IsClosed 返回连接是否已关闭。
func (c *WSConn) IsClosed() bool { return c.closed.Load() }

// Watchdog 返回关联的看门狗（可能为 nil）。
func (c *WSConn) Watchdog() *watchdog.Watchdog { return c.wd }

// autoPing 后台定期发送 Ping 消息。
func (c *WSConn) autoPing(interval time.Duration, ctx context.Context) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.IsClosed() {
				return
			}
			if err := c.Ping(); err != nil {
				log.Logger.Debugw("ws auto-ping failed", "url", c.url, "err", err)
				return
			}
		}
	}
}

// ============================================================================
// DialWS — 建立连接
// ============================================================================

// DialWS 建立 WebSocket 连接并返回 *WSConn。
//
// 调用方可通过返回的 *WSConn 进行读写操作。
// 使用完毕后应调用 Close() 释放资源。
//
// 如果设置了 WatchdogTimeout（或外部 Watchdog），每次收到消息或 Pong 会自动 Feed。
// 连接因看门狗超时断开时，读取方法会返回 *WatchdogTimeoutError。
func (r *Request) DialWS(cfg WSConfig) (*WSConn, error) {
	fullURL := r.buildWSURL()

	// --- 看门狗 ---
	origCtx := r.ctx
	var wd *watchdog.Watchdog
	var wdOwned bool
	if cfg.Watchdog != nil {
		wd = cfg.Watchdog
	} else if cfg.WatchdogTimeout > 0 {
		wd = watchdog.NewWithName(origCtx, cfg.WatchdogTimeout, "ws-watchdog")
		wdOwned = true
	}

	// --- Dialer ---
	dialer := websocket.Dialer{
		Subprotocols:      cfg.Subprotocols,
		HandshakeTimeout:  cfg.HandshakeTimeout,
		EnableCompression: cfg.EnableCompression,
	}

	// --- headers ---
	header := http.Header{}
	for k, v := range r.headers {
		header.Set(k, v)
	}

	start := time.Now()
	log.Logger.Debugw("ws connecting", "url", fullURL)

	dialCtx := origCtx
	if wd != nil {
		dialCtx = wd.Context()
	}

	conn, resp, err := dialer.DialContext(dialCtx, fullURL, header)
	if err != nil {
		if wdOwned && wd != nil {
			wd.Stop(true)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}

		// 看门狗超时
		if wd != nil && wd.TimedOut() {
			log.Logger.Warnw("ws dial failed (watchdog timeout)",
				"url", fullURL, "elapsed", time.Since(start))
			return nil, &WatchdogTimeoutError{
				URL:          fullURL,
				Elapsed:      time.Since(start),
				WatchdogName: wd.Name(),
			}
		}
		// 用户主动取消
		if origCtx.Err() != nil {
			log.Logger.Debugw("ws dial canceled by user", "url", fullURL)
			return nil, context.Canceled
		}
		// 普通 HTTP 错误（如 401/403）
		if resp != nil && resp.StatusCode != 0 {
			if cfg.OnError != nil {
				cfg.OnError(err)
			}
			return nil, errs.HTTPErrorf(resp.StatusCode,
				"ws dial to %s returned status %d", fullURL, resp.StatusCode)
		}
		if cfg.OnError != nil {
			cfg.OnError(err)
		}
		return nil, errs.Wrapf(err, "ws dial failed")
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	// --- 配置连接 ---
	if cfg.ReadLimit > 0 {
		conn.SetReadLimit(cfg.ReadLimit)
	}

	// 默认 Pong handler：Feed 看门狗
	conn.SetPongHandler(func(string) error {
		if wd != nil {
			wd.Feed()
		}
		return nil
	})

	wsc := &WSConn{
		conn:         conn,
		wd:           wd,
		wdOwned:      wdOwned,
		ctx:          origCtx,
		url:          fullURL,
		writeTimeout: cfg.WriteTimeout,
		done:         make(chan struct{}),
	}

	// 启动 context 监听：当 context（用户取消或看门狗超时）被取消时
	// 自动关闭连接，使阻塞中的 ReadMessage 立即返回。
	// 使用 dialCtx（如果存在看门狗，则是看门狗的 context），
	// 这样看门狗超时和用户取消都能触发连接关闭。
	go func() {
		select {
		case <-dialCtx.Done():
			if !wsc.closed.Swap(true) {
				_ = conn.WriteControl(
					WSCloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, ""),
					time.Now().Add(2*time.Second),
				)
				_ = conn.Close()
				if wsc.wdOwned && wsc.wd != nil {
					wsc.wd.Stop(true)
				}
				close(wsc.done)
				log.Logger.Debugw("ws connection closed (context canceled)",
					"url", fullURL)
			}
		case <-wsc.done:
		}
	}()

	log.Logger.Debugw("ws connected",
		"url", fullURL, "elapsed", time.Since(start))

	if cfg.OnConnect != nil {
		cfg.OnConnect(wsc)
	}

	return wsc, nil
}

// ============================================================================
// DoWS — 回调模式
// ============================================================================

// DoWS 建立 WebSocket 连接并持续读取消息，通过回调处理。
// 阻塞直到连接关闭、看门狗超时或 context 取消。
//
// 返回值：
//   - nil: 连接正常关闭（收到 Close 帧）
//   - *WatchdogTimeoutError: 看门狗超时
//   - context.Canceled: 用户主动取消
//   - 其他 error: 连接错误、回调错误等
func (r *Request) DoWS(cfg WSConfig) error {
	conn, err := r.DialWS(cfg)
	if err != nil {
		return err
	}

	// 自动 Ping
	if cfg.PingInterval > 0 {
		go conn.autoPing(cfg.PingInterval, r.ctx)
	}

	messagesReceived := 0
	bytesReceived := 0
	start := time.Now()

	for {
		msgType, data, readErr := conn.conn.ReadMessage()
		if readErr != nil {
			return conn.handleReadError(readErr, cfg, messagesReceived, bytesReceived, start)
		}

		messagesReceived++
		bytesReceived += len(data)

		// Feed 看门狗
		if conn.wd != nil {
			conn.wd.Feed()
		}

		msg := WSMessage{Type: msgType, Data: data}

		// OnMessage
		if cfg.OnMessage != nil {
			if err := cfg.OnMessage(msg); err != nil {
				log.Logger.Debugw("ws interrupted by OnMessage callback",
					"url", conn.url, "err", err)
				_ = conn.Close()
				return err
			}
		}

		// OnText / OnBinary
		if msgType == WSTextMessage && cfg.OnText != nil {
			if err := cfg.OnText(string(data)); err != nil {
				log.Logger.Debugw("ws interrupted by OnText callback",
					"url", conn.url, "err", err)
				_ = conn.Close()
				return err
			}
		} else if msgType == WSBinaryMessage && cfg.OnBinary != nil {
			if err := cfg.OnBinary(data); err != nil {
				log.Logger.Debugw("ws interrupted by OnBinary callback",
					"url", conn.url, "err", err)
				_ = conn.Close()
				return err
			}
		}
	}
}

// ============================================================================
// DoWSMessages — Channel 模式
// ============================================================================

// DoWSMessages 建立 WebSocket 连接，通过 channel 输出消息。
// 同时返回 *WSConn 供调用方写入（线程安全）。
//
// 消息 channel 在连接关闭后关闭。*WSConn 的生命周期由调用方管理
// （channel 关闭后应调用 conn.Close()）。
func (r *Request) DoWSMessages(cfg WSConfig) (<-chan WSMessage, *WSConn, error) {
	conn, err := r.DialWS(cfg)
	if err != nil {
		return nil, nil, err
	}

	ch := make(chan WSMessage, 64)

	// 自动 Ping
	if cfg.PingInterval > 0 {
		go conn.autoPing(cfg.PingInterval, r.ctx)
	}

	go func() {
		defer close(ch)
		defer func() { _ = conn.Close() }()

		start := time.Now()
		messagesReceived := 0
		bytesReceived := 0

		for {
			msgType, data, readErr := conn.conn.ReadMessage()
			if readErr != nil {
				_ = conn.handleReadError(readErr, cfg, messagesReceived, bytesReceived, start)
				return
			}

			messagesReceived++
			bytesReceived += len(data)

			if conn.wd != nil {
				conn.wd.Feed()
			}

			select {
			case ch <- WSMessage{Type: msgType, Data: data}:
			case <-r.ctx.Done():
				return
			}
		}
	}()

	return ch, conn, nil
}

// ============================================================================
// 内部工具
// ============================================================================

// handleReadError 统一处理 WebSocket 读取错误，进行分类并触发回调。
func (c *WSConn) handleReadError(
	readErr error,
	cfg WSConfig,
	messagesReceived, bytesReceived int,
	start time.Time,
) error {
	// 1. 看门狗超时（context-watcher 会设 closed=true，所以必须先检查）
	if c.wd != nil && c.wd.TimedOut() {
		log.Logger.Warnw("ws ended (watchdog timeout)",
			"url", c.url, "messages", messagesReceived,
			"bytes", bytesReceived, "elapsed", time.Since(start))
		err := &WatchdogTimeoutError{
			URL:           c.url,
			ItemsReceived: messagesReceived,
			BytesReceived: bytesReceived,
			Elapsed:       time.Since(start),
			WatchdogName:  c.wd.Name(),
		}
		if cfg.OnError != nil {
			cfg.OnError(err)
		}
		return err
	}

	// 2. 用户主动取消（context-watcher 也会设 closed=true，必须先检查）
	if c.ctx != nil && c.ctx.Err() != nil {
		log.Logger.Debugw("ws ended (user context canceled)",
			"url", c.url, "messages", messagesReceived)
		return context.Canceled
	}

	// 3. 客户端主动关闭（closed 标志已设置，但不是看门狗超时或用户取消）
	if c.closed.Load() {
		log.Logger.Debugw("ws closed (client initiated)",
			"url", c.url, "messages", messagesReceived,
			"elapsed", time.Since(start))
		if cfg.OnClose != nil {
			cfg.OnClose(0, "")
		}
		return nil
	}

	// 4. 正常关闭（收到 Close 帧）
	if websocket.IsCloseError(readErr,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	) {
		var ce *websocket.CloseError
		code, text := 0, ""
		if errors.As(readErr, &ce) {
			code, text = ce.Code, ce.Text
		}
		log.Logger.Debugw("ws closed normally",
			"url", c.url, "code", code, "messages", messagesReceived,
			"elapsed", time.Since(start))
		if cfg.OnClose != nil {
			cfg.OnClose(code, text)
		}
		return nil
	}

	// 其他读取错误
	log.Logger.Warnw("ws read error",
		"url", c.url, "err", readErr, "messages", messagesReceived)
	if cfg.OnError != nil {
		cfg.OnError(readErr)
	}
	return errs.Wrap(readErr, "ws read error")
}

// buildWSURL 从 Request 构建 WebSocket URL（http→ws、https→wss）。
func (r *Request) buildWSURL() string {
	fullURL := r.url
	if len(r.query) > 0 {
		fullURL += "?" + r.query.Encode()
	}

	lower := strings.ToLower(fullURL)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return "wss://" + fullURL[8:]
	case strings.HasPrefix(lower, "http://"):
		return "ws://" + fullURL[7:]
	case strings.HasPrefix(lower, "wss://"), strings.HasPrefix(lower, "ws://"):
		return fullURL // 已经是 WebSocket URL
	default:
		return fullURL
	}
}

// FormatWSCloseMessage 格式化 WebSocket 关闭消息（便捷函数）。
func FormatWSCloseMessage(code int, text string) []byte {
	return websocket.FormatCloseMessage(code, text)
}

// IsWSCloseError 判断错误是否为 WebSocket 关闭错误。
func IsWSCloseError(err error, codes ...int) bool {
	return websocket.IsCloseError(err, codes...)
}

// IsWSUnexpectedCloseError 判断错误是否为意外的 WebSocket 关闭。
func IsWSUnexpectedCloseError(err error) bool {
	return websocket.IsUnexpectedCloseError(err)
}

// WSCloseCode 返回错误中的 WebSocket 关闭码（如果不是关闭错误则返回 -1）。
func WSCloseCode(err error) int {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}
