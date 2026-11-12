package bot

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/mcp"
	utilhttp "github.com/kasuganosora/thinkbot/util/http"
)

// 浏览器 MCP 的连接状态。启动只进入 pending，握手在后台进行。
const (
	BrowserMCPStatePending    = "pending"
	BrowserMCPStateConnecting = "connecting"
	BrowserMCPStateConnected  = "connected"
	BrowserMCPStateFailed     = "failed"
)

// BrowserMCPStatus 是浏览器 MCP 后台连接的可观测快照。LastError 已脱敏。
type BrowserMCPStatus struct {
	State     string
	LastError string
	Attempts  int
	NextRetry time.Time
	Tools     []string
}

const (
	browserBackoffBase = time.Second
	browserBackoffCap  = 30 * time.Second
	browserWatchEvery  = 2 * time.Second
	browserServerName  = "browser"
)

// errRestart 表示本次握手被一次显式重启打断，循环应立刻再连，而不是按失败退避。
var errRestart = errors.New("browser mcp restart")

type watchReason int

const (
	watchStop watchReason = iota
	watchLost
	watchRestart
)

// browserMCPConfig 是后台连接循环的输入。Prepare 在每次尝试前运行（拉起容器、同步脚本、投递 cookie），
// 返回要连接的服务器配置；测试可以不提供 Prepare，改为事先 AddServer。
type browserMCPConfig struct {
	Manager       *mcp.Manager
	Tools         *tools.ToolManager
	Logger        *zap.SugaredLogger
	Prepare       func(ctx context.Context) (mcp.ServerConfig, error)
	BackoffBase   time.Duration
	BackoffCap    time.Duration
	WatchInterval time.Duration
	Redact        []string
	ServerName    string
}

// browserMCPSession 在单个 goroutine 里连接浏览器 MCP，失败后按封顶指数退避重试。
// 同一时刻只有一次握手在进行。
type browserMCPSession struct {
	manager *mcp.Manager
	tools   *mcp.ToolSurface
	logger  *zap.SugaredLogger
	prepare func(context.Context) (mcp.ServerConfig, error)
	base    time.Duration
	limit   time.Duration
	watch   time.Duration
	redact  []string
	name    string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once

	mu            sync.Mutex
	status        BrowserMCPStatus
	attemptCancel context.CancelFunc
	rng           *rand.Rand

	flight  sync.Mutex
	restart atomic.Bool
	wakeCh  chan struct{}
}

func startBrowserMCP(cfg browserMCPConfig) *browserMCPSession {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	base := cfg.BackoffBase
	if base <= 0 {
		base = browserBackoffBase
	}
	limit := cfg.BackoffCap
	if limit <= 0 {
		limit = browserBackoffCap
	}
	if limit < base {
		limit = base
	}
	watch := cfg.WatchInterval
	if watch <= 0 {
		watch = browserWatchEvery
	}
	name := cfg.ServerName
	if name == "" {
		name = browserServerName
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &browserMCPSession{
		manager: cfg.Manager,
		tools:   mcp.NewToolSurface(cfg.Tools),
		logger:  logger,
		prepare: cfg.Prepare,
		base:    base,
		limit:   limit,
		watch:   watch,
		redact:  append([]string(nil), cfg.Redact...),
		name:    name,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		rng:     rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x6d63)),
		wakeCh:  make(chan struct{}, 1),
		status:  BrowserMCPStatus{State: BrowserMCPStatePending},
	}
	s.startOnce.Do(func() {
		s.logger.Infow("browser mcp scheduled", "state", BrowserMCPStatePending, "server", s.name)
		go s.loop()
	})
	return s
}

