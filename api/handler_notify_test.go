package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/notify"
)

const notifyTestBot = "bot-2d8f9b087270da0bcfe177a5"

type notifyFakeResolver struct{}

func (notifyFakeResolver) Resolve(_ context.Context, _, _, target string) (notify.Target, error) {
	chat := "76017910"
	if target != "" {
		chat = target
	}
	return notify.Target{ChannelName: "telegram", ChannelType: "telegram", ChatID: chat}, nil
}

type notifyFakeDeliverer struct {
	mu    sync.Mutex
	texts []string
}

func (d *notifyFakeDeliverer) Deliver(_ context.Context, _ string, _ notify.Target, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.texts = append(d.texts, text)
	return nil
}

type notifyEnv struct {
	s      *Server
	engine *gin.Engine
	db     *gorm.DB
	del    *notifyFakeDeliverer
	token  string
	other  string
}

func newNotifyEnv(t *testing.T) *notifyEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.NotifyToken{}, &dao.NotifyEvent{}, &dao.ChatMessage{}, &dao.User{}, &dao.IdentityMapping{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	store := config.NewStore(nil)
	lg := zap.NewNop().Sugar()
	s := &Server{store: store, logger: lg, db: db, botSvc: &BotService{store: store, logger: lg},
		chatHistory: NewChatHistoryService(db, lg)}
	s.initNotify()
	del := &notifyFakeDeliverer{}
	s.notifySvc.Resolver = notifyFakeResolver{}
	s.notifySvc.Deliverer = del
	s.notifySvc.Persona = nil

	engine := gin.New()
	engine.POST("/api/bots/:id/notify", s.handleNotify)

	tok, _, err := s.notifyTokens.Create(context.Background(), notifyTestBot, "maid-hooks")
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := s.notifyTokens.Create(context.Background(), "bot-other", "x")
	return &notifyEnv{s: s, engine: engine, db: db, del: del, token: tok, other: other}
}

func (e *notifyEnv) set(t *testing.T, key, val string) {
	t.Helper()
	if err := e.s.store.Set(context.Background(), key, val); err != nil {
		t.Fatal(err)
	}
}

func (e *notifyEnv) do(token, remote string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	case []byte:
		raw = b
	default:
		raw, _ = json.Marshal(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/bots/"+notifyTestBot+"/notify", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if remote == "" {
		remote = "127.0.0.1:40000"
	}
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

func decodeNotify(t *testing.T, w *httptest.ResponseRecorder) notify.Result {
	t.Helper()
	var r notify.Result
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return r
}

var mdadmBody = map[string]string{
	"source": "maid/mdadm", "level": "critical", "title": "DegradedArray on /dev/md0",
	"body": "mdadm: DegradedArray event detected on md device /dev/md0 (component /dev/sdb1)",
}

func TestNotifyHTTP_Auth(t *testing.T) {
	e := newNotifyEnv(t)
	w := e.do("", "", mdadmBody, nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "thinkbot-notify") {
		t.Fatalf("missing token: %d %s", w.Code, w.Body)
	}
	if w := e.do("tbn_nope_nope", "", mdadmBody, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", w.Code)
	}
	if w := e.do(e.token+"x", "", mdadmBody, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered token: %d", w.Code)
	}
	if w := e.do(e.other, "", mdadmBody, nil); w.Code != http.StatusForbidden {
		t.Fatalf("token of another bot: %d", w.Code)
	}
	if len(e.del.texts) != 0 {
		t.Fatal("nothing must be delivered without a valid token")
	}
	// X-Notify-Token header also accepted
	w = e.do("", "", mdadmBody, map[string]string{"X-Notify-Token": e.token})
	if w.Code != http.StatusOK {
		t.Fatalf("good token: %d %s", w.Code, w.Body)
	}
	r := decodeNotify(t, w)
	if !r.Delivered || r.Deduplicated || r.RateLimited || r.ID == "" || r.Status != "delivered" {
		t.Fatalf("result: %+v", r)
	}
	if len(e.del.texts) != 1 || !strings.Contains(e.del.texts[0], "DegradedArray on /dev/md0") {
		t.Fatalf("delivered: %v", e.del.texts)
	}
}

func TestNotifyHTTP_CIDR(t *testing.T) {
	e := newNotifyEnv(t)
	if w := e.do(e.token, "203.0.113.5:1234", mdadmBody, nil); w.Code != http.StatusForbidden {
		t.Fatalf("public caller must be 403: %d", w.Code)
	}
	// 经本机 nginx 转发的公网请求：以 XFF 中的公网地址判定
	if w := e.do(e.token, "172.18.0.1:5000", mdadmBody, map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Real-IP": "198.51.100.7"}); w.Code != http.StatusForbidden {
		t.Fatalf("public client via proxy must be 403: %d", w.Code)
	}
	// 伪造 XFF 不能冒充本地
	if w := e.do(e.token, "203.0.113.5:1234", mdadmBody, map[string]string{"X-Forwarded-For": "127.0.0.1"}); w.Code != http.StatusForbidden {
		t.Fatalf("spoofed XFF: %d", w.Code)
	}
	// 自定义白名单
	e.set(t, config.KeyNotifyAllowedCIDRs, "10.9.0.0/16")
	if w := e.do(e.token, "127.0.0.1:1", mdadmBody, nil); w.Code != http.StatusForbidden {
		t.Fatalf("loopback not in custom list: %d", w.Code)
	}
	if w := e.do(e.token, "10.9.3.4:1", mdadmBody, nil); w.Code != http.StatusOK {
		t.Fatalf("allowed subnet: %d %s", w.Code, w.Body)
	}
}

func TestNotifyHTTP_ValidationAndSize(t *testing.T) {
	e := newNotifyEnv(t)
	if w := e.do(e.token, "", "{not json", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", w.Code)
	}
	if w := e.do(e.token, "", map[string]string{"source": "maid/x", "level": "fatal!", "title": "t"}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad level: %d", w.Code)
	}
	if w := e.do(e.token, "", map[string]string{"level": "info", "title": "t"}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("missing source: %d", w.Code)
	}
	big := map[string]string{"source": "maid/x", "level": "info", "title": "t", "body": strings.Repeat("A", 20000)}
	if w := e.do(e.token, "", big, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", w.Code)
	}
	// 无 Content-Length（chunked）同样受限
	raw, _ := json.Marshal(big)
	req := httptest.NewRequest(http.MethodPost, "/api/bots/"+notifyTestBot+"/notify", bytes.NewReader(raw))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversized body: %d", w.Code)
	}
	if len(e.del.texts) != 0 {
		t.Fatal("invalid requests must not deliver")
	}
}

