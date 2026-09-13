package outreach

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// TriggerRunner 与 heartbeat 同构：完整 pipeline + dispatcher。
type TriggerRunner interface {
	ProcessSync(ctx context.Context, env *core.Envelope) (*core.Envelope, []core.Action, error)
}

// AllowPostFn 某平台发言模式是否允许主动开口（通常接 toolperm.AllowProactivePost）。
type AllowPostFn func(platform string) bool

// FallbackSender 硬条件 pipeline 空输出时的模板投递。
type FallbackSender func(ctx context.Context, c dao.OutreachCommitment, content string) error

// Executor 实现 cron.Executor：评估 → 配额 → ProcessSync → 落库。
type Executor struct {
	botID     string
	cfgStore  *ConfigStore
	repo      *Repo
	location  *time.Location
	logger    *zap.SugaredLogger
	allowPost AllowPostFn
	fallback  FallbackSender
	nowFn     func() time.Time

	runnerMu sync.RWMutex
	runner   TriggerRunner
}

// ExecutorConfig 创建参数。
type ExecutorConfig struct {
	BotID     string
	CfgStore  *ConfigStore
	Repo      *Repo
	Location  *time.Location
	Logger    *zap.SugaredLogger
	AllowPost AllowPostFn
	Fallback  FallbackSender
	Runner    TriggerRunner
	Now       func() time.Time
}

// NewExecutor 创建执行器。
func NewExecutor(cfg ExecutorConfig) *Executor {
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Executor{
		botID:     cfg.BotID,
		cfgStore:  cfg.CfgStore,
		repo:      cfg.Repo,
		location:  loc,
		logger:    logger.With("component", "outreach_executor", "bot_id", cfg.BotID),
		allowPost: cfg.AllowPost,
		fallback:  cfg.Fallback,
		nowFn:     cfg.Now,
		runner:    cfg.Runner,
	}
}

// SetRunner 后置注入 Engine。
func (e *Executor) SetRunner(r TriggerRunner) {
	e.runnerMu.Lock()
	e.runner = r
	e.runnerMu.Unlock()
}

func (e *Executor) getRunner() TriggerRunner {
	e.runnerMu.RLock()
	defer e.runnerMu.RUnlock()
	return e.runner
}

// NotifyInbound 记录真实用户入站，刷新该平台静默窗。
func (e *Executor) NotifyInbound(ctx context.Context, userID, channelType string) {
	if e == nil || e.repo == nil || userID == "" || channelType == "" {
		return
	}
	key := e.repo.ResolveIdentityKey(ctx, channelType, userID)
	if err := e.repo.TouchInbound(ctx, e.botID, key, channelType, nowOr(e.nowFn).UTC()); err != nil {
		e.logger.Warnw("outreach: touch inbound failed", "err", err, "user_id", userID, "channel_type", channelType)
	}
}

// Execute 实现 cron.Executor。
func (e *Executor) Execute(ctx context.Context, _ *cron.Job) (*cron.ExecuteResult, error) {
	start := time.Now()
	now := nowOr(e.nowFn).In(e.location)
	nowUTC := now.UTC()

	cfg, err := e.cfgStore.Load(e.botID)
	if err != nil {
		return nil, fmt.Errorf("outreach: load config: %w", err)
	}

	if _, err := e.repo.ExpireStale(ctx, e.botID, nowUTC.Add(-ExpireAfter)); err != nil {
		e.logger.Warnw("outreach: expire stale failed", "err", err)
	}

	if !cfg.Enabled {
		// 总开关关闭不落 silent 对账，避免每 tick 一条噪声。
		return &cron.ExecuteResult{Output: "[silent] outreach disabled"}, nil
	}

	due, err := e.repo.ListDue(ctx, e.botID, nowUTC)
	if err != nil {
		return nil, fmt.Errorf("outreach: list due: %w", err)
	}
	if len(due) == 0 {
		e.mustWriteRecord(ctx, &dao.OutreachRecord{
			BotID:   e.botID,
			Status:  StatusSilent,
			Trigger: "none",
			Reason:  "nothing due",
			CostSec: time.Since(start).Seconds(),
		})
		return &cron.ExecuteResult{Output: "[silent] nothing due"}, nil
	}

	// 软条件跨承诺去重不靠内存 map：handleOne 里 CheckGate 读已落库的 sent 记录，
	// 同一 tick 顺序执行时第二条会 hit skipped_quota；跨进程靠日限 + singleinst。
	var sent, skipped int
	for i := range due {
		c := due[i]
		st, err := e.handleOne(ctx, cfg, c, now)
		if err != nil {
			e.logger.Warnw("outreach: handle commitment failed", "err", err, "id", c.ID)
			if st == "" {
				st = StatusError
			}
		}
		if st == StatusSent {
			sent++
		} else {
			skipped++
		}
	}

	return &cron.ExecuteResult{
		Output: fmt.Sprintf("[outreach] sent=%d skipped=%d due=%d cost=%.2fs",
			sent, skipped, len(due), time.Since(start).Seconds()),
	}, nil
}

