package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/heartbeat"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// UserCronExecutor — 用户级定时任务的执行桥接
//
// 实现 cron.Executor 接口。当 cron 调度器判定某个 Job 到期时，把 Job.Prompt
// 作为一条「无人监督」的合成消息注入 bot 的真实编排链路（Engine.ProcessSync，
// 即 pipeline + dispatcher 全链路），产出的回复按 Job.Channel 投递到对应渠道。
//
// 与 heartbeat.Executor 同构（同样持有 TriggerRunner=*agent.Engine，经 SetRunner
// 在 bot 创建后注入）。此前用户 cron 任务（data/cron/<botID>_cron.json）仅经 HTTP
// API 做 CRUD，从无运行中调度器消费 → 定时任务永不触发（修复 5300）。
// ============================================================================

// UserCronExecutor 桥接 cron 调度器与 bot 真实编排。
type UserCronExecutor struct {
	botID  string
	logger *zap.SugaredLogger

	// runner 真实编排入口（*agent.Engine），由 StartBot 在 bot 创建后注入。
	// 用读写锁保护，因为 Scheduler 在 bot.Run 之前启动，而 runner 在之后才注入。
	mu     sync.RWMutex
	runner heartbeat.TriggerRunner
}

// NewUserCronExecutor 创建用户级 cron 执行器。
func NewUserCronExecutor(botID string, logger *zap.SugaredLogger) *UserCronExecutor {
	return &UserCronExecutor{
		botID:  botID,
		logger: logger.With("component", "user_cron_executor"),
	}
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

// Execute 实现 cron.Executor 接口：把 Job.Prompt 注入 bot 流水线跑一次。
func (e *UserCronExecutor) Execute(ctx context.Context, job *cron.Job) (result *cron.ExecuteResult, err error) {
	// 兜底：单个 job 执行异常（如空 Channel 导致 dispatch panic、LLM 报错）不应
	// 拖垮整个 bot 的调度循环（scheduler.executeJob 自身不 recover）。
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

	now := time.Now()
	traceID := traceid.New()

	// Job.Prompt 必须是自包含的（cron 工具已强制），此处原样作为消息正文。
	// Channel 为空时表示「bot 默认渠道」——交由 pipeline/dispatcher 决定路由；
	// 若 Job 配置了 Channel（如 misskey/telegram），回复投递到该渠道。
	// 注：Job.Skills 暂未接入自主执行链路（prompt 自包含即可），留待后续增强。
	msg := core.Message{
		ID:        fmt.Sprintf("cron-%d", now.UnixMilli()),
		BotID:     e.botID,
		TraceID:   traceID,
		Source:    core.SourceCron,
		Channel:   job.Channel,
		UserID:    "system:cron",
		Text:      job.Prompt,
		Mentioned: false,
		CreatedAt: now,
	}
	env := core.NewEnvelope(msg)

	if _, _, err := runner.ProcessSync(ctx, env); err != nil {
		e.logger.Errorw("user cron job execution failed",
			"bot_id", e.botID, "job_id", job.ID, "name", job.Name,
			"channel", job.Channel, "err", err, "trace_id", traceID)
		return nil, err
	}

	e.logger.Infow("user cron job executed",
		"bot_id", e.botID, "job_id", job.ID, "name", job.Name,
		"channel", job.Channel, "trace_id", traceID)

	return &cron.ExecuteResult{
		Output: fmt.Sprintf("cron job %q executed", job.Name),
	}, nil
}
