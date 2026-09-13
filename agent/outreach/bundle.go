package outreach

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/cron"
)

// Bundle 封装主动开口子系统。
type Bundle struct {
	Executor  *Executor
	Scheduler *cron.Scheduler
	CfgStore  *ConfigStore
	Repo      *Repo
}

// BundleConfig 创建参数。
type BundleConfig struct {
	BotID     string
	Repo      *Repo
	CfgStore  *ConfigStore
	DataDir   string
	Location  *time.Location
	Logger    *zap.SugaredLogger
	AllowPost AllowPostFn
	Fallback  FallbackSender
	Runner    TriggerRunner
	Now       func() time.Time
}

// NewBundle 创建 Bundle。Scheduler 已注册 Job 但尚未 Start。
// 总开关关闭时仍创建（每 tick 读配置，可热开），与心跳「Enabled=false 则 nil」不同。
func NewBundle(cfg BundleConfig) *Bundle {
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop().Sugar()
	}
	store := cfg.CfgStore
	if store == nil {
		store = NewConfigStore(cfg.DataDir)
	}
	config, err := store.Load(cfg.BotID)
	if err != nil {
		cfg.Logger.Warnw("outreach: load config failed, using defaults", "err", err, "bot_id", cfg.BotID)
		config = DefaultConfig()
	}
	if config.IntervalMin <= 0 {
		config.IntervalMin = 30
	}

	executor := NewExecutor(ExecutorConfig{
		BotID:     cfg.BotID,
		CfgStore:  store,
		Repo:      cfg.Repo,
		Location:  cfg.Location,
		Logger:    cfg.Logger,
		AllowPost: cfg.AllowPost,
		Fallback:  cfg.Fallback,
		Runner:    cfg.Runner,
		Now:       cfg.Now,
	})

	cronFile := store.CronFilePath(cfg.BotID)
	cronStore := cron.NewStore(cronFile)
	schedCfg := cron.DefaultSchedulerConfig()
	schedCfg.BotID = cfg.BotID
	schedCfg.Location = cfg.Location
	schedCfg.Name = "outreach"

	scheduler := cron.NewScheduler(cronStore, executor, schedCfg)

	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	mgr := cron.NewManager(cronStore, loc)
	jobName := "outreach-" + cfg.BotID
	for _, existing := range mgr.ListJobs() {
		if existing.Name == jobName {
			if derr := mgr.DeleteJob(existing.ID); derr != nil {
				cfg.Logger.Warnw("outreach: failed to prune stale cron job", "job_id", existing.ID, "err", derr)
			}
		}
	}
	if _, err := mgr.CreateJob(cron.CreateJobRequest{
		Name:     jobName,
		Prompt:   "evaluate outreach commitments",
		Schedule: fmt.Sprintf("every %dm", config.IntervalMin),
		Feature:  "outreach",
		Tags:     []string{"outreach", "proactive"},
	}); err != nil {
		cfg.Logger.Errorw("failed to create outreach cron job", "err", err, "bot_id", cfg.BotID)
		return nil
	}

	cfg.Logger.Infow("outreach bundle created",
		"bot_id", cfg.BotID,
		"interval_min", config.IntervalMin,
		"enabled", config.Enabled)

	return &Bundle{
		Executor:  executor,
		Scheduler: scheduler,
		CfgStore:  store,
		Repo:      cfg.Repo,
	}
}

// Start 启动调度器。
func (b *Bundle) Start(ctx context.Context) {
	if b == nil {
		return
	}
	b.Scheduler.Start(ctx)
}

// Stop 停止调度器。
func (b *Bundle) Stop() {
	if b == nil {
		return
	}
	b.Scheduler.Stop()
}

// SetRunner 注入真实编排入口。
func (b *Bundle) SetRunner(r TriggerRunner) {
	if b == nil || b.Executor == nil {
		return
	}
	b.Executor.SetRunner(r)
}

// NotifyInbound 转发到 Executor。
func (b *Bundle) NotifyInbound(ctx context.Context, userID, channelType string) {
	if b == nil || b.Executor == nil {
		return
	}
	b.Executor.NotifyInbound(ctx, userID, channelType)
}
