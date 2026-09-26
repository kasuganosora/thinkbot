package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/notify"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Notify — 外部程序让 bot 给主人发通知（docs/notify.md）
//
//   POST   /api/bots/:id/notify              → 外部调用（notify token 鉴权，非会话）
//   GET    /api/bots/:id/notify/tokens       → 列出 token（admin）
//   POST   /api/bots/:id/notify/tokens       → 创建 token，明文只返回一次（admin）
//   DELETE /api/bots/:id/notify/tokens/:tid  → 吊销 token（admin）
//   GET    /api/bots/:id/notify/events       → 审计记录（admin）
//
// 复用机制：投递走与心跳 / outreach 兜底相同的「直接取渠道 Sender.Send」路径
// （heartbeatChannelPoster 同款），不经 pipeline，因此天然绕过 reply_control、
// 低信息抑制、发言模式等闸门；主人 = 活跃 admin 用户在该平台的身份绑定。
// ============================================================================

// notifyPathSuffix 用于识别 notify 调用路径（请求日志跳过 body 预读 / 记录）。
const notifyPathSuffix = "/notify"

func isNotifyCallPath(method, path string) bool {
	return method == http.MethodPost && strings.HasPrefix(path, "/api/bots/") && strings.HasSuffix(path, notifyPathSuffix)
}

// initNotify 构造 notify 服务与 token 仓储（db 缺失时不启用）。
func (s *Server) initNotify() {
	if s.db == nil {
		return
	}
	s.notifyTokens = notify.NewTokenStore(s.db)
	s.notifySvc = notify.NewService(s.db,
		func(botID string) notify.Config { return notify.LoadConfig(s.store, botID) },
		&notifyResolver{s: s},
		&notifyDeliverer{s: s},
		&notify.LLMPersona{Source: s.notifyPersonaSource},
		&notifyHistory{s: s},
		s.logger,
	)
}

// handleNotify 处理外部通知调用。
// 顺序：总开关(404) → 来源 CIDR(403) → token(401/403) → 请求体大小(413) → JSON(400) → 服务编排。
func (s *Server) handleNotify(c *gin.Context) {
	botID := c.Param("id")
	if s.notifySvc == nil || s.notifyTokens == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "notify unavailable"})
		return
	}
	cfg := notify.LoadConfig(s.store, botID)
	if !cfg.Enabled {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	allowed, _ := notify.ParseCIDRs(s.storeString(config.KeyNotifyAllowedCIDRs, config.DefaultNotifyCIDRs))
	trusted, _ := notify.ParseCIDRs(s.storeString(config.KeyNotifyTrustedProxies, config.DefaultNotifyCIDRs))
	ip := notify.ClientIP(c.Request, trusted)
	ipStr := ""
	if ip.IsValid() {
		ipStr = ip.String()
	}
	if !notify.ContainsIP(allowed, ip) {
		s.logger.Warnw("notify: caller address not allowed", "bot_id", botID, "ip", ipStr, "remote", c.Request.RemoteAddr)
		c.JSON(http.StatusForbidden, gin.H{"error": "source address not allowed"})
		return
	}

	tok, err := s.notifyTokens.Authenticate(c.Request.Context(), notifyTokenFromRequest(c.Request), botID)
	if err != nil {
		switch {
		case errors.Is(err, notify.ErrTokenScope):
			s.logger.Warnw("notify: token used for wrong bot", "bot_id", botID, "token_id", tok.ID, "ip", ipStr)
			c.JSON(http.StatusForbidden, gin.H{"error": "token not valid for this bot"})
		case errors.Is(err, notify.ErrTokenMissing), errors.Is(err, notify.ErrTokenInvalid):
			s.logger.Warnw("notify: unauthorized", "bot_id", botID, "ip", ipStr, "reason", err.Error())
			c.Header("WWW-Authenticate", `Bearer realm="thinkbot-notify"`)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
		default:
			s.logger.Errorw("notify: token lookup failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	caller := notify.Caller{TokenID: tok.ID, IP: ipStr}

	if c.Request.ContentLength > cfg.MaxRequestBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large", "limit": cfg.MaxRequestBytes})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, cfg.MaxRequestBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large", "limit": cfg.MaxRequestBytes})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
		return
	}
	var req notify.Request
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	res := s.notifySvc.Notify(c.Request.Context(), botID, caller, req)
	if res.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(res.RetryAfter))
	}
	c.JSON(res.HTTPStatus, res)
}

// notifyTokenFromRequest 取 Authorization: Bearer <token>，或 X-Notify-Token。
// 不接受 query 参数（会进访问日志）。
func notifyTokenFromRequest(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Notify-Token"))
}

