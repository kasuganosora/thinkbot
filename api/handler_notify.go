package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/notify"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Notify — 外部程序让 bot 给主人发通知（docs/notify.md）
//
//   POST   /api/notify                       → 外部调用，请求体 bot 指定经哪个 bot（notify token 鉴权，非会话）
//   POST   /api/bots/:id/notify              → 同上，bot 取路径（请求体 bot 可省略，给出须一致）
//   GET    /api/notify/tokens                → 列出全部 token（admin）
//   POST   /api/notify/tokens                → 创建 token（scope：bots 列表或 allBots），明文只返回一次（admin）
//   DELETE /api/notify/tokens/:tid           → 吊销 token（admin）
//   GET    /api/bots/:id/notify/tokens       → 列出作用域覆盖该 bot 的 token（admin）
//   POST   /api/bots/:id/notify/tokens       → 创建 token（默认 scope = 该 bot）（admin）
//   DELETE /api/bots/:id/notify/tokens/:tid  → 吊销 token（admin）
//   GET    /api/bots/:id/notify/events       → 审计记录（admin）
//
// 复用机制：投递走与心跳 / outreach 兜底相同的「直接取渠道 Sender.Send」路径
// （heartbeatChannelPoster 同款），不经 pipeline，因此天然绕过 reply_control、
// 低信息抑制、发言模式等闸门；主人 = 活跃 admin 用户在该平台的身份绑定。
// ============================================================================

// registerNotifyCallRoutes 注册外部调用入口（主 API 或独立监听器共用）。g 为 /api 分组。
func registerNotifyCallRoutes(g gin.IRoutes, h gin.HandlerFunc) {
	g.POST("/notify", h)
	g.POST("/bots/:id/notify", h)
}

// isNotifyCallPath 识别 notify 调用路径（请求日志跳过 body 预读 / 记录）。
func isNotifyCallPath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	return path == "/api/notify" || (strings.HasPrefix(path, "/api/bots/") && strings.HasSuffix(path, "/notify"))
}

// notifyBotIDRE 限定请求里的 bot ID 形态。
var notifyBotIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// initNotify 构造 notify 服务与 token 仓储（db 缺失时不启用）。
func (s *Server) initNotify() {
	if s.db == nil {
		return
	}
	// 回填旧 token 的作用域（scope = bot_id）；表本身已由 dao.Migrate 建好，这里幂等。
	if err := notify.MigrateTokens(s.db); err != nil && s.logger != nil {
		s.logger.Warnw("notify: token scope backfill failed", "err", err)
	}
	s.notifyTokens = notify.NewTokenStore(s.db)
	s.notifySvc = notify.NewService(s.db,
		func(botID string) notify.Config { return notify.LoadConfig(s.store, botID) },
		&notifyResolver{s: s},
		&notifyDeliverer{s: s},
		&notify.LLMBot{Source: s.notifyBotContext},
		&notifyHistory{s: s},
		s.logger,
	)
}

// handleNotify 处理外部通知调用。
// 顺序：总开关(404) → 来源 CIDR(403) → token(401) → 请求体大小(413) → JSON(400)
// → 目标 bot（缺失 / 路径与请求体不一致 400）→ token 作用域(403) → 服务编排。
func (s *Server) handleNotify(c *gin.Context) {
	pathBot := strings.TrimSpace(c.Param("id"))
	if s.notifySvc == nil || s.notifyTokens == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "notify unavailable"})
		return
	}
	// 全局项（enabled / max_request_bytes）与 bot 无关，先用全局配置。
	gcfg := notify.LoadConfig(s.store, "")
	if !gcfg.Enabled {
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
		s.logger.Warnw("notify: caller address not allowed", "path_bot", pathBot, "ip", ipStr, "remote", c.Request.RemoteAddr)
		c.JSON(http.StatusForbidden, gin.H{"error": "source address not allowed"})
		return
	}

	tok, err := s.notifyTokens.Authenticate(c.Request.Context(), notifyTokenFromRequest(c.Request))
	if err != nil {
		switch {
		case errors.Is(err, notify.ErrTokenMissing), errors.Is(err, notify.ErrTokenInvalid):
			s.logger.Warnw("notify: unauthorized", "path_bot", pathBot, "ip", ipStr, "reason", err.Error())
			c.Header("WWW-Authenticate", `Bearer realm="thinkbot-notify"`)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
		default:
			s.logger.Errorw("notify: token lookup failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}

	if c.Request.ContentLength > gcfg.MaxRequestBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large", "limit": gcfg.MaxRequestBytes})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, gcfg.MaxRequestBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large", "limit": gcfg.MaxRequestBytes})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
		return
	}
	var req notify.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	botID, msg := resolveNotifyBotID(pathBot, req.Bot)
	if msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if !notify.TokenAllows(tok, botID) {
		s.logger.Warnw("notify: token used for bot outside its scope", "bot_id", botID, "token_id", tok.ID, "ip", ipStr)
		c.JSON(http.StatusForbidden, gin.H{"error": "token not valid for this bot"})
		return
	}
	s.notifyTokens.Touch(c.Request.Context(), tok.ID)
	caller := notify.Caller{TokenID: tok.ID, IP: ipStr}

	res := s.notifySvc.Notify(c.Request.Context(), botID, caller, req)
	if res.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(res.RetryAfter))
	}
	c.JSON(res.HTTPStatus, res)
}