func (s *browserMCPSession) loop() {
	defer close(s.done)
	attempt := 0
	for {
		if s.stopped() {
			return
		}
		attempt++
		s.setConnecting(attempt)
		if s.prepare != nil {
			cfg, err := s.prepare(s.ctx)
			if s.stopped() {
				return
			}
			if err != nil {
				if !s.backoff(attempt, err) {
					return
				}
				continue
			}
			if cfg.Name == "" {
				cfg.Name = s.name
			}
			s.manager.AddServer(cfg)
		}
		err := s.connectOnce(s.ctx)
		if s.stopped() {
			return
		}
		if errors.Is(err, errRestart) {
			attempt--
			continue
		}
		if err != nil {
			if !s.backoff(attempt, err) {
				return
			}
			continue
		}
		attempt = 0
		s.setConnected()
		switch s.watchUntil() {
		case watchStop:
			return
		case watchRestart:
			continue
		case watchLost:
			s.drop("browser mcp connection lost")
			attempt++
			if !s.backoff(attempt, errors.New("browser mcp connection lost")) {
				return
			}
		}
	}
}

// backoff 记录失败状态并等待。ctx 取消或被重启唤醒时立即返回。
// 返回 false 表示会话已停止。
func (s *browserMCPSession) backoff(attempt int, err error) bool {
	d := s.delay(attempt)
	s.setFailed(attempt, err, time.Now().Add(d))
	return s.sleep(d)
}

func (s *browserMCPSession) delay(attempt int) time.Duration {
	return browserBackoff(attempt, s.base, s.limit, s.rng.Float64())
}

func (s *browserMCPSession) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return false
	case <-s.wakeCh:
		return !s.stopped()
	case <-timer.C:
		return !s.stopped()
	}
}

func (s *browserMCPSession) watchUntil() watchReason {
	tick := time.NewTicker(s.watch)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return watchStop
		case <-s.wakeCh:
			if s.restart.Load() {
				return watchRestart
			}
		case <-tick.C:
			if !s.healthy() {
				return watchLost
			}
		}
	}
}

func (s *browserMCPSession) healthy() bool {
	if s.manager == nil {
		return false
	}
	c, ok := s.manager.GetClient(s.name)
	return ok && c != nil && c.IsHealthy()
}

func (s *browserMCPSession) drop(reason string) {
	if s.tools != nil {
		s.tools.Clear()
	}
	if s.manager != nil {
		s.manager.Disconnect(s.name)
	}
	s.mu.Lock()
	s.status.Tools = nil
	s.mu.Unlock()
	s.logger.Warnw("browser mcp disconnected", "state", BrowserMCPStateFailed, "err", sanitizeBrowserErr(errors.New(reason), s.redact))
}

// connectOnce 进行一次握手并在成功后注册工具。并发调用共用这一把锁，后来的调用
// 如果发现已经连上就直接返回，不会再起一个子进程。
func (s *browserMCPSession) connectOnce(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.flight.Lock()
	defer s.flight.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.restart.Swap(false) {
		s.tools.Clear()
		s.manager.Disconnect(s.name)
	}
	if s.manager.IsServerConnected(s.name) && s.tools.Ready() {
		return nil
	}
	if s.manager.IsServerConnected(s.name) {
		s.manager.Disconnect(s.name)
	}

	actx, cancel := s.newAttemptCtx(s.ctx)
	defer cancel()
	if err := s.manager.EnableServer(actx, s.name); err != nil {
		if s.ctx.Err() != nil {
			return err
		}
		if errors.Is(err, context.Canceled) {
			return errRestart
		}
		return err
	}
	lctx, lcancel := context.WithTimeout(actx, 8*time.Second)
	defer lcancel()
	if err := s.tools.Install(lctx, s.manager); err != nil {
		s.manager.Disconnect(s.name)
		if s.ctx.Err() != nil {
			return err
		}
		if errors.Is(err, context.Canceled) {
			return errRestart
		}
		return err
	}
	return nil
}

func (s *browserMCPSession) newAttemptCtx(parent context.Context) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attemptCancel != nil {
		s.attemptCancel()
	}
	ctx, cancel := context.WithCancel(parent)
	s.attemptCancel = cancel
	return ctx, cancel
}

