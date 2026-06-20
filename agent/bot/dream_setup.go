package bot

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// DreamExecutor — 将 cron.Scheduler 与 memory.DreamManager 桥接
//
// 实现 cron.Executor 接口。当 cron Job 触发时，调用 DreamManager.Run()
// 执行三相位梦境巩固管线。
// ============================================================================

// DreamExecutor 桥接 cron 调度器和 DreamManager。
type DreamExecutor struct {
	dreamManager *memory.DreamManager
	logger       *zap.SugaredLogger
}

// NewDreamExecutor 创建梦境执行器。
func NewDreamExecutor(dreamManager *memory.DreamManager, logger *zap.SugaredLogger) *DreamExecutor {
	return &DreamExecutor{
		dreamManager: dreamManager,
		logger:       logger.With("component", "dream_executor"),
	}
}

// Execute 实现 cron.Executor 接口。
func (e *DreamExecutor) Execute(ctx context.Context, _ *cron.Job) (*cron.ExecuteResult, error) {
	report, err := e.dreamManager.Run(ctx)
	if err != nil {
		e.logger.Errorw("dream execution failed", "err", err)
		return nil, err
	}

	output := fmt.Sprintf("dream complete: ingested=%d promoted=%d themes=%d",
		report.LightIngested, report.DeepPromoted, report.REMThemes)

	e.logger.Infow("dream execution completed",
		"ingested", report.LightIngested,
		"promoted", report.DeepPromoted,
		"duration", report.Duration())

	return &cron.ExecuteResult{
		Output: output,
	}, nil
}

// DreamingBundle 封装梦境巩固子系统的完整组件。
type DreamingBundle struct {
	Manager   *memory.DreamManager
	Executor  *DreamExecutor
	Scheduler *cron.Scheduler
	CronStore *cron.Store
	CronJob   *cron.Job
	TieredMgr *memory.TieredManager
}

// NewDreamingBundle 为单个 Bot 创建完整的梦境巩固子系统。
//
// 参数：
//   - dreamCfg: 梦境配置（从 config.GetDreamingConfig 构建）
//   - provider: LLM 提供商（用于 Light 相位提取和画像验证）
//   - location: 时区（用于 cron 调度）
//   - tp: TracerProvider
//   - logger: 日志
//   - botID: Bot ID（用于日志和 cron Job 标识）
//   - cronFilePath: cron Job 的 JSON 持久化文件路径
//
// 返回的 bundle 中 Scheduler 已注册好 Job 但尚未 Start（由 Bot.Run 负责启动）。
// 如果 dreamCfg.Enabled 为 false，返回 nil。
func NewDreamingBundle(
	dreamCfg memory.DreamConfig,
	provider llm.Provider,
	location *time.Location,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
	botID string,
	cronFilePath string,
) *DreamingBundle {
	if !dreamCfg.Enabled {
		return nil
	}

	// 1. 创建分层记忆管理器
	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: true,
	}, tp, logger)

	// 2. 创建 DreamManager
	dreamMgr := memory.NewDreamManager(dreamCfg, tieredMgr, provider, tp, logger)

	// 3. 创建 cron Store + Executor + Scheduler
	cronStore := cron.NewStore(cronFilePath)
	executor := NewDreamExecutor(dreamMgr, logger)

	schedCfg := cron.DefaultSchedulerConfig()
	schedCfg.BotID = botID
	schedCfg.Location = location

	scheduler := cron.NewScheduler(cronStore, executor, schedCfg)

	// 4. 创建并注册 cron Job
	mgr := cron.NewManager(cronStore, location)

	job, err := mgr.CreateJob(cron.CreateJobRequest{
		Name:     "dreaming-" + botID,
		Prompt:   "trigger dreaming consolidation",
		Schedule: dreamCfg.Schedule,
		Feature:  "dreaming",
		Tags:     []string{"dreaming", "memory"},
	})
	if err != nil {
		logger.Errorw("failed to create dream cron job", "err", err)
		return nil
	}

	logger.Infow("dreaming bundle created",
		"bot_id", botID,
		"schedule", dreamCfg.Schedule,
		"job_id", job.ID)

	return &DreamingBundle{
		Manager:   dreamMgr,
		Executor:  executor,
		Scheduler: scheduler,
		CronStore: cronStore,
		CronJob:   job,
		TieredMgr: tieredMgr,
	}
}

// Stop 优雅关闭梦境巩固子系统。
func (b *DreamingBundle) Stop() {
	if b == nil {
		return
	}
	if b.Scheduler != nil {
		b.Scheduler.Stop()
	}
}
