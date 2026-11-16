package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/heartbeat"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// UserCronExecutor — 用户级定时任务的执行桥接
//
// 实现 cron.Executor 接口。当 cron 调度器判定某个 Job 到期时，把 Job.Prompt
// 作为一条「无人监督」的合成消息注入 bot 的真实编排链路（Engine.ProcessSync），
// 提取清洗后的正文并按 Job 的目标渠道（Web 会话 / Telegram / 企微 / Misskey / Mastodon）投递。
//
// 注意：SourceCron 不是已注册的真实 Channel（dispatcher 中没有名为 "cron" 的 Sender），
// 因此本执行器恒设硬抑制 KVSuppressReasonCron 阻止通用 dispatcher 自动回写，
// 改为从 llm.result 提取可投递文本后经 CronDeliverFunc 显式投递。
// ============================================================================

// CronDeliverFunc 将定时任务产出的清洗后正文投递到目标渠道/会话。
type CronDeliverFunc func(ctx context.Context, job *cron.Job, traceID, content string) error

// UserCronExecutor 桥接 cron 调度器与 bot 真实编排。
type UserCronExecutor struct {
	botID   string
	logger  *zap.SugaredLogger
	deliver CronDeliverFunc

	// runner 真实编排入口（*agent.Engine），由 StartBot 在 bot 创建后注入。
	// 用读写锁保护，因为 Scheduler 在 bot.Run 之前构造，而 runner 在之后注入。
	mu     sync.RWMutex
	runner heartbeat.TriggerRunner
}

// NewUserCronExecutor 创建用户级 cron 执行器。
func NewUserCronExecutor(botID string, logger *zap.SugaredLogger) *UserCronExecutor {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &UserCronExecutor{
		botID:  botID,
		logger: logger.With("component", "user_cron_executor"),
	}
}

// WithDeliverer 注入结果投递回调（可选；nil 时仅执行并把输出记录在 LastResult）。
func (e *UserCronExecutor) WithDeliverer(fn CronDeliverFunc) *UserCronExecutor {
	e.deliver = fn
	return e
}

// SetRunner 注入真实编排入口（bot 创建后调用，与 heartbeat 一致）。
func (e *UserCronExecutor) SetRunner(r heartbeat.TriggerRunner) {
	e.mu.Lock()
	e.runner = r
	e.mu.Unlock()
}

func (e *UserCronExecutor) getRunner() heartbeat.TriggerRunner {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.runner
}

