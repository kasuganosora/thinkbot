package heartbeat

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Heartbeat — Per-Bot 心跳自省模块
//
// 核心职责：
//   - 定时调用 LLM 评估 Bot 当前状态（连接、消息积压、异常等）
//   - 产出 normal / alert 两种状态
//   - 持久化心跳日志供前端查看
//
// 设计：
//   - 完全复用 cron.Scheduler + cron.Executor 模式（与 Dreaming 同构）
//   - 配置和日志使用文件系统存储（data/heartbeat/{botId}/）
//   - Bundle 模式：HeartbeatBundle 封装 Executor + Scheduler + Store
// ============================================================================

// Config 心跳配置。
type Config struct {
	// Enabled 是否启用心跳。
	Enabled bool `json:"enabled"`
	// Interval 心跳间隔（分钟）。范围 1-1440，默认 30。
	Interval int `json:"interval"`
}

// DefaultConfig 返回默认心跳配置。
func DefaultConfig() Config {
	return Config{
		Enabled:  true,
		Interval: 30,
	}
}

// Log 单条心跳日志。
type Log struct {
	ID     string  `json:"id"`     // "hb-{unixMilli}"
	Status string  `json:"status"` // "normal" | "alert"
	Time   string  `json:"time"`   // "2026/7/1 14:30:00"
	Cost   float64 `json:"cost"`   // 执行耗时（秒）
	Result string  `json:"result"` // LLM 输出的状态描述
}

// ContextProvider 为心跳执行器提供上下文信息的接口。
// Bot 层实现此接口，注入当前运行时状态。
type ContextProvider interface {
	// RecentMessageCount 返回最近 N 分钟内的消息数。
	RecentMessageCount(ctx context.Context, minutes int) int
	// ChannelStatus 返回各 Channel 的连接状态描述。
	ChannelStatus(ctx context.Context) string
	// PendingMessageCount 返回未处理的消息数。
	PendingMessageCount(ctx context.Context) int
	// BotName 返回 Bot 显示名。
	BotName() string
}

// noopContextProvider 无操作的上下文提供者（用于 ContextProvider 为 nil 时）。
type noopContextProvider struct {
	botName string
}

func (n *noopContextProvider) RecentMessageCount(_ context.Context, _ int) int { return 0 }
func (n *noopContextProvider) ChannelStatus(_ context.Context) string          { return "unknown" }
func (n *noopContextProvider) PendingMessageCount(_ context.Context) int       { return 0 }
func (n *noopContextProvider) BotName() string                                 { return n.botName }

// Executor 实现 cron.Executor 接口，桥接 cron 调度器和心跳推理逻辑。
type Executor struct {
	provider llm.Provider
	model    string
	botID    string
	store    *Store
	ctxProv  ContextProvider
	location *time.Location
	logger   *zap.SugaredLogger
}

// ExecutorConfig 创建 Executor 的参数。
type ExecutorConfig struct {
	Provider        llm.Provider
	Model           string
	BotID           string
	Store           *Store
	ContextProvider ContextProvider
	Location        *time.Location
	Logger          *zap.SugaredLogger
}

// NewExecutor 创建心跳执行器。
func NewExecutor(cfg ExecutorConfig) *Executor {
	ctxProv := cfg.ContextProvider
	if ctxProv == nil {
		ctxProv = &noopContextProvider{botName: cfg.BotID}
	}
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	return &Executor{
		provider: cfg.Provider,
		model:    cfg.Model,
		botID:    cfg.BotID,
		store:    cfg.Store,
		ctxProv:  ctxProv,
		location: loc,
		logger:   cfg.Logger.With("component", "heartbeat_executor", "bot_id", cfg.BotID),
	}
}

