package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kasuganosora/thinkbot/util/watchdog"
)

// ============================================================================
// 辅助
// ============================================================================

// wsUpgrader 测试用的 WebSocket upgrader。
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// newWSTestServer 创建一个 WebSocket 测试服务器。
// handler 在新 goroutine 中处理每个连接。
func newWSTestServer(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		handler(conn)
	}))
	return srv
}

// ============================================================================
// Echo 测试
// ============================================================================

func TestWSEcho(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(msgType, data); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	var messages []string
	c := New()

	err := c.Get(srv.URL).DoWS(WSConfig{
		OnConnect: func(conn *WSConn) {
			// 连接建立后发送几条消息
			go func() {
				for i := 0; i < 3; i++ {
					_ = conn.WriteText(fmt.Sprintf("hello-%d", i))
					time.Sleep(10 * time.Millisecond)
				}
				// 最后一条后关闭连接
				time.Sleep(50 * time.Millisecond)
				_ = conn.Close()
			}()
		},
		OnText: func(text string) error {
			messages = append(messages, text)
			return nil
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %v", len(messages), messages)
	}
	for i, m := range messages {
		expected := fmt.Sprintf("hello-%d", i)
		if m != expected {
			t.Errorf("expected %s, got %s", expected, m)
		}
	}
}

// ============================================================================
// DialWS + 手动读写测试
// ============================================================================

func TestWSDialAndReadWrite(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		// Echo
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.WriteMessage(msgType, data)
		}
	})
	defer srv.Close()

	c := New()
	conn, err := c.Get(srv.URL).DialWS(WSConfig{})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 写文本
	if err := conn.WriteText("ping"); err != nil {
		t.Fatalf("write text failed: %v", err)
	}

	// 读
	_ = conn.Underlying().SetReadDeadline(time.Now().Add(2 * time.Second))
	msgType, data, err := conn.Underlying().ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if msgType != WSTextMessage {
		t.Errorf("expected text message, got type %d", msgType)
	}
	if string(data) != "ping" {
		t.Errorf("expected 'ping', got %q", string(data))
	}

	// 写 JSON
	if err := conn.WriteJSON(map[string]string{"key": "value"}); err != nil {
		t.Fatalf("write json failed: %v", err)
	}

	_ = conn.Underlying().SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err = conn.Underlying().ReadMessage()
	if err != nil {
		t.Fatalf("read json failed: %v", err)
	}
	if !strings.Contains(string(data), "value") {
		t.Errorf("expected 'value' in response, got %q", string(data))
	}
}

// ============================================================================
// Binary 消息测试
// ============================================================================

