package wecom

// 长连接的帧收发层。
//
// 职责边界：只管「一条 WebSocket 连接上的帧怎么发、怎么收、怎么把响应对回请求」，
// 不理解业务（消息归一、会话路由都在 channel.go）。
//
// 三个实测出来的坑直接决定了这里的形状（详见 docs/wecom-channel-design.md §9）：
//   1. ACK 帧**不带 cmd**，只能靠 headers.req_id 关联 → 必须有 pending 表
//   2. 错误 ACK **连 req_id 都不带** → pending 表必须有超时兜底 + 孤儿归因
//   3. 一个 bot 只允许一条连接，新连接会把旧的踢掉 → 收到 disconnected_event 必须停止重连

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

const (
	// defaultEndpoint 企业微信智能机器人的官方长连接地址。
	defaultEndpoint = "wss://openws.work.weixin.qq.com"

	// subscribeTimeout 等待 aibot_subscribe 响应的超时。
	// 订阅是建立连接后必须完成的第一步，超时即判定连接不可用、整体重连。
	subscribeTimeout = 15 * time.Second

	// callTimeout 普通请求（发消息等）等待响应的超时。
	//
	// ⚠️ 这个超时**不是**为了防网络慢：实测错误 ACK（如 40058 markdown 超长）
	// 根本不带 req_id，无法关联到具体请求，只能靠超时兜底把调用方放出来。
	// 调小会让偶发慢响应被误判成失败（且可能重复发送），调大则卡住出站。
	callTimeout = 15 * time.Second
)

// endpoint 返回生效的长连接地址。
func (c Config) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return defaultEndpoint
}

// pendingReq 是一个在途请求。
type pendingReq struct {
	seq uint64
	ch  chan ackResult
}

// wsSession 是一次长连接的生命周期对象。
//
// 每次重连都会新建一个 —— 旧 session 的在途请求会被统一以「连接已断开」失败掉，
// 不会静默挂到 callTimeout 才超时。
type wsSession struct {
	cfg  Config
	conn *http.WSConn

	// closed 在分发循环结束（连接断开）时关闭，用于唤醒所有等待者。
	closed chan struct{}
	// closeOnce 保证 closed 只被关一次。
	closeOnce sync.Once

	mu      sync.Mutex
	pending map[string]*pendingReq
	seq     uint64

	onMsg   func(context.Context, *callbackBody)
	onEvent func(context.Context, string)
}

// newWSSession 创建一个会话对象。
func newWSSession(cfg Config, conn *http.WSConn, onMsg func(context.Context, *callbackBody), onEvent func(context.Context, string)) *wsSession {
	return &wsSession{
		cfg:     cfg,
		conn:    conn,
		closed:  make(chan struct{}),
		pending: make(map[string]*pendingReq),
		onMsg:   onMsg,
		onEvent: onEvent,
	}
}

// addPending 登记一个在途请求，返回其 req_id 与等待通道。
func (s *wsSession) addPending() (string, *pendingReq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("tb-%d-%d", time.Now().UnixNano(), s.seq)
	p := &pendingReq{seq: s.seq, ch: make(chan ackResult, 1)}
	s.pending[id] = p
	return id, p
}

// removePending 摘除一个在途请求。
func (s *wsSession) removePending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// resolve 把响应投递给指定 req_id 的在途请求。
func (s *wsSession) resolve(id string, res ackResult) {
	s.mu.Lock()
	p := s.pending[id]
	s.mu.Unlock()
	if p == nil {
		return // 已超时/已被摘除，丢弃
	}
	select {
	case p.ch <- res:
	default:
	}
}

// deliverOrphan 把「没有 req_id 的错误响应」归因到最老的一个在途请求。
//
// ⚠️ 这是**启发式**，不是协议保证：实测错误 ACK 不带 req_id，我们无从判断它属于
// 哪个请求。企微的请求-响应在单条连接上基本是串行的（发消息 30 条/分钟限流），
// 取 seq 最小的那个在绝大多数情况下就是它。
// 万一猜错，代价只是一次「发消息失败」的误报（本就是失败），不会造成重复发送。
func (s *wsSession) deliverOrphan(res ackResult) {
	s.mu.Lock()
	var oldest *pendingReq
	for _, p := range s.pending {
		if oldest == nil || p.seq < oldest.seq {
			oldest = p
		}
	}
	s.mu.Unlock()
	if oldest == nil {
		return
	}
	select {
	case oldest.ch <- res:
	default:
	}
}

// failAll 让所有在途请求立即失败（连接断开时调用）。
func (s *wsSession) failAll(err error) {
	s.mu.Lock()
	all := make([]*pendingReq, 0, len(s.pending))
	for _, p := range s.pending {
		all = append(all, p)
	}
	s.pending = make(map[string]*pendingReq)
	s.mu.Unlock()
	for _, p := range all {
		select {
		case p.ch <- ackResult{ErrCode: -1, ErrMsg: err.Error()}:
		default:
		}
	}
}

// writeFrame 发送一帧。body 为 nil 时省略 body 字段（如 ping）。
func (s *wsSession) writeFrame(cmd, reqID string, body any) error {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errs.Wrapf(err, "wecom: marshal body for %s", cmd)
		}
		raw = b
	}
	return s.conn.WriteJSON(frame{
		Cmd:     cmd,
		Headers: frameHeaders{ReqID: reqID},
		Body:    raw,
	})
}