func (s *browserMCPSession) cancelAttempt() {
	s.mu.Lock()
	cancel := s.attemptCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Restart 丢掉当前连接并等待下一次成功注册。打开桌面、把浏览器挪到共享屏幕时用。
func (s *browserMCPSession) Restart(ctx context.Context) error {
	if s == nil {
		return errors.New("browser mcp is not running")
	}
	before := s.installCount()
	s.restart.Store(true)
	s.cancelAttempt()
	s.wake()
	for {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if s.installCount() > before && s.Status().State == BrowserMCPStateConnected && !s.restart.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
	}
}

// Stop 取消后台循环。已连上的客户端留给调用方做优雅关闭，不在这里拆掉。
func (s *browserMCPSession) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.cancel()
		s.cancelAttempt()
		s.wake()
		<-s.done
	})
}

func (s *browserMCPSession) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *browserMCPSession) stopped() bool {
	return s.ctx.Err() != nil
}

func (s *browserMCPSession) Status() BrowserMCPStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	if len(st.Tools) > 0 {
		st.Tools = append([]string(nil), st.Tools...)
	}
	return st
}

func (s *browserMCPSession) installCount() int {
	if s == nil || s.tools == nil {
		return 0
	}
	return s.tools.InstallCount()
}

func (s *browserMCPSession) setConnecting(attempt int) {
	s.mu.Lock()
	s.status.State = BrowserMCPStateConnecting
	s.status.Attempts = attempt
	s.status.NextRetry = time.Time{}
	s.mu.Unlock()
	s.logger.Infow("browser mcp connecting", "state", BrowserMCPStateConnecting, "attempt", attempt)
}

func (s *browserMCPSession) setConnected() {
	names := s.tools.Names()
	s.mu.Lock()
	s.status.State = BrowserMCPStateConnected
	s.status.LastError = ""
	s.status.NextRetry = time.Time{}
	s.status.Tools = names
	attempt := s.status.Attempts
	s.mu.Unlock()
	s.logger.Infow("browser mcp connected",
		"state", BrowserMCPStateConnected, "attempt", attempt, "tools", len(names))
}

func (s *browserMCPSession) setFailed(attempt int, err error, next time.Time) {
	msg := sanitizeBrowserErr(err, s.redact)
	s.mu.Lock()
	s.status.State = BrowserMCPStateFailed
	s.status.Attempts = attempt
	s.status.LastError = msg
	s.status.NextRetry = next
	s.status.Tools = nil
	s.mu.Unlock()
	s.logger.Warnw("browser mcp connect failed",
		"state", BrowserMCPStateFailed,
		"attempt", attempt,
		"err", msg,
		"next_retry", next.UTC().Format(time.RFC3339))
}

// browserBackoff 计算第 attempt 次失败后的等待（从 1 计）。
// 等待落在 [d/2, d]，d = min(cap, base*2^(attempt-1))。frac 取 0 得下界，取 1 得上界。
func browserBackoff(attempt int, base, cap time.Duration, frac float64) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	if cap < base {
		cap = base
	}
	d := base
	for i := 1; i < attempt && d < cap; i++ {
		if d > cap/2 {
			d = cap
			break
		}
		d *= 2
	}
	if d > cap {
		d = cap
	}
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	floor := d / 2
	span := d - floor
	return floor + time.Duration(float64(span)*frac)
}

// sanitizeBrowserErr 去掉错误文本里的凭据，供状态和日志使用。
func sanitizeBrowserErr(err error, redact []string) string {
	if err == nil {
		return ""
	}
	msg := utilhttp.SanitizeError(err).Error()
	return sanitizeBrowserText(msg, redact)
}

func sanitizeBrowserText(msg string, redact []string) string {
	secrets := append([]string(nil), redact...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "***")
	}
	const max = 500
	if len(msg) > max {
		msg = msg[:max]
	}
	return msg
}

// browserRedactList 收集不能写进日志的代理串。完整代理 URL 一律打码；
// URL 里的密码单独再列一次，避免 stderr 只打出密码时漏掉。
func browserRedactList(proxy string) []string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return nil
	}
	out := []string{proxy}
	u, err := url.Parse(proxy)
	if err != nil || u.User == nil {
		return out
	}
	if p, ok := u.User.Password(); ok && len(p) >= 4 {
		out = append(out, p)
	}
	return out
}