func TestWSBinary(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.WriteMessage(msgType, data)
		}
	})
	defer srv.Close()

	var binaryReceived []byte
	c := New()

	err := c.Get(srv.URL).DoWS(WSConfig{
		OnConnect: func(conn *WSConn) {
			go func() {
				_ = conn.WriteBinary([]byte{0x01, 0x02, 0xFF})
				time.Sleep(50 * time.Millisecond)
				_ = conn.Close()
			}()
		},
		OnBinary: func(data []byte) error {
			binaryReceived = make([]byte, len(data))
			copy(binaryReceived, data)
			return nil
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(binaryReceived) != 3 {
		t.Fatalf("expected 3 bytes, got %d", len(binaryReceived))
	}
	if binaryReceived[0] != 0x01 || binaryReceived[1] != 0x02 || binaryReceived[2] != 0xFF {
		t.Errorf("unexpected binary data: %v", binaryReceived)
	}
}

// ============================================================================
// OnMessage 通用回调测试
// ============================================================================

func TestWSOnMessage(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		_ = conn.WriteMessage(WSTextMessage, []byte("text-msg"))
		_ = conn.WriteMessage(WSBinaryMessage, []byte("bin-msg"))
		time.Sleep(50 * time.Millisecond)
		_ = conn.WriteMessage(WSCloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	defer srv.Close()

	var allMessages []WSMessage
	c := New()

	err := c.Get(srv.URL).DoWS(WSConfig{
		OnMessage: func(msg WSMessage) error {
			data := make([]byte, len(msg.Data))
			copy(data, msg.Data)
			allMessages = append(allMessages, WSMessage{Type: msg.Type, Data: data})
			return nil
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 第一条文本 + 第二条二进制，Close 帧不会触发 OnMessage
	if len(allMessages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(allMessages))
	}
	if !allMessages[0].IsText() || allMessages[0].Text() != "text-msg" {
		t.Errorf("unexpected msg[0]: %+v", allMessages[0])
	}
	if !allMessages[1].IsBinary() {
		t.Errorf("unexpected msg[1]: not binary")
	}
}

// ============================================================================
// Channel 模式测试
// ============================================================================

func TestWSChannel(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for i := 0; i < 5; i++ {
			_ = conn.WriteMessage(WSTextMessage, []byte(fmt.Sprintf("msg-%d", i)))
		}
		time.Sleep(50 * time.Millisecond)
		_ = conn.WriteMessage(WSCloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	defer srv.Close()

	c := New()
	ch, conn, err := c.Get(srv.URL).DoWSMessages(WSConfig{})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	var messages []string
	for msg := range ch {
		messages = append(messages, msg.Text())
	}
	_ = conn.Close()

	if len(messages) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(messages))
	}
	if messages[0] != "msg-0" {
		t.Errorf("expected msg-0, got %s", messages[0])
	}
	if messages[4] != "msg-4" {
		t.Errorf("expected msg-4, got %s", messages[4])
	}
}

// ============================================================================
// Channel + 写入测试（双向通信）
// ============================================================================

func TestWSChannelBidirectional(t *testing.T) {
	var receivedByServer int32

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for i := 0; i < 3; i++ {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			atomic.AddInt32(&receivedByServer, 1)
			// Echo back
			_ = conn.WriteMessage(WSTextMessage, []byte("ack"))
		}
		// 发完后等一下再关闭
		time.Sleep(50 * time.Millisecond)
		_ = conn.WriteMessage(WSCloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	defer srv.Close()

	c := New()
	ch, conn, err := c.Get(srv.URL).DoWSMessages(WSConfig{})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	// 写入几条消息
	for i := 0; i < 3; i++ {
		_ = conn.WriteText(fmt.Sprintf("client-%d", i))
	}

	var echoes int
	for range ch {
		echoes++
	}
	_ = conn.Close()

	if echoes != 3 {
		t.Errorf("expected 3 echo messages, got %d", echoes)
	}
	if atomic.LoadInt32(&receivedByServer) != 3 {
		t.Errorf("expected server received 3 messages, got %d", atomic.LoadInt32(&receivedByServer))
	}
}

// ============================================================================
// 看门狗超时测试
// ============================================================================

func TestWSWatchdogTimeout(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		// 发一条消息后永久卡住
		_ = conn.WriteMessage(WSTextMessage, []byte("first"))
		// 等待客户端断开
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	var messages []string
	c := New()

	start := time.Now()
	err := c.Get(srv.URL).DoWS(WSConfig{
		WatchdogTimeout: 200 * time.Millisecond,
		OnText: func(text string) error {
			messages = append(messages, text)
			return nil
		},
	})
	elapsed := time.Since(start)

	// 应该收到第一条消息
	if len(messages) < 1 {
		t.Errorf("expected at least 1 message, got %d", len(messages))
	}

	// 应该返回 WatchdogTimeoutError
	if err == nil {
		t.Fatal("expected WatchdogTimeoutError, got nil")
	}
	if !IsWatchdogTimeout(err) {
		t.Fatalf("expected WatchdogTimeoutError, got %T: %v", err, err)
	}

	wdErr, ok := err.(*WatchdogTimeoutError)
	if !ok {
		t.Fatalf("expected *WatchdogTimeoutError, got %T", err)
	}
	if wdErr.ItemsReceived != 1 {
		t.Errorf("expected 1 item received, got %d", wdErr.ItemsReceived)
	}

	if elapsed > 2*time.Second {
		t.Errorf("watchdog should disconnect within ~200ms, took %v", elapsed)
	}
	t.Logf("WS watchdog: disconnected after %v, items=%d, bytes=%d",
		elapsed, wdErr.ItemsReceived, wdErr.BytesReceived)
}

// ============================================================================
// 用户主动取消测试
// ============================================================================

func TestWSUserCancel(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := c.Get(srv.URL).SetContext(ctx).DoWS(WSConfig{
		WatchdogTimeout: 10 * time.Second, // 长看门狗
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error")
	}
	if IsWatchdogTimeout(err) {
		t.Error("should NOT be WatchdogTimeoutError for user cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %T: %v", err, err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("should cancel within ~100ms, took %v", elapsed)
	}
}

// ============================================================================
// 服务器关闭连接测试
// ============================================================================

func TestWSServerClose(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		_ = conn.WriteMessage(WSTextMessage, []byte("hello"))
		time.Sleep(20 * time.Millisecond)
		_ = conn.WriteMessage(WSCloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	})
	defer srv.Close()

	var closeCalled bool
	var closeCode int
	var closeText string

	c := New()
	err := c.Get(srv.URL).DoWS(WSConfig{
		OnClose: func(code int, text string) {
			closeCalled = true
			closeCode = code
			closeText = text
		},
		OnText: func(text string) error {
			if text != "hello" {
				t.Errorf("expected 'hello', got %q", text)
			}
			return nil
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !closeCalled {
		t.Error("expected OnClose to be called")
	}
	if closeCode != websocket.CloseNormalClosure {
		t.Errorf("expected close code %d, got %d", websocket.CloseNormalClosure, closeCode)
	}
	if closeText != "bye" {
		t.Errorf("expected close text 'bye', got %q", closeText)
	}
}

// ============================================================================
// 回调中断测试
// ============================================================================

func TestWSCallbackInterrupt(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for i := 0; i < 10; i++ {
			_ = conn.WriteMessage(WSTextMessage, []byte(fmt.Sprintf("msg-%d", i)))
			time.Sleep(10 * time.Millisecond)
		}
	})
	defer srv.Close()

	var count int
	myErr := errors.New("stop reading")
	c := New()

	err := c.Get(srv.URL).DoWS(WSConfig{
		OnText: func(text string) error {
			count++
			if count == 3 {
				return myErr // 中断
			}
			return nil
		},
	})

	if err == nil {
		t.Fatal("expected error from callback")
	}
	if !errors.Is(err, myErr) {
		t.Errorf("expected callback error, got %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 messages before interrupt, got %d", count)
	}
}

// ============================================================================
// 连接失败测试
// ============================================================================

func TestWSConnectFail(t *testing.T) {
	// 用一个普通 HTTP 服务器（不是 WebSocket）来模拟握手失败
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New()
	_, err := c.Get(srv.URL).DialWS(WSConfig{})

	if err == nil {
		t.Fatal("expected error on connect fail")
	}
}

// ============================================================================
// 自动 Ping 测试
// ============================================================================

func TestWSAutoPing(t *testing.T) {
	var pingCount int32

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		// 回显 Pong
		conn.SetPingHandler(func(appData string) error {
			atomic.AddInt32(&pingCount, 1)
			return conn.WriteMessage(WSPongMessage, []byte(appData))
		})
		// 保持连接打开
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New()

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	_ = c.Get(srv.URL).SetContext(ctx).DoWS(WSConfig{
		PingInterval: 50 * time.Millisecond,
	})

	if atomic.LoadInt32(&pingCount) < 2 {
		t.Errorf("expected at least 2 pings, got %d", atomic.LoadInt32(&pingCount))
	}
}

// ============================================================================
// URL 转换测试
// ============================================================================

func TestBuildWSURL(t *testing.T) {
	r := &Request{url: "http://example.com/ws"}
	if u := r.buildWSURL(); u != "ws://example.com/ws" {
		t.Errorf("expected ws://example.com/ws, got %s", u)
	}

	r = &Request{url: "https://example.com/ws"}
	if u := r.buildWSURL(); u != "wss://example.com/ws" {
		t.Errorf("expected wss://example.com/ws, got %s", u)
	}

	r = &Request{url: "ws://example.com/ws"}
	if u := r.buildWSURL(); u != "ws://example.com/ws" {
		t.Errorf("expected ws://example.com/ws, got %s", u)
	}

	r = &Request{url: "wss://example.com/ws"}
	if u := r.buildWSURL(); u != "wss://example.com/ws" {
		t.Errorf("expected wss://example.com/ws, got %s", u)
	}
}

// ============================================================================
// WSMessage 方法测试
// ============================================================================

func TestWSMessageMethods(t *testing.T) {
	msg := WSMessage{Type: WSTextMessage, Data: []byte(`{"key":"val"}`)}
	if !msg.IsText() {
		t.Error("expected IsText=true")
	}
	if msg.IsBinary() {
		t.Error("expected IsBinary=false")
	}
	if msg.Text() != `{"key":"val"}` {
		t.Errorf("unexpected text: %s", msg.Text())
	}

	var m map[string]string
	if err := msg.JSON(&m); err != nil {
		t.Fatalf("json error: %v", err)
	}
	if m["key"] != "val" {
		t.Errorf("expected key=val, got %s", m["key"])
	}

	bin := WSMessage{Type: WSBinaryMessage, Data: []byte{1, 2, 3}}
	if !bin.IsBinary() {
		t.Error("expected IsBinary=true")
	}
	if bin.IsText() {
		t.Error("expected IsText=false")
	}
}

// ============================================================================
// WSCloseCode 测试
// ============================================================================

func TestWSCloseCode(t *testing.T) {
	err := &websocket.CloseError{Code: 1000, Text: "normal"}
	if code := WSCloseCode(err); code != 1000 {
		t.Errorf("expected 1000, got %d", code)
	}

	plainErr := errors.New("not a close error")
	if code := WSCloseCode(plainErr); code != -1 {
		t.Errorf("expected -1, got %d", code)
	}
}

// ============================================================================
// 看门狗 vs 用户取消 区分测试
// ============================================================================

func TestWSWatchdogTimeoutVsUserCancel(t *testing.T) {
	// 场景 1：看门狗超时
	t.Run("watchdog_timeout", func(t *testing.T) {
		srv := newWSTestServer(t, func(conn *websocket.Conn) {
			defer func() { _ = conn.Close() }()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		})
		defer srv.Close()

		c := New()
		err := c.Get(srv.URL).DoWS(WSConfig{
			WatchdogTimeout: 150 * time.Millisecond,
		})

		if err == nil {
			t.Fatal("expected error")
		}
		if !IsWatchdogTimeout(err) {
			t.Errorf("expected WatchdogTimeoutError, got %T: %v", err, err)
		}
	})

	// 场景 2：用户主动取消
	t.Run("user_cancel", func(t *testing.T) {
		srv := newWSTestServer(t, func(conn *websocket.Conn) {
			defer func() { _ = conn.Close() }()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		})
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		c := New()

		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()

		err := c.Get(srv.URL).SetContext(ctx).DoWS(WSConfig{
			WatchdogTimeout: 10 * time.Second,
		})

		if err == nil {
			t.Fatal("expected error")
		}
		if IsWatchdogTimeout(err) {
			t.Error("should NOT be WatchdogTimeoutError for user cancel")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %T: %v", err, err)
		}
	})
}

// ============================================================================
// 头部传递测试
// ============================================================================

func TestWSHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		_ = conn.WriteMessage(WSTextMessage, []byte("auth-ok"))
		_ = conn.WriteMessage(WSCloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = conn.Close()
	}))
	defer srv.Close()

	var received string
	c := New()

	err := c.Get(srv.URL).BearerToken("test123").DoWS(WSConfig{
		OnText: func(text string) error {
			received = text
			return nil
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received != "auth-ok" {
		t.Errorf("expected 'auth-ok', got %q", received)
	}
}

// ============================================================================
// Subprotocol 测试
// ============================================================================

func TestWSSubprotocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin:  func(r *http.Request) bool { return true },
			Subprotocols: []string{"chat", "echo"},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		// 回显协商的子协议
		_ = conn.WriteMessage(WSTextMessage, []byte(conn.Subprotocol()))
		_ = conn.WriteMessage(WSCloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = conn.Close()
	}))
	defer srv.Close()

	var proto string
	c := New()

	err := c.Get(srv.URL).DoWS(WSConfig{
		Subprotocols: []string{"chat", "echo"},
		OnText: func(text string) error {
			proto = text
			return nil
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "chat" {
		t.Errorf("expected subprotocol 'chat', got %q", proto)
	}
}

// ============================================================================
// 外部 Watchdog 测试
// ============================================================================

func TestWSExternalWatchdog(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		_ = conn.WriteMessage(WSTextMessage, []byte("data1"))
		_ = conn.WriteMessage(WSTextMessage, []byte("data2"))
		time.Sleep(50 * time.Millisecond)
		_ = conn.WriteMessage(WSCloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	defer srv.Close()

	wd := watchdog.New(context.Background(), 5*time.Second)
	defer wd.Stop(true)

	var messages []string
	c := New()

	err := c.Get(srv.URL).DoWS(WSConfig{
		Watchdog: wd,
		OnText: func(text string) error {
			messages = append(messages, text)
			return nil
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
}

// ============================================================================
// CloseWithCode 测试
// ============================================================================

func TestWSCloseWithCode(t *testing.T) {
	var serverCloseCode int32

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				if ce, ok := err.(*websocket.CloseError); ok {
					atomic.StoreInt32(&serverCloseCode, int32(ce.Code))
				}
				return
			}
		}
	})
	defer srv.Close()

	c := New()
	conn, err := c.Get(srv.URL).DialWS(WSConfig{})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	_ = conn.CloseWithCode(websocket.CloseTryAgainLater, "retry later")

	// 给服务器时间处理关闭帧
	time.Sleep(100 * time.Millisecond)

	if !conn.IsClosed() {
		t.Error("expected conn to be closed")
	}

	if code := int(atomic.LoadInt32(&serverCloseCode)); code != websocket.CloseTryAgainLater {
		t.Errorf("expected server to see close code %d, got %d", websocket.CloseTryAgainLater, code)
	}

	// 再次 Close 不应 panic
	_ = conn.Close()
}

// ============================================================================
// 并发写测试
// ============================================================================

func TestWSConcurrentWrite(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	c := New()
	conn, err := c.Get(srv.URL).DialWS(WSConfig{})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 并发写不应 panic
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func(n int) {
			_ = conn.WriteText(fmt.Sprintf("concurrent-%d", n))
			done <- struct{}{}
		}(i)
	}

	for i := 0; i < 5; i++ {
		<-done
	}
}