// dispatch 处理一帧，返回被它 resolve 的 req_id（非 ACK 帧返回空串）。
func (s *wsSession) dispatch(ctx context.Context, data []byte) string {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		traceid.L(ctx).Debugw("wecom: failed to parse frame",
			"raw", strutil.Truncate(string(data), 300), "err", err)
		return ""
	}

	// ⚠️ ACK 帧没有 cmd —— 这是判断「响应 vs 推送」的唯一依据，不能反过来按 cmd 匹配。
	if f.Cmd == "" {
		res := ackResult{ErrCode: f.ErrCode, ErrMsg: f.ErrMsg}
		if f.Headers.ReqID == "" {
			if res.OK() {
				return "" // 空成功包（如 feedback_event 的回包），无意义
			}
			traceid.L(ctx).Warnw("wecom: error ack without req_id",
				"errcode", res.ErrCode, "errmsg", res.ErrMsg)
			s.deliverOrphan(res)
			return ""
		}
		if !res.OK() {
			traceid.L(ctx).Warnw("wecom: request rejected",
				"req_id", f.Headers.ReqID, "errcode", res.ErrCode, "errmsg", res.ErrMsg)
		}
		s.resolve(f.Headers.ReqID, res)
		return f.Headers.ReqID
	}

	switch f.Cmd {
	case cmdMsgCallback:
		var body callbackBody
		if err := json.Unmarshal(f.Body, &body); err != nil {
			traceid.L(ctx).Debugw("wecom: failed to parse msg callback", "err", err)
			return ""
		}
		if s.onMsg != nil {
			s.onMsg(ctx, &body)
		}
	case cmdEventCallback:
		var body eventBody
		if err := json.Unmarshal(f.Body, &body); err != nil {
			traceid.L(ctx).Debugw("wecom: failed to parse event callback", "err", err)
			return ""
		}
		et := ""
		if body.Event != nil {
			et = body.Event.EventType
		}
		if s.onEvent != nil {
			s.onEvent(ctx, et)
		}
	default:
		traceid.L(ctx).Debugw("wecom: unknown cmd", "cmd", f.Cmd)
	}
	return ""
}

// serve 是分发循环，阻塞到连接断开或 ctx 取消。
func (s *wsSession) serve(ctx context.Context, ch <-chan http.WSMessage) error {
	defer s.closeOnce.Do(func() { close(s.closed) })
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return errors.New("wecom: ws message channel closed")
			}
			s.dispatch(ctx, msg.Data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// run 接管一条连接：先起分发循环，再做订阅握手，最后阻塞到连接结束。
func (s *wsSession) run(ctx context.Context, ch <-chan http.WSMessage) error {
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, ch) }()

	if err := s.subscribe(ctx); err != nil {
		_ = s.conn.Close()
		s.failAll(err)
		<-done
		return err
	}
	return <-done
}

// subscribe 发送 aibot_subscribe 并等待响应。
//
// 必须在分发循环跑起来之后调用 —— 握手期间若有别的帧进来也照常被分发，
// 不会因为没人读 ch 而把读 goroutine 卡死。
func (s *wsSession) subscribe(ctx context.Context) error {
	id, p := s.addPending()
	defer s.removePending(id)

	if err := s.writeFrame(cmdSubscribe, id, subscribeBody{
		BotID:  s.cfg.RobotID,
		Secret: s.cfg.Secret,
	}); err != nil {
		return errs.Wrap(err, "wecom: send subscribe frame")
	}

	timer := time.NewTimer(subscribeTimeout)
	defer timer.Stop()

	select {
	case res := <-p.ch:
		if !res.OK() {
			return errs.Newf("wecom: subscribe rejected: errcode=%d errmsg=%s", res.ErrCode, res.ErrMsg)
		}
		return nil
	case <-s.closed:
		return errors.New("wecom: connection closed during subscribe")
	case <-timer.C:
		return errs.Newf("wecom: subscribe timeout after %s (no ack)", subscribeTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// call 发送一个请求命令并等待响应。
//
// 用于 aibot_send_msg / ping 等。超时或连接断开都返回 error。
func (s *wsSession) call(ctx context.Context, cmd string, body any) (ackResult, error) {
	id, p := s.addPending()
	defer s.removePending(id)

	if err := s.writeFrame(cmd, id, body); err != nil {
		return ackResult{}, errs.Wrapf(err, "wecom: send %s", cmd)
	}

	timer := time.NewTimer(callTimeout)
	defer timer.Stop()

	select {
	case res := <-p.ch:
		if res.ErrCode == -1 {
			// failAll 投出来的伪响应，代表连接已断。
			return res, errors.New(res.ErrMsg)
		}
		return res, nil
	case <-s.closed:
		return ackResult{}, errors.New("wecom: connection closed while awaiting response")
	case <-timer.C:
		return ackResult{}, errs.Newf("wecom: %s timeout after %s (no ack)", cmd, callTimeout)
	case <-ctx.Done():
		return ackResult{}, ctx.Err()
	}
}

// sendMarkdown 向指定会话发送一条 markdown 消息。
func (s *wsSession) sendMarkdown(ctx context.Context, chatID string, chatType int, content string) error {
	res, err := s.call(ctx, cmdSendMsg, sendMsgBody{
		ChatID:   chatID,
		ChatType: chatType,
		MsgType:  MsgTypeMarkdown,
		Markdown: &markdownBody{Content: content},
	})
	if err != nil {
		return err
	}
	if !res.OK() {
		return errs.Newf("wecom: send msg failed: errcode=%d errmsg=%s", res.ErrCode, res.ErrMsg)
	}
	return nil
}