// Execute 实现 cron.Executor 接口。
// 收集上下文 → LLM 推理 → 判定状态 → 写入日志。
func (e *Executor) Execute(ctx context.Context, _ *cron.Job) (*cron.ExecuteResult, error) {
	start := time.Now()

	// 读取配置获取 interval
	cfg, _ := e.store.LoadConfig(e.botID)
	interval := 30
	if cfg != nil && cfg.Interval > 0 {
		interval = cfg.Interval
	}

	// 1. 收集上下文
	now := time.Now().In(e.location)
	recentCount := e.ctxProv.RecentMessageCount(ctx, interval)
	channelStatus := e.ctxProv.ChannelStatus(ctx)
	pendingCount := e.ctxProv.PendingMessageCount(ctx)
	botName := e.ctxProv.BotName()

	// 获取上次心跳信息
	lastBeat := "无记录"
	logs, _ := e.store.LoadLogs(e.botID)
	if logs != nil && len(logs.Logs) > 0 {
		lastBeat = logs.Logs[0].Time + " (" + logs.Logs[0].Status + ")"
	}

	// 2. 构建 prompt
	prompt := fmt.Sprintf(
		"当前时间：%s\n距上次心跳：%s\n最近 %d 分钟消息数：%d\nChannel 状态：%s\n未处理消息：%d",
		now.Format("2006/1/2 15:04:05"),
		lastBeat,
		interval,
		recentCount,
		channelStatus,
		pendingCount,
	)

	system := fmt.Sprintf(heartbeatSystemPrompt, botName)

	maxTokens := 256
	result, err := e.provider.DoGenerate(
		llm.WithStatsFeature(ctx, "heartbeat"),
		llm.GenerateParams{
			Model:     llm.ChatModel(e.model),
			System:    system,
			Messages:  []llm.Message{llm.UserMessage(prompt)},
			MaxTokens: &maxTokens,
		},
	)
	if err != nil {
		// LLM 失败时记录为 alert
		cost := time.Since(start).Seconds()
		entry := Log{
			ID:     fmt.Sprintf("hb-%d", time.Now().UnixMilli()),
			Status: "alert",
			Time:   now.Format("2006/1/2 15:04:05"),
			Cost:   cost,
			Result: fmt.Sprintf("心跳检查失败: %v", err),
		}
		e.store.AppendLog(e.botID, entry)
		e.logger.Warnw("heartbeat LLM call failed", "err", err)
		return nil, err
	}

	// 3. 判定状态
	status := "normal"
	text := result.Text
	if len(text) > 6 && text[:6] == "ALERT:" {
		status = "alert"
		text = text[6:]
	} else if len(text) > 7 && text[:7] == "ALERT: " {
		status = "alert"
		text = text[7:]
	}

	// 4. 写入日志
	cost := time.Since(start).Seconds()
	entry := Log{
		ID:     fmt.Sprintf("hb-%d", time.Now().UnixMilli()),
		Status: status,
		Time:   now.Format("2006/1/2 15:04:05"),
		Cost:   cost,
		Result: text,
	}
	e.store.AppendLog(e.botID, entry)

	e.logger.Infow("heartbeat completed",
		"status", status,
		"cost_sec", cost,
		"recent_msgs", recentCount)

	return &cron.ExecuteResult{
		Output: fmt.Sprintf("[%s] %s", status, text),
		Usage:  result.Usage,
	}, nil
}

const heartbeatSystemPrompt = `你是 %s 的内省系统。你的任务是定期检查 Bot 的运行状态。

评估规则：
- 如果一切正常（channel 在线、无消息积压），简要描述当前状态
- 如果发现异常（channel 断连、消息积压严重、长时间无活动但有待处理任务），以 "ALERT:" 开头报告

注意：
- 回复控制在 80 字以内
- 使用自然语言，简洁明了
- 不需要格式化，纯文本即可`

// Bundle 封装心跳子系统的完整组件。
type Bundle struct {
	Executor  *Executor
	Scheduler *cron.Scheduler
	Store     *Store
	cronStore *cron.Store
}

// BundleConfig 创建 Bundle 的参数。
type BundleConfig struct {
	BotID           string
	Provider        llm.Provider
	Model           string
	Location        *time.Location
	Logger          *zap.SugaredLogger
	DataDir         string // 心跳数据根目录，默认 "data/heartbeat"
	ContextProvider ContextProvider
}

// NewBundle 创建心跳子系统 Bundle。
// 如果配置为 disabled，返回 nil。
// 返回的 Bundle 中 Scheduler 已注册 Job 但尚未 Start。
func NewBundle(cfg BundleConfig) *Bundle {
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "data/heartbeat"
	}

	store := NewStore(dataDir)

	// 加载配置，检查是否启用
	config, _ := store.LoadConfig(cfg.BotID)
	if config == nil {
		config = &Config{Enabled: true, Interval: 30}
		_ = store.SaveConfig(cfg.BotID, config)
	}
	if !config.Enabled {
		return nil
	}

	// 创建 Executor
	executor := NewExecutor(ExecutorConfig{
		Provider:        cfg.Provider,
		Model:           cfg.Model,
		BotID:           cfg.BotID,
		Store:           store,
		ContextProvider: cfg.ContextProvider,
		Location:        cfg.Location,
		Logger:          cfg.Logger,
	})

	// 创建 cron Store + Scheduler
	cronFilePath := store.CronFilePath(cfg.BotID)
	cronStore := cron.NewStore(cronFilePath)

	schedCfg := cron.DefaultSchedulerConfig()
	schedCfg.BotID = cfg.BotID
	schedCfg.Location = cfg.Location

	scheduler := cron.NewScheduler(cronStore, executor, schedCfg)

	// 注册 cron Job
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	mgr := cron.NewManager(cronStore, loc)

	schedule := fmt.Sprintf("every %dm", config.Interval)
	_, err := mgr.CreateJob(cron.CreateJobRequest{
		Name:     "heartbeat-" + cfg.BotID,
		Prompt:   "trigger heartbeat check",
		Schedule: schedule,
		Feature:  "heartbeat",
		Tags:     []string{"heartbeat", "monitoring"},
	})
	if err != nil {
		cfg.Logger.Errorw("failed to create heartbeat cron job", "err", err, "bot_id", cfg.BotID)
		return nil
	}

	cfg.Logger.Infow("heartbeat bundle created",
		"bot_id", cfg.BotID,
		"interval_min", config.Interval)

	return &Bundle{
		Executor:  executor,
		Scheduler: scheduler,
		Store:     store,
		cronStore: cronStore,
	}
}

// Start 启动心跳调度器。
func (b *Bundle) Start(ctx context.Context) {
	if b == nil {
		return
	}
	b.Scheduler.Start(ctx)
}

// Stop 停止心跳调度器。
func (b *Bundle) Stop() {
	if b == nil {
		return
	}
	b.Scheduler.Stop()
}