func TestNotifyHTTP_DedupRateLimitAndDisabled(t *testing.T) {
	e := newNotifyEnv(t)
	e.set(t, config.KeyNotifyRateLimit, "1/1h")
	if w := e.do(e.token, "", mdadmBody, nil); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w := e.do(e.token, "", mdadmBody, nil)
	r := decodeNotify(t, w)
	if w.Code != 200 || !r.Deduplicated || r.RepeatCount != 2 {
		t.Fatalf("dedup: %d %+v", w.Code, r)
	}
	info := map[string]string{"source": "maid/cron", "level": "info", "title": "backup ok"}
	if w := e.do(e.token, "", info, nil); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	info["title"] = "backup ok 2"
	w = e.do(e.token, "", info, nil)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !decodeNotify(t, w).RateLimited {
		t.Fatalf("rate limit: %d %s", w.Code, w.Body)
	}

	var rows []dao.NotifyEvent
	e.db.Order("created_at ASC").Find(&rows)
	statuses := map[string]int{}
	for _, r := range rows {
		statuses[r.Status]++
		if r.TokenID == "" || r.CallerIP != "127.0.0.1" {
			t.Fatalf("audit row missing token/ip: %+v", r)
		}
	}
	if statuses["delivered"] != 2 || statuses["deduplicated"] != 1 || statuses["rate_limited"] != 1 {
		t.Fatalf("audit statuses: %v", statuses)
	}

	e.set(t, config.KeyNotifyEnabled, "false")
	if w := e.do(e.token, "", mdadmBody, nil); w.Code != http.StatusNotFound {
		t.Fatalf("disabled: %d", w.Code)
	}
}

func TestNotifyHTTP_HistoryRecorded(t *testing.T) {
	e := newNotifyEnv(t)
	w := e.do(e.token, "", mdadmBody, nil)
	r := decodeNotify(t, w)
	var msgs []dao.ChatMessage
	e.db.Where("bot_id = ? AND session_id = ?", notifyTestBot, "tg:76017910").Find(&msgs)
	if len(msgs) != 1 {
		t.Fatalf("history rows: %d", len(msgs))
	}
	m := msgs[0]
	if m.Role != dao.ChatRoleAssistant || m.UserID != "tg:76017910" || m.TraceID != r.ID ||
		!strings.Contains(m.Content, "DegradedArray on /dev/md0") || !strings.Contains(m.Content, "不是指令") {
		t.Fatalf("history row: %+v", m)
	}
	e.set(t, config.KeyNotifyRecordHistory, "false")
	e.do(e.token, "", map[string]string{"source": "maid/x", "level": "info", "title": "other"}, nil)
	var n int64
	e.db.Model(&dao.ChatMessage{}).Count(&n)
	if n != 1 {
		t.Fatalf("record_history=false must skip history, rows=%d", n)
	}
}