// resolveNotifyBotID 从路径与请求体确定目标 bot；返回非空 msg 表示 400。
func resolveNotifyBotID(pathBot, bodyBot string) (string, string) {
	bodyBot = strings.TrimSpace(bodyBot)
	switch {
	case pathBot != "" && bodyBot != "" && pathBot != bodyBot:
		return "", "bot in request body does not match bot in path"
	case pathBot != "":
		bodyBot = pathBot
	case bodyBot == "":
		return "", "bot is required (bot id of the bot that should notify its owner)"
	}
	if !notifyBotIDRE.MatchString(bodyBot) {
		return "", "bot contains invalid characters"
	}
	return bodyBot, ""
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
// 管理接口（admin 会话）。路径带 :id 时按该 bot 过滤 / 作默认作用域；
// /api/notify/tokens 为跨 bot 视图。
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
	views := make([]notify.TokenView, 0, len(rows))
	for _, r := range rows {
		views = append(views, notify.ViewOf(r))
	}
	OK(c, gin.H{"tokens": views, "total": len(views)})
}

func (s *Server) handleCreateNotifyToken(c *gin.Context) {
	if s.notifyTokens == nil {
		Fail(c, errs.New("notify unavailable"))
		return
	}
	pathBot := c.Param("id")
	var req struct {
		Name    string   `json:"name"`
		Bots    []string `json:"bots"`
		AllBots bool     `json:"allBots"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.AllBots && len(req.Bots) > 0 {
		Fail(c, errs.BadRequest("use either bots or allBots, not both"))
		return
	}
	scope := req.Bots
	switch {
	case req.AllBots:
		scope = []string{notify.ScopeAll}
	case len(scope) == 0 && pathBot != "":
		scope = []string{pathBot}
	}
	norm, err := notify.NormalizeScope(scope)
	if err != nil {
		Fail(c, errs.BadRequest(err.Error()))
		return
	}
	if norm[0] != notify.ScopeAll && s.botSvc != nil && s.botSvc.db != nil {
		for _, b := range norm {
			if _, err := s.botSvc.GetDefinition(b); err != nil {
				Fail(c, errs.NotFound("bot not found: "+b))
				return
			}
		}
	}
	plain, row, err := s.notifyTokens.Create(c.Request.Context(), norm, req.Name)
	if err != nil {
		Fail(c, errs.Wrap(err, "create notify token"))
		return
	}
	auditLog(c, s.logger, "create_notify_token", "token_id", row.ID, "scope", row.Scope, "name", row.Name)
	setNoStore(c)
	v := notify.ViewOf(*row)
	OK(c, gin.H{"id": v.ID, "botId": v.BotID, "scope": v.Scope, "allBots": v.AllBots, "name": v.Name, "createdAt": v.CreatedAt,
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

// notifyRecall 用该 bot 对话 pipeline 里的同一个 RecallStage 召回长期记忆：
// 构造一条「主人私聊」形态的消息（bot / 会话 / 用户三 scope 与入站 Telegram 私聊一致，
// 通知标题 + 正文作为相关性召回的 query），读取其注入的 KVMemoryRecall。失败返回空串。
func (s *BotService) notifyRecall(ctx context.Context, botID string, t notify.Target, n notify.Notification) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	rs := s.recallStages[botID]
	s.mu.RUnlock()
	if rs == nil {
		return ""
	}
	env := core.NewEnvelope(core.Message{
		BotID:    botID,
		Source:   t.ChannelName,
		Channel:  t.ChatID,
		UserID:   t.ChatID,
		ChatType: core.ChatPrivate,
		Text:     strings.TrimSpace(n.Title + "\n" + n.Body),
	})
	out, err := rs.Process(ctx, env)
	if err != nil || out == nil {
		return ""
	}
	if v, ok := out.Get(core.KVMemoryRecall); ok {
		if text, ok := v.(string); ok {
			return text
		}
	}
	return ""
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

// notifySessionID 返回投递目标对应的会话键（Telegram: "tg:<chat>"，与入站历史同键）。
func notifySessionID(t notify.Target) string {
	if t.ChannelType == "telegram" {
		return "tg:" + t.ChatID
	}
	return t.ChatID
}

// notifyHistory 把已送达通知写进主人会话：系统备注（role=notify，LLM 上下文中为 system 消息）
// + bot 模式下 bot 的原话（role=assistant）。trace_id = 事件 ID，不与对话轮次的
// UpsertAssistantByTrace 冲突；均为纯文本，不含 tool 调用，不影响工具配对 / 压缩边界。
type notifyHistory struct{ s *Server }

func (h *notifyHistory) Record(_ context.Context, botID string, t notify.Target, eventID string, entries []notify.HistoryEntry) error {
	if h.s.chatHistory == nil {
		return errors.New("chat history unavailable")
	}
	sid := notifySessionID(t)
	for _, e := range entries {
		role := dao.ChatRoleNotify
		if e.Role == notify.HistoryRoleAssistant {
			role = dao.ChatRoleAssistant
		}
		if err := h.s.chatHistory.SaveMessage(botID, sid, role, e.Content, eventID, sid); err != nil {
			return err
		}
	}
	return nil
}

// notifyBotContext 为 bot 模式组装 bot 的真实上下文：
//   - 模型：bot 主模型（温度跟随 ModelDef）；reasoning_effort / 输出上限走内部调用策略
//     （llm.InternalPolicy，用途 notify，默认 low）；
//   - 身份：运行中 bot 已加载的 SOUL.md（未加载则用 bot 配置的 system prompt）；
//   - 记忆：该 bot 的 RecallStage（与对话主链路同一实例、同一 scope 规则：bot / 会话 / 用户）；
//   - 历史：主人会话近期消息（与入站 enricher 同源：LoadContextBySession + 上下文检查点）。
func (s *Server) notifyBotContext(ctx context.Context, botID string, t notify.Target, n notify.Notification, historyLimit int) (*notify.BotContext, error) {
	if s.botSvc == nil {
		return nil, notify.ErrBotUnavailable
	}
	bundle, ok := s.botSvc.GetLLMBundle(botID)
	if !ok || bundle == nil || bundle.Main == nil {
		return nil, notify.ErrBotUnavailable
	}
	bc := &notify.BotContext{
		Provider:       bundle.Main,
		Model:          bundle.MainDef.Model,
		ModelMaxTokens: bundle.MainDef.MaxTokens,
		Temperature:    bundle.MainDef.Temperature,
	}
	var def *dao.BotDefinition
	botEffort := ""
	if s.botSvc.db != nil {
		if d, err := s.botSvc.GetDefinition(botID); err == nil {
			def = d
			bc.BotName = d.Name
			botEffort = d.ReasoningEffort
		}
	}
	// 与其它内部调用共用的策略（用途 llm.PurposeNotify）：reasoning_effort + 输出上限。
	bc.Policy = newInternalPolicy(s.store, s.logger, botEffort, bundle)

	if b := s.botSvc.runningBot(botID); b != nil && b.SoulLoader() != nil && b.SoulLoader().Loaded() {
		bc.Identity = b.SoulLoader().Content()
	}
	if strings.TrimSpace(bc.Identity) == "" && def != nil {
		bc.Identity = def.SystemPrompt
	}

	bc.Memory = s.botSvc.notifyRecall(ctx, botID, t, n)

	if historyLimit > 0 && s.chatHistory != nil {
		sid := notifySessionID(t)
		if rows, err := s.chatHistory.LoadContextBySession(botID, sid, historyLimit); err != nil {
			s.logger.Warnw("notify: load owner history failed", "bot_id", botID, "session", sid, "err", err)
		} else {
			rows = s.chatHistory.ApplyContextCheckpoint("notify", botID, sid, rows)
			bc.History, _ = chatHistoryToLLM(rows)
		}
	}
	return bc, nil
}