func (s *Server) storeString(key, def string) string {
	if s.store == nil {
		return def
	}
	return s.store.GetString(key, def)
}

// ---------------------------------------------------------------------------
// 管理接口（admin 会话）
// ---------------------------------------------------------------------------

func (s *Server) handleListNotifyTokens(c *gin.Context) {
	if s.notifyTokens == nil {
		Fail(c, errs.New("notify unavailable"))
		return
	}
	rows, err := s.notifyTokens.List(c.Request.Context(), c.Param("id"))
	if err != nil {
		Fail(c, errs.Wrap(err, "list notify tokens"))
		return
	}
	OK(c, gin.H{"tokens": rows, "total": len(rows)})
}

func (s *Server) handleCreateNotifyToken(c *gin.Context) {
	if s.notifyTokens == nil {
		Fail(c, errs.New("notify unavailable"))
		return
	}
	botID := c.Param("id")
	var req struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&req)
	if s.botSvc != nil && s.botSvc.db != nil {
		if _, err := s.botSvc.GetDefinition(botID); err != nil {
			Fail(c, errs.NotFound("bot not found"))
			return
		}
	}
	plain, row, err := s.notifyTokens.Create(c.Request.Context(), botID, req.Name)
	if err != nil {
		Fail(c, errs.Wrap(err, "create notify token"))
		return
	}
	auditLog(c, s.logger, "create_notify_token", "bot_id", botID, "token_id", row.ID, "name", row.Name)
	setNoStore(c)
	OK(c, gin.H{"id": row.ID, "botId": row.BotID, "name": row.Name, "createdAt": row.CreatedAt,
		"notifyToken": plain, "note": "shown once; only a hash is stored"})
}

func (s *Server) handleRevokeNotifyToken(c *gin.Context) {
	if s.notifyTokens == nil {
		Fail(c, errs.New("notify unavailable"))
		return
	}
	botID, tid := c.Param("id"), c.Param("tid")
	ok, err := s.notifyTokens.Revoke(c.Request.Context(), botID, tid)
	if err != nil {
		Fail(c, errs.Wrap(err, "revoke notify token"))
		return
	}
	if !ok {
		Fail(c, errs.NotFound("token not found or already revoked"))
		return
	}
	auditLog(c, s.logger, "revoke_notify_token", "bot_id", botID, "token_id", tid)
	OK(c, gin.H{"id": tid, "revoked": true})
}