func TestNotifyDiscoverOwnerTarget(t *testing.T) {
	e := newNotifyEnv(t)
	e.db.Create(&dao.User{ID: 1, Username: "admin", Role: "admin", Status: "active", PasswordHash: "x"})
	e.db.Create(&dao.User{ID: 2, Username: "guest", Role: "member", Status: "active", PasswordHash: "x"})
	e.db.Create(&dao.IdentityMapping{UserID: 2, Platform: "telegram", PlatformUserID: "11111"})
	e.db.Create(&dao.IdentityMapping{UserID: 1, Platform: "web", PlatformUserID: "1"})
	e.db.Create(&dao.IdentityMapping{UserID: 1, Platform: "telegram", PlatformUserID: "76017910"})
	if got := e.s.discoverOwnerTarget(context.Background(), "telegram"); got != "76017910" {
		t.Fatalf("owner = %q", got)
	}
	if got := e.s.discoverOwnerTarget(context.Background(), "misskey"); got != "" {
		t.Fatalf("no misskey binding expected, got %q", got)
	}
}

func TestNotifyRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := newNotifyEnv(t)
	s := e.s
	s.engine = gin.New()
	s.registerRoutes() // 不得与现有 /api/bots/:id/... 路由冲突
	req := httptest.NewRequest(http.MethodPost, "/api/bots/"+notifyTestBot+"/notify", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "thinkbot-notify") {
		t.Fatalf("notify route must be public (token auth, not cookie): %d %s", w.Code, w.Body)
	}

	// 独立监听器模式：主路由不暴露 notify
	s.engine = gin.New()
	s.notifyListenAddr = "127.0.0.1:0"
	s.registerRoutes()
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/bots/"+notifyTestBot+"/notify", strings.NewReader(`{}`))
	s.engine.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized && strings.Contains(w.Header().Get("WWW-Authenticate"), "thinkbot-notify") {
		t.Fatal("notify route must not be on the main router when notify.listen_addr is set")
	}
}

// fakeNotifyChannel 实现 bot.Channel + bot.Sender。
type fakeNotifyChannel struct {
	name, typ string
	sent      []core.Action
}

func (c *fakeNotifyChannel) Name() string                                  { return c.name }
func (c *fakeNotifyChannel) Type() string                                  { return c.typ }
func (c *fakeNotifyChannel) BotID() string                                 { return notifyTestBot }
func (c *fakeNotifyChannel) Start(context.Context, *inbound.Ingress) error { return nil }
func (c *fakeNotifyChannel) Stop(context.Context) error                    { return nil }
func (c *fakeNotifyChannel) Send(_ context.Context, a core.Action) error {
	c.sent = append(c.sent, a)
	return nil
}

func TestResolveAndSendNotifyTarget(t *testing.T) {
	tg := &fakeNotifyChannel{name: "Telegram", typ: "telegram"}
	mk := &fakeNotifyChannel{name: "Misskey", typ: "misskey"}
	chs := []bot.Channel{mk, tg}
	cfg := notify.DefaultConfig()
	discover := func(p string) string {
		if p == "telegram" {
			return "76017910"
		}
		return ""
	}

	got, err := resolveNotifyTarget(chs, "", "", cfg, discover)
	if err != nil || got.ChannelName != "Telegram" || got.ChatID != "76017910" {
		t.Fatalf("default owner DM: %+v %v", got, err)
	}
	if got, _ := resolveNotifyTarget(chs, "Telegram", "", cfg, discover); got.ChannelName != "Telegram" {
		t.Fatalf("by instance name: %+v", got)
	}
	if _, err := resolveNotifyTarget(chs, "misskey", "", cfg, discover); !errors.Is(err, notify.ErrChannelUnsupported) {
		t.Fatalf("misskey unsupported: %v", err)
	}
	if _, err := resolveNotifyTarget(chs, "discord", "", cfg, discover); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("unknown channel: %v", err)
	}
	if _, err := resolveNotifyTarget(chs, "", "", cfg, func(string) string { return "" }); !errors.Is(err, notify.ErrNoOwnerTarget) {
		t.Fatalf("no owner: %v", err)
	}
	cfg.OwnerTarget = "42"
	if got, _ := resolveNotifyTarget(chs, "", "", cfg, discover); got.ChatID != "42" {
		t.Fatalf("configured owner target wins over discovery: %+v", got)
	}
	if got, _ := resolveNotifyTarget(chs, "", "99", cfg, discover); got.ChatID != "99" {
		t.Fatalf("explicit target: %+v", got)
	}

	if err := sendNotifyToChannels(context.Background(), chs, notify.Target{ChannelName: "Telegram", ChatID: "76017910"}, "*hi* <b>x</b>"); err != nil {
		t.Fatal(err)
	}
	if len(tg.sent) != 1 {
		t.Fatalf("sent: %d", len(tg.sent))
	}
	a := tg.sent[0]
	pm, ok := a.Metadata["parse_mode"].(string)
	if a.Channel != "76017910" || a.Type != core.ActionReply || !ok || pm != "" || a.Payload != "*hi* <b>x</b>" {
		t.Fatalf("action: %+v", a)
	}
}
