package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// Target 是一次投递的目标：渠道实例 + 会话。
type Target struct {
	ChannelName string // 渠道实例名（bot.Channel.Name()）
	ChannelType string // telegram | ...
	ChatID      string // 会话 ID（Telegram chat id）
}

// 目标解析错误。
var (
	ErrBotNotFound        = errors.New("notify: bot not found")
	ErrBotNotRunning      = errors.New("notify: bot not running")
	ErrChannelNotFound    = errors.New("notify: channel not found on bot")
	ErrChannelUnsupported = errors.New("notify: channel type not supported for notify")
	ErrNoOwnerTarget      = errors.New("notify: owner target not configured and not discoverable")
)

// Resolver 把（渠道覆盖, 目标覆盖）解析为真实投递目标。
// channel 为渠道类型或实例名；target 为空时解析主人私聊。
type Resolver interface {
	Resolve(ctx context.Context, botID, channel, target string) (Target, error)
}

// Deliverer 直接经渠道 Sender 发送（不经 pipeline，因此不受 reply_control /
// 低信息抑制 / 发言模式等闸门影响）。text 为纯文本。
type Deliverer interface {
	Deliver(ctx context.Context, botID string, t Target, text string) error
}

// 会话历史条目角色。
const (
	// HistoryRoleNote 是系统备注（外部通知原文要点，外部数据），在 LLM 上下文里以 system 消息出现。
	HistoryRoleNote = "notify"
	// HistoryRoleAssistant 是 bot 自己发给主人的话（bot 模式转述文本）。
	HistoryRoleAssistant = "assistant"
)

// HistoryEntry 是写入主人会话历史的一条消息（按顺序写入）。
type HistoryEntry struct {
	Role    string
	Content string
}

// HistoryRecorder 把已送达的通知写入主人会话历史，让 bot 知道自己发过。
type HistoryRecorder interface {
	Record(ctx context.Context, botID string, t Target, eventID string, entries []HistoryEntry) error
}

// Caller 是已鉴权的调用方信息。
type Caller struct {
	TokenID string
	IP      string
}