// Execute 实现 cron.Executor 接口：把 Job.Prompt 注入 bot 流水线跑一次并按需投递产出。
func (e *UserCronExecutor) Execute(ctx context.Context, job *cron.Job) (result *cron.ExecuteResult, err error) {
	// 兜底：单个 job 执行异常不应拖垮整个 bot 的调度循环（scheduler.executeJob 自身不 recover）。
	defer func() {
		if r := recover(); r != nil {
			e.logger.Errorw("user cron job panicked",
				"bot_id", e.botID, "job_id", job.ID, "name", job.Name, "panic", r)
			err = fmt.Errorf("user cron job panicked: %v", r)
		}
	}()

	runner := e.getRunner()
	if runner == nil {
		return nil, fmt.Errorf("user cron executor: runner (engine) not wired for bot %q", e.botID)
	}

	ctx = cron.WithCronSession(ctx)
	now := time.Now()
	traceID := traceid.New()

	var meta map[string]any
	if strings.TrimSpace(job.ChannelType) != "" {
		meta = map[string]any{
			"channel_type": strings.TrimSpace(job.ChannelType),
		}
	}

	// Job.Prompt 必须是自包含的（cron 工具已强制），此处原样作为消息正文。
	// ChatType 设为 ChatPrivate，使 Scopes 限定为 private/group 的工具（memory/spawn/task）正常挂载；
	// Channel 使用按 job 隔离的命名空间，避免不同 cron 任务互相串扰上下文。
	msg := core.Message{
		ID:        fmt.Sprintf("cron-%s-%d", job.ID, now.UnixNano()),
		BotID:     e.botID,
		TraceID:   traceID,
		Source:    core.SourceCron,
		Channel:   "cron:" + job.ID,
		ChatType:  core.ChatPrivate,
		UserID:    "system:cron",
		Text:      job.Prompt,
		Mentioned: true,
		Metadata:  meta,
		CreatedAt: now,
	}
	env := core.NewEnvelope(msg)
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, core.KVSuppressReasonCron)

	outEnv, actions, err := runner.ProcessSync(ctx, env)
	if err != nil {
		e.logger.Errorw("user cron job execution failed",
			"bot_id", e.botID, "job_id", job.ID, "name", job.Name,
			"channel", job.Channel, "err", err, "trace_id", traceID)
		return nil, err
	}

	outputText, usage, toolCalls, steps := extractCronTurnResult(outEnv, actions)

	if outputText != "" && e.deliver != nil && hasCronDeliveryTarget(job) {
		if dErr := e.deliver(ctx, job, traceID, outputText); dErr != nil {
			e.logger.Errorw("user cron job delivery failed",
				"bot_id", e.botID, "job_id", job.ID, "name", job.Name,
				"channel", job.Channel, "channel_type", job.ChannelType,
				"err", dErr, "trace_id", traceID)
			return nil, dErr
		}
	}

	e.logger.Infow("user cron job executed",
		"bot_id", e.botID, "job_id", job.ID, "name", job.Name,
		"channel", job.Channel, "output_len", len(outputText), "trace_id", traceID)

	summary := outputText
	if summary == "" {
		summary = fmt.Sprintf("cron job %q executed", job.Name)
	}
	return &cron.ExecuteResult{
		Output:    summary,
		Usage:     usage,
		ToolCalls: toolCalls,
		Steps:     steps,
	}, nil
}

func hasCronDeliveryTarget(job *cron.Job) bool {
	if job == nil {
		return false
	}
	return strings.TrimSpace(job.Channel) != "" ||
		strings.TrimSpace(job.ChannelType) != "" ||
		strings.TrimSpace(job.SessionID) != ""
}

func extractCronTurnResult(outEnv *core.Envelope, actions []core.Action) (output string, usage llm.Usage, toolCalls, steps int) {
	if outEnv != nil {
		if v, ok := outEnv.Get("llm.result"); ok {
			if genRes, ok := v.(*llm.GenerateResult); ok && genRes != nil {
				output = stages.ExtractDeliverableText(genRes.Text)
				usage = genRes.Usage
				steps = len(genRes.Steps)
				for _, st := range genRes.Steps {
					toolCalls += len(st.ToolCalls)
				}
				if toolCalls == 0 {
					toolCalls = len(genRes.ToolCalls)
				}
				if steps == 0 && (genRes.Text != "" || toolCalls > 0) {
					steps = 1
				}
			}
		}
	}
	if output == "" {
		for _, a := range actions {
			if a.Type == core.ActionReply {
				if s, ok := a.Payload.(string); ok && strings.TrimSpace(s) != "" {
					output = stages.ExtractDeliverableText(s)
					break
				}
			}
		}
	}
	return output, usage, toolCalls, steps
}