func (s *Server) handleListNotifyEvents(c *gin.Context) {
	if s.notifySvc == nil {
		Fail(c, errs.New("notify unavailable"))
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	rows, err := s.notifySvc.ListEvents(c.Request.Context(), c.Param("id"), limit)
	if err != nil {
		Fail(c, errs.Wrap(err, "list notify events"))
		return
	}
	OK(c, gin.H{"events": rows, "total": len(rows)})
}

// ---------------------------------------------------------------------------
// 适配器
// ---------------------------------------------------------------------------

// runningBot 线程安全地取运行中的 bot 实例。
func (s *BotService) runningBot(botID string) *bot.Bot {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.botInstances[botID]
}

// notifyResolver 解析投递目标：渠道 = 请求覆盖 / 配置默认（类型或实例名）；
// 会话 = 请求 target（需 notify.allow_target_override）/ 配置 owner_target /
// 自动发现（活跃 admin 用户在该平台的 identity_mappings 绑定，Telegram 私聊 chat id = 用户 id）。
type notifyResolver struct{ s *Server }

func (r *notifyResolver) Resolve(ctx context.Context, botID, channel, target string) (notify.Target, error) {
	s := r.s
	cfg := notify.LoadConfig(s.store, botID)
	if s.botSvc == nil {
		return notify.Target{}, notify.ErrBotNotRunning
	}
	b := s.botSvc.runningBot(botID)
	if b == nil {
		if s.botSvc.db != nil {
			if _, err := s.botSvc.GetDefinition(botID); err != nil {
				return notify.Target{}, notify.ErrBotNotFound
			}
		}
		return notify.Target{}, notify.ErrBotNotRunning
	}
	return resolveNotifyTarget(b.Channels(), channel, target, cfg,
		func(platform string) string { return s.discoverOwnerTarget(ctx, platform) })
}

// resolveNotifyTarget 纯逻辑部分（便于测试）：选渠道 → 校验类型 → 定会话。
func resolveNotifyTarget(chs []bot.Channel, channel, target string, cfg notify.Config, discover func(platform string) string) (notify.Target, error) {
	want := strings.TrimSpace(channel)
	if want == "" {
		want = cfg.DefaultChannel
	}
	chosen := pickChannel(chs, want)
	if chosen == nil {
		return notify.Target{}, notify.ErrChannelNotFound
	}
	if chosen.Type() != "telegram" {
		return notify.Target{}, notify.ErrChannelUnsupported
	}
	t := notify.Target{ChannelName: chosen.Name(), ChannelType: chosen.Type()}
	switch {
	case target != "":
		t.ChatID = target
	case cfg.OwnerTarget != "":
		t.ChatID = cfg.OwnerTarget
	case discover != nil:
		t.ChatID = discover(chosen.Type())
	}
	if t.ChatID == "" {
		return notify.Target{}, notify.ErrNoOwnerTarget
	}
	if _, err := strconv.ParseInt(t.ChatID, 10, 64); err != nil {
		return notify.Target{}, notify.ErrNoOwnerTarget
	}
	return t, nil
}

// pickChannel 先按实例名精确匹配，再按类型取第一个。
func pickChannel(chs []bot.Channel, want string) bot.Channel {
	for _, ch := range chs {
		if ch.Name() == want {
			return ch
		}
	}
	for _, ch := range chs {
		if strings.EqualFold(ch.Type(), want) {
			return ch
		}
	}
	return nil
}

// discoverOwnerTarget 取活跃 admin 用户（按 id 最小者优先）在 platform 上的绑定 ID。
func (s *Server) discoverOwnerTarget(ctx context.Context, platform string) string {
	if s.db == nil {
		return ""
	}
	var ids []string
	err := s.db.WithContext(ctx).Table("identity_mappings AS m").
		Select("m.platform_user_id").
		Joins("JOIN users AS u ON u.id = m.user_id").
		Where("m.platform = ? AND u.role = ? AND u.status = ?", platform, auth.RoleAdmin, auth.StatusActive).
		Order("u.id ASC, m.id ASC").Limit(1).
		Pluck("m.platform_user_id", &ids).Error
	if err != nil || len(ids) == 0 {
		return ""
	}
	return strings.TrimSpace(ids[0])
}

// notifyDeliverer 与 heartbeatChannelPoster 同路：直接调渠道 Sender.Send，不经 pipeline。
// 强制 parse_mode="" 纯文本发送，外部内容中的 Markdown/HTML 不会被 Telegram 解析。
type notifyDeliverer struct{ s *Server }

func (d *notifyDeliverer) Deliver(ctx context.Context, botID string, t notify.Target, text string) error {
	b := d.s.botSvc.runningBot(botID)
	if b == nil {
		return notify.ErrBotNotRunning
	}
	return sendNotifyToChannels(ctx, b.Channels(), t, text)
}

// sendNotifyToChannels 在渠道列表中找到目标实例并以纯文本发送。
func sendNotifyToChannels(ctx context.Context, chs []bot.Channel, t notify.Target, text string) error {
	var chosen bot.Channel
	for _, ch := range chs {
		if ch.Name() == t.ChannelName {
			chosen = ch
			break
		}
	}
	if chosen == nil {
		return notify.ErrChannelNotFound
	}
	sender, ok := chosen.(bot.Sender)
	if !ok {
		return notify.ErrChannelUnsupported
	}
	return sender.Send(ctx, core.Action{
		Type:    core.ActionReply,
		Channel: t.ChatID,
		Payload: text,
		Metadata: map[string]any{
			"source_channel": t.ChannelName,
			// 强制纯文本：外部内容里的 Markdown/HTML 不会被 Telegram 解析，也不会因格式错误整条 400。
			"parse_mode": "",
		},
	})
}

// notifyHistory 把已送达通知写进主人会话（Telegram: session "tg:<chat>"，与入站历史同键）。
type notifyHistory struct{ s *Server }

func (h *notifyHistory) Record(_ context.Context, botID string, t notify.Target, eventID, content string) error {
	if h.s.chatHistory == nil {
		return errors.New("chat history unavailable")
	}
	sid := t.ChatID
	if t.ChannelType == "telegram" {
		sid = "tg:" + t.ChatID
	}
	return h.s.chatHistory.SaveMessage(botID, sid, dao.ChatRoleAssistant, content, eventID, sid)
}

// notifyPersonaSource 提供 persona 改写所需的 LLM（bot 主模型）与人格文本（SOUL.md）。
func (s *Server) notifyPersonaSource(botID string) (llm.Provider, string, int, string, bool) {
	if s.botSvc == nil {
		return nil, "", 0, "", false
	}
	bundle, ok := s.botSvc.GetLLMBundle(botID)
	if !ok || bundle == nil || bundle.Main == nil {
		return nil, "", 0, "", false
	}
	persona := ""
	if soul, err := s.readBotSoul(context.Background(), botID); err == nil && soul != nil {
		persona = soul.Content
	}
	return bundle.Main, bundle.MainDef.Model, bundle.MainDef.MaxTokens, persona, true
}