// Result 是一次 notify 调用的结果（直接序列化为 HTTP 响应）。
type Result struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Delivered    bool   `json:"delivered"`
	Deduplicated bool   `json:"deduplicated"`
	RateLimited  bool   `json:"rate_limited"`
	RepeatCount  int    `json:"repeat_count,omitempty"`
	DuplicateOf  string `json:"duplicate_of,omitempty"`
	Bot          string `json:"bot,omitempty"`
	Mode         string `json:"mode,omitempty"`
	// BotUsed：bot 模式下模型输出被采用（false = 回落了 raw）。
	BotUsed    bool   `json:"bot_used,omitempty"`
	Channel    string `json:"channel,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
	Error      string `json:"error,omitempty"`

	HTTPStatus int `json:"-"`
}

// Service 编排：校验 → 去重 → 限流 → 解析目标 → 渲染（bot/raw）→ 投递 → 审计 → 历史。
type Service struct {
	DB        *gorm.DB
	Config    func(botID string) Config
	Resolver  Resolver
	Deliverer Deliverer
	Bot       BotWriter
	History   HistoryRecorder
	Logger    *zap.SugaredLogger
	Now       func() time.Time

	limiter *Limiter
	mu      sync.Mutex // 串行化「去重检查 + 预占 pending 行」，防并发重复投递
}

// NewService 创建服务。
func NewService(db *gorm.DB, cfg func(string) Config, r Resolver, d Deliverer, w BotWriter, h HistoryRecorder, logger *zap.SugaredLogger) *Service {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Service{DB: db, Config: cfg, Resolver: r, Deliverer: d, Bot: w, History: h,
		Logger: logger.With("component", "notify"), Now: time.Now, limiter: NewLimiter()}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) cfg(botID string) Config {
	if s.Config != nil {
		return s.Config(botID)
	}
	return DefaultConfig()
}

// DedupHash 计算去重键：显式 dedup_key 优先，否则 source+level+title+body。
// 两种都含 bot ID（同一告警发给不同 bot 互不去重）；自动哈希包含 level，
// 使 warn → critical 的升级不会被当成重复吞掉。
func DedupHash(botID string, n Notification) string {
	var raw string
	if n.DedupKey != "" {
		raw = "k\x00" + botID + "\x00" + n.DedupKey
	} else {
		raw = "c\x00" + botID + "\x00" + n.Source + "\x00" + n.Level + "\x00" + n.Title + "\x00" + n.Body
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Notify 处理一次已鉴权（含 bot 作用域校验）的通知请求。botID 由调用方从路径 / 请求体确定。
func (s *Service) Notify(ctx context.Context, botID string, caller Caller, req Request) Result {
	cfg := s.cfg(botID)
	now := s.now()
	ev := &dao.NotifyEvent{
		ID:        idgen.New("ntf"),
		BotID:     botID,
		CreatedAt: now.UTC(),
		Source:    truncateRunes(singleLine(Sanitize(req.Source)), 64),
		Level:     truncateRunes(singleLine(req.Level), 16),
		Title:     truncateRunes(singleLine(Sanitize(req.Title)), 500),
		DedupKey:  truncateRunes(singleLine(Sanitize(req.DedupKey)), 250),
		TokenID:   caller.TokenID,
		CallerIP:  caller.IP,
	}
	res := Result{ID: ev.ID, Bot: botID}

	n, err := Validate(req, cfg, now)
	if err != nil {
		ev.Status = StatusRejected
		ev.Error = err.Error()
		s.insert(ctx, ev)
		res.Status, res.Error, res.HTTPStatus = StatusRejected, err.Error(), http.StatusBadRequest
		return res
	}
	ev.Source, ev.Level, ev.Title, ev.DedupKey = n.Source, n.Level, n.Title, n.DedupKey

	mode := n.Mode
	if mode == "" {
		mode = cfg.DefaultMode
	}
	ev.Mode, res.Mode = mode, mode

	if n.Target != "" && !cfg.AllowTargetOverride {
		ev.Status, ev.Error = StatusRejected, "target override not allowed"
		s.insert(ctx, ev)
		res.Status, res.Error, res.HTTPStatus = StatusRejected, ev.Error, http.StatusForbidden
		return res
	}

	ev.DedupHash = DedupHash(botID, n)

	// ---- 去重 + 限流 + 预占（临界区短，不含 LLM / 网络 IO）----
	s.mu.Lock()
	if cfg.DedupWindow > 0 {
		if anchor, ok := s.findAnchor(ctx, botID, ev.DedupHash, now.Add(-cfg.DedupWindow)); ok {
			s.DB.WithContext(ctx).Model(&dao.NotifyEvent{}).Where("id = ?", anchor.ID).
				UpdateColumn("repeat_count", gorm.Expr("repeat_count + 1"))
			ev.Status = StatusDeduplicated
			ev.DuplicateOf = anchor.ID
			ev.RepeatCount = anchor.RepeatCount + 1
			ev.ChannelName, ev.Target = anchor.ChannelName, anchor.Target
			s.insert(ctx, ev)
			s.mu.Unlock()
			res.Status, res.Deduplicated, res.DuplicateOf, res.RepeatCount = StatusDeduplicated, true, anchor.ID, ev.RepeatCount
			res.Delivered = anchor.Status == StatusDelivered
			res.HTTPStatus = http.StatusOK
			return res
		}
	}
	// 限流桶按 token + bot + source 计：同一 token 替多个 bot 发通知时各 bot 预算独立。
	rate, bucketKey := cfg.RateLimit, caller.TokenID+"|"+botID+"|"+n.Source
	if n.Level == LevelCritical {
		// critical 走独立预算：普通告警刷满桶也挡不住 critical。
		rate, bucketKey = cfg.RateLimitCritical, bucketKey+"|critical"
	}
	if ok, wait := s.limiter.Allow(bucketKey, rate, now); !ok {
		ev.Status = StatusRateLimited
		ev.Error = "rate limit " + rate.String()
		s.insert(ctx, ev)
		s.mu.Unlock()
		res.Status, res.RateLimited, res.Error = StatusRateLimited, true, ev.Error
		res.RetryAfter = int(wait.Round(time.Second) / time.Second)
		res.HTTPStatus = http.StatusTooManyRequests
		return res
	}
	prev := s.previousAnchor(ctx, botID, ev.DedupHash, now.Add(-24*time.Hour))
	ev.Status = StatusPending
	s.insert(ctx, ev)
	s.mu.Unlock()

	// ---- 解析目标 ----
	target, err := s.Resolver.Resolve(ctx, botID, n.Channel, n.Target)
	if err != nil {
		code := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrBotNotFound):
			code = http.StatusNotFound
		case errors.Is(err, ErrChannelNotFound), errors.Is(err, ErrChannelUnsupported):
			code = http.StatusBadRequest
		case errors.Is(err, ErrNoOwnerTarget):
			code = http.StatusUnprocessableEntity
		}
		return s.fail(ctx, ev, res, code, err)
	}
	ev.ChannelName, ev.Target = target.ChannelName, target.ChatID
	res.Channel = target.ChannelName

	// ---- 渲染 ----
	text := FormatRaw(n, cfg.Location)
	botUsed := false
	if mode == ModeBot && s.Bot != nil {
		out, berr := s.Bot.Compose(ctx, botID, target, n, cfg)
		if berr != nil || strings.TrimSpace(out) == "" {
			// 模型失败 / 空输出 / 超时：回落 raw，通知绝不因模型而丢失。
			s.Logger.Warnw("notify: bot compose failed, falling back to raw", "bot_id", botID, "event_id", ev.ID, "err", berr)
		} else {
			text = ComposeBot(out, n, cfg.Location)
			botUsed = true
		}
	}
	ev.PersonaUsed, res.BotUsed = botUsed, botUsed
	if prev != nil && prev.RepeatCount > 1 {
		text += fmt.Sprintf("\n↻ 上一条相同通知（%s）之后又重复了 %d 次（已去重）",
			prev.CreatedAt.In(locOr(cfg.Location)).Format("01-02 15:04"), prev.RepeatCount-1)
	}

	// ---- 投递 ----
	if err := s.Deliverer.Deliver(ctx, botID, target, text); err != nil {
		return s.fail(ctx, ev, res, http.StatusBadGateway, err)
	}
	ev.Status = StatusDelivered
	s.update(ctx, ev)
	res.Status, res.Delivered, res.RepeatCount, res.HTTPStatus = StatusDelivered, true, 1, http.StatusOK

	// ---- 会话历史 ----
	// 系统备注（外部数据、原文要点）在前；bot 模式再追加 bot 的原话（assistant），
	// 让之后的对话里 bot 知道自己说过什么、主人追问时能接上。
	if cfg.RecordHistory && s.History != nil {
		entries := []HistoryEntry{{Role: HistoryRoleNote, Content: HistoryNote(ev.ID, n, cfg.Location, botUsed)}}
		if botUsed {
			entries = append(entries, HistoryEntry{Role: HistoryRoleAssistant, Content: text})
		}
		if herr := s.History.Record(ctx, botID, target, ev.ID, entries); herr != nil {
			s.Logger.Warnw("notify: record history failed", "bot_id", botID, "event_id", ev.ID, "err", herr)
		}
	}
	s.Logger.Infow("notify: delivered", "bot_id", botID, "event_id", ev.ID, "source", n.Source,
		"level", n.Level, "mode", mode, "bot_used", botUsed, "channel", target.ChannelName, "token_id", caller.TokenID, "ip", caller.IP)
	return res
}

func (s *Service) fail(ctx context.Context, ev *dao.NotifyEvent, res Result, code int, err error) Result {
	ev.Status = StatusFailed
	ev.Error = truncateRunes(err.Error(), 1000)
	s.update(ctx, ev)
	s.Logger.Warnw("notify: delivery failed", "bot_id", ev.BotID, "event_id", ev.ID, "err", err)
	res.Status, res.Error, res.HTTPStatus = StatusFailed, ev.Error, code
	res.Channel = ev.ChannelName
	return res
}

// findAnchor 查窗口内同哈希、已送达或正在投递的最近一条。
func (s *Service) findAnchor(ctx context.Context, botID, hash string, since time.Time) (*dao.NotifyEvent, bool) {
	var row dao.NotifyEvent
	err := s.DB.WithContext(ctx).
		Where("bot_id = ? AND dedup_hash = ? AND status IN ? AND created_at >= ?",
			botID, hash, []string{StatusDelivered, StatusPending}, since.UTC()).
		Order("created_at DESC").Take(&row).Error
	if err != nil {
		return nil, false
	}
	return &row, true
}

// previousAnchor 查最近一条（已过窗口的）同哈希送达记录，用于附加「重复 N 次」提示。
func (s *Service) previousAnchor(ctx context.Context, botID, hash string, since time.Time) *dao.NotifyEvent {
	var row dao.NotifyEvent
	err := s.DB.WithContext(ctx).
		Where("bot_id = ? AND dedup_hash = ? AND status = ? AND created_at >= ?", botID, hash, StatusDelivered, since.UTC()).
		Order("created_at DESC").Take(&row).Error
	if err != nil {
		return nil
	}
	return &row
}

func (s *Service) insert(ctx context.Context, ev *dao.NotifyEvent) {
	if ev.RepeatCount == 0 {
		ev.RepeatCount = 1
	}
	if err := s.DB.WithContext(ctx).Create(ev).Error; err != nil {
		s.Logger.Errorw("notify: write audit row failed", "event_id", ev.ID, "err", err)
	}
}

func (s *Service) update(ctx context.Context, ev *dao.NotifyEvent) {
	err := s.DB.WithContext(context.WithoutCancel(ctx)).Model(&dao.NotifyEvent{}).Where("id = ?", ev.ID).
		Updates(map[string]any{
			"status":       ev.Status,
			"error":        ev.Error,
			"channel_name": ev.ChannelName,
			"target":       ev.Target,
			"persona_used": ev.PersonaUsed,
		}).Error
	if err != nil {
		s.Logger.Errorw("notify: update audit row failed", "event_id", ev.ID, "err", err)
	}
}

// ListEvents 返回最近的审计记录（管理接口用）。
func (s *Service) ListEvents(ctx context.Context, botID string, limit int) ([]dao.NotifyEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []dao.NotifyEvent
	err := s.DB.WithContext(ctx).Where("bot_id = ?", botID).Order("created_at DESC").Limit(limit).Find(&rows).Error
	return rows, err
}