func (e *Executor) handleOne(ctx context.Context, cfg Config, c dao.OutreachCommitment, now time.Time) (string, error) {
	start := time.Now()
	reason := ReasonReminder(c.Topic, c.Context)
	if !isHard(c.Kind) {
		reason = ReasonWatch(c.Topic, c.Context)
	}

	if already, err := e.repo.HasSentRecord(ctx, c.ID); err != nil {
		return "", err
	} else if already {
		if merr := e.repo.MarkDelivered(ctx, e.botID, c.ID, "", nowOr(e.nowFn).UTC()); merr != nil {
			e.logger.Warnw("outreach: mark already-sent commitment delivered", "err", merr, "id", c.ID)
		}
		return StatusSent, nil
	}

	if e.allowPost != nil && !e.allowPost(c.ChannelType) {
		return e.skip(ctx, c, StatusSkippedSpeakMode, reason, "speak mode not active", start)
	}

	gate, err := CheckGate(ctx, e.repo, cfg, c, now.UTC())
	if err != nil {
		return "", err
	}
	if !gate.Allowed {
		return e.skip(ctx, c, gate.Status, reason, gate.Detail, start)
	}

	if strings.TrimSpace(c.Channel) == "" {
		return e.skip(ctx, c, StatusSkippedNoTarget, reason, "missing channel", start)
	}

	runner := e.getRunner()
	if runner == nil {
		rec := e.baseRecord(c, reason, start)
		rec.Status = StatusError
		rec.Reason = reason + " (runner not wired)"
		return e.recordFailedAttempt(ctx, c, rec, fmt.Errorf("outreach: runner is nil"))
	}

	traceID := traceid.New()
	env := e.buildEnvelope(c, reason, now, traceID)
	ctx = WithOutreachSession(ctx)

	out, actions, err := runner.ProcessSync(ctx, env)
	content := firstReply(actions)
	if content == "" && out != nil {
		content = firstReply(out.Actions())
	}

	if err != nil || strings.TrimSpace(content) == "" {
		if isHard(c.Kind) {
			content = TemplateFallback(c.Topic)
			if e.fallback == nil {
				rec := e.baseRecord(c, reason, start)
				rec.Status = StatusError
				rec.TraceID = traceID
				rec.Reason = reason + " (no fallback sender)"
				return e.recordFailedAttempt(ctx, c, rec, fmt.Errorf("outreach: no fallback sender"))
			}
			if ferr := e.fallback(ctx, c, content); ferr != nil {
				rec := e.baseRecord(c, reason, start)
				rec.Status = StatusError
				rec.TraceID = traceID
				rec.Reason = reason + " (fallback send failed: " + ferr.Error() + ")"
				return e.recordFailedAttempt(ctx, c, rec, ferr)
			}
			return e.delivered(ctx, c, content, reason, traceID, start)
		}
		rec := e.baseRecord(c, reason, start)
		rec.Status = StatusError
		rec.TraceID = traceID
		if err != nil {
			rec.Reason = reason + " (pipeline failed: " + err.Error() + ")"
		} else {
			rec.Reason = reason + " (empty output, soft skip)"
		}
		return e.recordFailedAttempt(ctx, c, rec, err)
	}

	return e.delivered(ctx, c, content, reason, traceID, start)
}

func (e *Executor) skip(ctx context.Context, c dao.OutreachCommitment, status, reason, detail string, start time.Time) (string, error) {
	rec := e.baseRecord(c, reason, start)
	rec.Status = status
	if detail != "" {
		rec.Reason = reason + "（" + detail + "）"
	}
	if err := e.writeRecord(ctx, &rec); err != nil {
		return status, err
	}
	return status, nil
}