// cronChannelDeliverer 构造将定时任务结果投递到真实渠道的回调。
//   - Web 会话：通过 ChatHistoryService.UpsertAssistantByTrace 写入原聊天会话；
//   - Misskey / Mastodon：无 ConversationID 时发顶层时间线新帖，有 ConversationID 时回复对应帖子；
//   - Telegram / 企业微信等 Sender 渠道：按 ConversationID 发送 ActionReply。
//   - 若目标渠道在权限系统中被设为「只看不发」（ModeMute），跳过外部发送。
func (s *BotService) cronChannelDeliverer(botID string) CronDeliverFunc {
	return func(ctx context.Context, job *cron.Job, traceID, content string) error {
		if strings.TrimSpace(content) == "" || !hasCronDeliveryTarget(job) {
			return nil
		}

		chName := strings.TrimSpace(job.Channel)
		chType := strings.ToLower(strings.TrimSpace(job.ChannelType))
		convID := strings.TrimSpace(job.ConversationID)
		sessID := strings.TrimSpace(job.SessionID)

		// Web 聊天会话落库路径
		if chType == "web" || strings.HasPrefix(chName, "web-") || (chName == "" && sessID != "") {
			if s.permSvc != nil && s.permSvc.IsReadOnly(botID, "web") {
				s.logger.Infow("cron delivery skipped: web channel is muted", "bot_id", botID, "job_id", job.ID)
				return nil
			}
			if s.chatHistory == nil || sessID == "" {
				s.logger.Infow("cron web delivery skipped: no target chat session", "bot_id", botID, "job_id", job.ID)
				return nil
			}
			uid := strings.TrimSpace(job.UserID)
			if uid == "" {
				uid = "system:cron"
			}
			return s.chatHistory.UpsertAssistantByTrace(botID, uid, content, traceID, "", "", sessID, false)
		}

		s.mu.RLock()
		b := s.botInstances[botID]
		s.mu.RUnlock()
		if b == nil {
			return fmt.Errorf("cron deliver: bot %q is not running", botID)
		}

		// 支持形如 "tg:<chatID>" / "telegram:<chatID>" / "wecom:..." 的简写配置（Web 管理端手动填写场景）
		lookupType := chType
		if idx := strings.Index(chName, ":"); idx > 0 {
			prefix := strings.ToLower(chName[:idx])
			rest := strings.TrimSpace(chName[idx+1:])
			switch prefix {
			case "tg", "telegram":
				lookupType = "telegram"
				if convID == "" {
					convID = rest
				}
			case "wecom":
				lookupType = "wecom"
				if convID == "" {
					convID = chName
				}
			}
		}

		var chosen bot.Channel
		for _, ch := range b.Channels() {
			if ch.Name() == chName {
				chosen = ch
				break
			}
		}
		if chosen == nil {
			wantType := lookupType
			if wantType == "" {
				wantType = strings.ToLower(chName)
			}
			for _, ch := range b.Channels() {
				if strings.EqualFold(ch.Type(), wantType) {
					chosen = ch
					break
				}
			}
		}
		if chosen == nil {
			return fmt.Errorf("cron deliver: channel %q not found on bot %q", chName, botID)
		}

		if chType == "" {
			chType = strings.ToLower(chosen.Type())
		}
		if s.permSvc != nil && s.permSvc.IsReadOnly(botID, chType) {
			s.logger.Infow("cron delivery skipped: channel is muted",
				"bot_id", botID, "job_id", job.ID, "platform", chType)
			return nil
		}

		sender, ok := chosen.(bot.Sender)
		if !ok {
			return fmt.Errorf("cron deliver: channel %q is not a sender", chosen.Name())
		}

		switch chType {
		case "misskey", "mastodon":
			if convID == "" {
				tp, ok := chosen.(core.TimelinePoster)
				if !ok {
					return fmt.Errorf("cron deliver: channel %q does not support timeline posting", chosen.Name())
				}
				_, err := tp.PostTimeline(ctx, content, "home", "")
				return err
			}
			return sender.Send(ctx, core.Action{
				Type:    core.ActionReply,
				Channel: convID,
				Payload: content,
				Metadata: map[string]any{
					"source_channel": chosen.Name(),
					"visibility":     "home",
				},
			})
		default:
			if convID == "" {
				return fmt.Errorf("cron deliver: conversation_id is required for %s channel %q", chType, chosen.Name())
			}
			return sender.Send(ctx, core.Action{
				Type:    core.ActionReply,
				Channel: convID,
				Payload: content,
				Metadata: map[string]any{
					"source_channel": chosen.Name(),
				},
			})
		}
	}
}