func (e *Executor) delivered(ctx context.Context, c dao.OutreachCommitment, content, reason, traceID string, start time.Time) (string, error) {
	rec := e.baseRecord(c, reason, start)
	rec.Status = StatusSent
	rec.Content = content
	rec.TraceID = traceID
	if err := e.writeRecord(ctx, &rec); err != nil {
		return StatusSent, err
	}
	if err := e.repo.MarkDelivered(ctx, e.botID, c.ID, rec.ID, nowOr(e.nowFn).UTC()); err != nil {
		e.logger.Errorw("outreach: sent but failed to mark delivered; will skip re-send if sent record exists",
			"err", err, "commitment_id", c.ID, "record_id", rec.ID)
		return StatusSent, err
	}
	return StatusSent, nil
}

func (e *Executor) baseRecord(c dao.OutreachCommitment, reason string, start time.Time) dao.OutreachRecord {
	trigger := c.Kind
	if trigger == "" {
		trigger = "none"
	}
	return dao.OutreachRecord{
		BotID:        e.botID,
		IdentityKey:  c.IdentityKey,
		CommitmentID: c.ID,
		Trigger:      trigger,
		Reason:       reason,
		ChannelType:  c.ChannelType,
		CostSec:      time.Since(start).Seconds(),
	}
}

func (e *Executor) recordFailedAttempt(ctx context.Context, c dao.OutreachCommitment, rec dao.OutreachRecord, cause error) (string, error) {
	n, err := e.repo.BumpAttempts(ctx, e.botID, c.ID)
	if err != nil {
		e.logger.Warnw("outreach: bump attempts failed", "err", err, "id", c.ID)
	}
	if n > 0 {
		rec.Reason = fmt.Sprintf("%s (attempt %d/%d)", rec.Reason, n, MaxAttempts)
	}
	e.mustWriteRecord(ctx, &rec)
	if n >= MaxAttempts {
		if ferr := e.repo.MarkFailed(ctx, e.botID, c.ID); ferr != nil {
			e.logger.Warnw("outreach: mark failed", "err", ferr, "id", c.ID)
		}
	}
	return StatusError, cause
}

func (e *Executor) writeRecord(ctx context.Context, rec *dao.OutreachRecord) error {
	if rec.ID == "" {
		rec.ID = idgen.New("or")
	}
	if err := e.repo.InsertRecord(ctx, rec); err != nil {
		e.logger.Warnw("outreach: persist record failed", "err", err, "status", rec.Status)
		return err
	}
	return nil
}

func (e *Executor) mustWriteRecord(ctx context.Context, rec *dao.OutreachRecord) {
	if err := e.writeRecord(ctx, rec); err != nil {
		e.logger.Warnw("outreach: persist record failed", "err", err, "status", rec.Status)
	}
}

func (e *Executor) buildEnvelope(c dao.OutreachCommitment, reason string, now time.Time, traceID string) *core.Envelope {
	meta := map[string]any{
		core.MetaOrigin:          core.SourceOutreach,
		"channel_type":           c.ChannelType,
		"chat_session_id":        c.SessionID,
		"session_id":             c.SessionID,
		"user_id":                c.UserID,
		"outreach_commitment_id": c.ID,
		"outreach_reason":        reason,
	}
	if c.ConversationID != "" {
		meta["reply_target"] = c.ConversationID
	}
	conv := c.ConversationID
	if conv == "" && c.ChannelType == "web" && c.UserID != "" {
		conv = "web:" + c.UserID
	}
	if conv == "" {
		conv = c.Channel
	}
	msg := core.Message{
		ID:            fmt.Sprintf("ou-%d", now.UnixMilli()),
		BotID:         e.botID,
		TraceID:       traceID,
		Source:        c.Channel, // 真实渠道实例名，dispatcher 才能找到 Sender
		Channel:       conv,
		ChatType:      core.ChatPrivate,
		UserID:        c.UserID,
		Text:          "",
		InjectContext: buildOutreachPrompt(c),
		Mentioned:     true, // 避免 engagement / passive 把门当「未被 @」
		CreatedAt:     now,
		Metadata:      meta,
	}
	env := core.NewEnvelope(msg)
	env.Set(core.KVOutreachForceSend, true)
	return env
}

func firstReply(actions []core.Action) string {
	for _, a := range actions {
		if a.Type == core.ActionReply {
			if s, ok := a.Payload.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}
