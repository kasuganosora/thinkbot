package cron

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Scheduler — 定时任务调度器
//
// 核心职责：
//   - 每 tick（默认 60s）扫描所有活跃 Job
//   - NextRunAt 到期的 Job → 调用 Executor 执行
//   - 执行完成后计算下一次 NextRunAt（cron/interval）
//   - 一次性 Job（once）执行后标记 Done
//   - 并发控制：同时执行的 Job 数量受 MaxConcurrent 限制
//
// 时区处理：
//   - 调度器初始化时接收 *time.Location（从 BotConfig.Location() 获取）
//   - 所有 cron 表达式和 ISO 时间戳在此时区内解析
//   - NextRunAt 以 UTC 存储，比较时转换
// ============================================================================

// ============================================================================
// 代码级反嵌套（anti-nesting）上下文标记 — 报告 5373
//
// 调度器在执行一个 cron Job 时，会把它触发的无人监督会话标记到 ctx 中。
// cron 工具在 action=create 时检查该标记：cron 触发的会话不得再创建新的
// cron 任务，从代码层面（而非仅提示词）强制反嵌套。
// ============================================================================

type cronSessionCtxKey struct{}

// WithCronSession 将「当前为 cron 调度的无人监督会话」标记注入 ctx。
func WithCronSession(ctx context.Context) context.Context {
	return context.WithValue(ctx, cronSessionCtxKey{}, true)
}

// IsCronSession 判断 ctx 是否来自 cron 调度的会话。
// 安全控制采用 fail-closed：上下文无效（nil 或无法取值）时按「是 cron 会话」
// 处理，阻止嵌套创建，而非放行。
func IsCronSession(ctx context.Context) (is bool) {
	defer func() {
		if r := recover(); r != nil {
			is = true
		}
	}()
	if ctx == nil {
		return true
	}
	v, ok := ctx.Value(cronSessionCtxKey{}).(bool)
	return ok && v
}

// maxConsecutiveFailures 是失败熔断阈值：达到该连续失败次数后，Job 转为
// StateFailed 不再重试（报告 5333）。
const maxConsecutiveFailures = 8

// computeFailureBackoff 计算第 N 次连续失败后的重试退避时长（指数退避 2^n）。
// 基础退避 1 分钟，封顶 1 小时，避免无限放大。
func computeFailureBackoff(failures int) time.Duration {
	base := time.Minute
	exp := failures - 1
	if exp < 0 {
		exp = 0
	}
	if exp > 6 {
		exp = 6
	}
	d := base * (1 << uint(exp)) // 1m, 2m, 4m, 8m, 16m, 32m, 64m
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// ExecuteResult 是 Job 执行的返回结果。
// 包含输出摘要和可选的 token 用量信息（用于统计）。
type ExecuteResult struct {
	// Output 执行输出的摘要文本。
	Output string

	// Usage 本次执行中 LLM 调用的 token 用量。
	// 为零值时表示无需记录统计。
	Usage llm.Usage

	// ToolCalls 执行过程中工具调用总次数。
	ToolCalls int

	// Steps 执行过程中的编排步数。
	Steps int
}

// Executor 是 Job 执行的抽象接口。
// 由调用方（如 Bot）注入实现，调度器不关心具体执行逻辑。
type Executor interface {
	// Execute 执行一个 Job。
	// ctx 用于超时/取消控制，已注入 trace_id。
	// 返回执行结果和可能的错误。
	Execute(ctx context.Context, job *Job) (*ExecuteResult, error)
}

// ExecutorFunc 是 Executor 的函数适配器。
type ExecutorFunc func(ctx context.Context, job *Job) (*ExecuteResult, error)

// Execute 实现 Executor 接口。
func (f ExecutorFunc) Execute(ctx context.Context, job *Job) (*ExecuteResult, error) {
	return f(ctx, job)
}

// SchedulerConfig 配置调度器行为。
type SchedulerConfig struct {
	// TickInterval 调度循环间隔（默认 60s）。
	// 越小越精确但 CPU 开销越大。
	TickInterval time.Duration

	// MaxConcurrent 同时执行的最大 Job 数（默认 3）。
	MaxConcurrent int

	// JobTimeout 单个 Job 执行超时（默认 5m）。
	JobTimeout time.Duration

	// OnceGracePeriod 一次性 Job 的宽限窗口（默认 120s）。
	// 创建后 NextRunAt 在过去 N 秒内仍会触发。
	OnceGracePeriod time.Duration

	// Location 时区。cron 表达式和 ISO 时间戳在此解析。
	// nil 表示 time.Local。
	Location *time.Location

	// BotID 关联的 Bot ID，用于 token 统计归属。
	// 为空时使用 "unknown"。
	BotID string
}

// DefaultSchedulerConfig 返回合理的默认配置。
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		TickInterval:    60 * time.Second,
		MaxConcurrent:   3,
		JobTimeout:      5 * time.Minute,
		OnceGracePeriod: 120 * time.Second,
		Location:        time.Local,
	}
}

// Scheduler 是定时任务调度器。
type Scheduler struct {
	store    *Store
	executor Executor
	config   SchedulerConfig
	logger   *zap.SugaredLogger
	recorder llm.UsageRecorder // 可选，nil 时不记录统计

	mu          sync.Mutex
	wg          sync.WaitGroup
	sem         chan struct{}
	stopCh      chan struct{}
	stopped     bool
	runningJobs map[string]bool // 正在执行的 job IDs
}

// NewScheduler 创建调度器。
func NewScheduler(store *Store, executor Executor, config SchedulerConfig) *Scheduler {
	if config.TickInterval <= 0 {
		config.TickInterval = 60 * time.Second
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 3
	}
	if config.JobTimeout <= 0 {
		config.JobTimeout = 5 * time.Minute
	}
	if config.OnceGracePeriod <= 0 {
		config.OnceGracePeriod = 120 * time.Second
	}
	if config.Location == nil {
		config.Location = time.Local
	}
	return &Scheduler{
		store:       store,
		executor:    executor,
		config:      config,
		logger:      log.Logger,
		stopCh:      make(chan struct{}),
		sem:         make(chan struct{}, config.MaxConcurrent),
		runningJobs: make(map[string]bool),
	}
}

// WithLogger 设置自定义 logger。
func (s *Scheduler) WithLogger(l *zap.SugaredLogger) *Scheduler {
	if l != nil {
		s.logger = l
	}
	return s
}

// WithUsageRecorder 设置 token 用量记录器。
// 设置后，每次 Job 执行产生 token 消耗时将自动记录到统计模块。
func (s *Scheduler) WithUsageRecorder(r llm.UsageRecorder) *Scheduler {
	s.recorder = r
	return s
}

// Start 启动调度循环（非阻塞）。
func (s *Scheduler) Start(ctx context.Context) {
	s.logger.Infow("cron: scheduler starting",
		"tick_interval", s.config.TickInterval.String(),
		"max_concurrent", s.config.MaxConcurrent,
		"job_timeout", s.config.JobTimeout.String(),
		"tz", s.config.Location.String(),
		"bot_id", s.config.BotID)
	s.wg.Go(func() {
		s.loop(ctx)
	})
}

// Stop 停止调度器并等待所有正在执行的 Job 完成。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stopCh)
	s.mu.Unlock()
	s.wg.Wait()
	s.logger.Infow("cron: scheduler stopped")
}

// loop 是主调度循环。
func (s *Scheduler) loop(ctx context.Context) {
	ticker := time.NewTicker(s.config.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick 执行一轮调度检查。
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	jobs := s.store.ListActive()

	for _, j := range jobs {
		// 检查是否到达执行时间
		if j.NextRunAt == nil {
			continue
		}
		nextRun := j.NextRunAt.UTC()

		// 一次性 Job 宽限窗口检查
		if j.ScheduleKind == ScheduleOnce {
			// 如果 NextRunAt 远在过去（超过宽限期），跳过
			if now.Sub(nextRun) > s.config.OnceGracePeriod {
				s.markDone(j, "skipped: one-shot job expired")
				continue
			}
		}

		// NextRunAt 在未来 → 跳过
		if nextRun.After(now) {
			continue
		}

		// 已在运行中 → 跳过（防止重叠）
		s.mu.Lock()
		if s.runningJobs[j.ID] {
			s.mu.Unlock()
			continue
		}
		s.runningJobs[j.ID] = true
		s.mu.Unlock()

		// 异步执行：sync.WaitGroup.Go 不 recover panic，必须自行兜底，
		// 否则任一 job（含心跳自主发帖）panic 会击穿整个进程。
		s.wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Errorw("job panicked, recovered to protect process",
						"job_id", j.ID, "panic", r)
				}
			}()
			s.executeJob(ctx, j)
		})
	}
}

// executeJob 执行单个 Job。
func (s *Scheduler) executeJob(ctx context.Context, job *Job) {
	defer func() {
		s.mu.Lock()
		delete(s.runningJobs, job.ID)
		s.mu.Unlock()
	}()

	// 获取 semaphore
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-s.stopCh:
		return
	case <-ctx.Done():
		return
	}

	// 读取最新状态
	latest, ok := s.store.Get(job.ID)
	if !ok || !latest.Enabled {
		return
	}

	// 为每次执行生成独立 trace_id，注入 context，并标记这是一个
	// cron 调度的无人监督会话（供 cron 工具做代码级反嵌套检查，报告 5373）
	execCtx := WithCronSession(traceid.NewContext(ctx))
	logger := traceid.L(execCtx)

	// 执行超时控制
	jobCtx, cancel := context.WithTimeout(execCtx, s.config.JobTimeout)
	defer cancel()

	logger.Infow("cron: job executing",
		"job_id", job.ID,
		"job_name", job.Name,
		"schedule", job.Schedule,
		"run_count", latest.RunCount+1,
		"tz", s.config.Location.String())

	startTime := time.Now()
	result, err := s.executor.Execute(jobCtx, latest)
	duration := time.Since(startTime)

	now := time.Now().UTC()
	latest.LastRunAt = &now
	latest.RunCount++

	if err != nil {
		latest.LastError = err.Error()
		latest.LastResult = ""
		latest.ConsecutiveFailures++

		logger.Errorw("cron: job failed",
			"job_id", job.ID,
			"job_name", job.Name,
			"duration", duration.String(),
			"err", err,
			"consecutive_failures", latest.ConsecutiveFailures)

		// 一次性任务失败不重试（报告 5333 修正）：直接转 failed，
		// 避免带副作用的任务（已发帖但回写失败）被重复执行。
		if latest.ScheduleKind == ScheduleOnce {
			latest.State = StateFailed
			latest.NextRunAt = nil
			latest.NextRetryAt = nil
			latest.LastResult = "failed: one-shot job error; not retried"
		} else if latest.MaxRuns > 0 && latest.RunCount >= latest.MaxRuns {
			latest.State = StateFailed
			latest.NextRunAt = nil
			latest.NextRetryAt = nil
		} else if latest.ConsecutiveFailures >= maxConsecutiveFailures {
			// 连续失败达到熔断阈值：停止重试，转 failed 状态等待人工干预
			latest.State = StateFailed
			latest.NextRunAt = nil
			latest.NextRetryAt = nil
			latest.LastResult = "failed: exceeded max consecutive failures; manual intervention required"
			logger.Errorw("cron: job circuit-broken after consecutive failures",
				"job_id", job.ID, "job_name", job.Name,
				"consecutive_failures", latest.ConsecutiveFailures)
		} else {
			// 指数退避：以 2^N 递增重试间隔，避免失败任务紧循环刷爆
			backoff := computeFailureBackoff(latest.ConsecutiveFailures)
			nr := now.Add(backoff)
			latest.NextRunAt = &nr
			latest.NextRetryAt = &nr
		}
	} else {
		output := ""
		if result != nil {
			output = strutil.Truncate(result.Output, 500)
		}
		latest.LastResult = output
		latest.LastError = ""
		latest.ConsecutiveFailures = 0 // 成功即重置连续失败计数

		logger.Infow("cron: job completed",
			"job_id", job.ID,
			"job_name", job.Name,
			"duration", duration.String(),
			"output_len", len(output))

		// 记录 token 用量到统计模块
		if s.recorder != nil && result != nil {
			usage := result.Usage
			if usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
				feature := job.Feature
				if feature == "" {
					feature = "cron"
				}
				botID := s.config.BotID
				if botID == "" {
					botID = "unknown"
				}
				s.recorder.RecordUsage(jobCtx, llm.UsageMetric{
					BotID:     botID,
					At:        time.Now(),
					Model:     job.Model,
					Feature:   feature,
					Usage:     usage,
					ToolCalls: result.ToolCalls,
					Steps:     result.Steps,
				})
			}
		}

		// 检查是否完成
		if latest.MaxRuns > 0 && latest.RunCount >= latest.MaxRuns {
			latest.State = StateDone
			latest.NextRunAt = nil
			latest.NextRetryAt = nil
		} else if latest.ScheduleKind == ScheduleOnce {
			// 一次性任务执行成功 → Done
			latest.State = StateDone
			latest.NextRunAt = nil
			latest.NextRetryAt = nil
		} else {
			s.computeNextRun(latest, now)
		}
	}

	// 重新读取磁盘/内存中最新版本，仅合并本执行产生的运行时字段，
	// 避免覆盖并发的 TriggerJob / UpdateJob 写入（读-改-写竞态，报告 5365）。
	cur, ok := s.store.Get(job.ID)
	if !ok {
		// 执行期间任务已被删除：不写回，避免已删除任务被复活。
		return
	}
	cur.RunCount = latest.RunCount
	cur.LastRunAt = latest.LastRunAt
	cur.LastResult = latest.LastResult
	cur.LastError = latest.LastError
	cur.State = latest.State
	cur.NextRunAt = latest.NextRunAt
	cur.NextRetryAt = latest.NextRetryAt
	cur.ConsecutiveFailures = latest.ConsecutiveFailures
	_ = s.store.Save(cur)
}

// markDone 将 Job 标记为完成状态并记录原因。
func (s *Scheduler) markDone(job *Job, reason string) {
	latest, ok := s.store.Get(job.ID)
	if !ok {
		return
	}
	latest.State = StateDone
	latest.NextRunAt = nil
	latest.LastResult = reason
	_ = s.store.Save(latest)
	s.logger.Infow("cron: job marked done",
		"job_id", job.ID,
		"job_name", job.Name,
		"reason", reason)
}

// computeNextRun 计算 Job 的下一次执行时间。
func (s *Scheduler) computeNextRun(job *Job, fromUTC time.Time) {
	loc := s.config.Location
	fromLocal := fromUTC.In(loc)

	switch job.ScheduleKind {
	case ScheduleInterval:
		dur, err := parseDuration(job.Schedule[6:]) // 去掉 "every " 前缀
		if err != nil {
			job.State = StateFailed
			job.NextRunAt = nil
			return
		}
		nr := fromLocal.Add(dur)
		job.NextRunAt = &nr

	case ScheduleCron:
		if job.cronExpr == nil {
			// 重新解析
			ce, err := parseCronExpr(job.Schedule)
			if err != nil {
				job.State = StateFailed
				job.NextRunAt = nil
				return
			}
			job.cronExpr = ce
		}
		nr := job.cronExpr.Next(fromLocal, loc)
		if nr.IsZero() {
			job.State = StateDone
			job.NextRunAt = nil
			return
		}
		job.NextRunAt = &nr

	case ScheduleOnce:
		// 一次性任务不需要计算下次
		job.State = StateDone
		job.NextRunAt = nil
	}
}

// ============================================================================
// Manager — Job 的 CRUD 管理接口
// ============================================================================

// Manager 提供 Job 的创建/查询/更新/删除 API。
type Manager struct {
	store  *Store
	loc    *time.Location
	logger *zap.SugaredLogger
}

// NewManager 创建 Manager。
// store 是持久化存储，loc 是时区（cron/ISO 解析使用）。
func NewManager(store *Store, loc *time.Location) *Manager {
	if loc == nil {
		loc = time.Local
	}
	return &Manager{store: store, loc: loc, logger: log.Logger}
}

// WithLogger 设置自定义 logger。
func (m *Manager) WithLogger(l *zap.SugaredLogger) *Manager {
	if l != nil {
		m.logger = l
	}
	return m
}

// CreateJobRequest 是创建 Job 的请求参数。
type CreateJobRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"` // 可选：任务描述
	Prompt      string   `json:"prompt"`
	Schedule    string   `json:"schedule"`
	Model       string   `json:"model,omitempty"`
	Channel     string   `json:"channel,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Feature     string   `json:"feature,omitempty"` // 统计维度标签（默认 "cron"）
	MaxRuns     int      `json:"max_runs,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// CreateJob 创建一个新 Job。
func (m *Manager) CreateJob(req CreateJobRequest) (*Job, error) {
	if req.Name == "" {
		return nil, errs.New("job name is required")
	}
	if req.Prompt == "" {
		return nil, errs.New("job prompt is required")
	}
	if req.Schedule == "" {
		return nil, errs.New("job schedule is required")
	}

	// 安全扫描：对所有创建路径（工具 + REST API）统一执行 prompt 注入/exfil 检测（报告 5381）
	if blocked := scanCronPrompt(req.Prompt); blocked != "" {
		return nil, errs.New(blocked)
	}

	kind, display, cronE, nextRun, err := parseSchedule(req.Schedule, m.loc)
	if err != nil {
		return nil, errs.Wrapf(err, "invalid schedule %q", req.Schedule)
	}

	now := time.Now()
	job := &Job{
		ID:              idgen.New("cron"),
		Name:            req.Name,
		Description:     req.Description,
		Prompt:          req.Prompt,
		Model:           req.Model,
		Channel:         req.Channel,
		Skills:          req.Skills,
		Feature:         req.Feature,
		Schedule:        req.Schedule,
		ScheduleKind:    kind,
		ScheduleDisplay: display,
		MaxRuns:         req.MaxRuns,
		Enabled:         true,
		State:           StateActive,
		NextRunAt:       nextRun,
		CreatedAt:       now,
		UpdatedAt:       now,
		Tags:            req.Tags,
		cronExpr:        cronE,
	}

	if err := m.store.Save(job); err != nil {
		return nil, errs.Wrap(err, "failed to save job")
	}
	m.logger.Infow("cron: job created",
		"job_id", job.ID,
		"job_name", job.Name,
		"schedule", job.Schedule,
		"schedule_kind", job.ScheduleKind)
	return job, nil
}

// GetJob 返回指定 ID 的 Job。
func (m *Manager) GetJob(id string) (*Job, bool) {
	return m.store.Get(id)
}

// ListJobs 返回所有 Job。
func (m *Manager) ListJobs() []*Job {
	return m.store.List()
}

// ListActiveJobs 返回所有活跃 Job。
func (m *Manager) ListActiveJobs() []*Job {
	return m.store.ListActive()
}

// UpdateJob 更新 Job 的可变字段。
func (m *Manager) UpdateJob(id string, updates map[string]any) (*Job, error) {
	job, ok := m.store.Get(id)
	if !ok {
		return nil, errs.Newf("job %q not found", id)
	}

	if name, ok := updates["name"].(string); ok {
		job.Name = name
	}
	if desc, ok := updates["description"].(string); ok {
		job.Description = desc
	}
	if prompt, ok := updates["prompt"].(string); ok {
		// 安全扫描：更新 prompt 时同样对所有路径统一执行检测（报告 5381）
		if blocked := scanCronPrompt(prompt); blocked != "" {
			return nil, errs.New(blocked)
		}
		job.Prompt = prompt
	}
	if model, ok := updates["model"].(string); ok {
		job.Model = model
	}
	if channel, ok := updates["channel"].(string); ok {
		job.Channel = channel
	}
	if feature, ok := updates["feature"].(string); ok {
		job.Feature = feature
	}
	if schedule, ok := updates["schedule"].(string); ok {
		kind, display, cronE, nextRun, err := parseSchedule(schedule, m.loc)
		if err != nil {
			return nil, errs.Wrapf(err, "invalid schedule %q", schedule)
		}
		job.Schedule = schedule
		job.ScheduleKind = kind
		job.ScheduleDisplay = display
		job.NextRunAt = nextRun
		job.cronExpr = cronE
		// 更新调度后重置状态
		if job.Enabled && (job.State == StateDone || job.State == StateFailed) {
			job.State = StateActive
		}
	}
	if maxRuns, ok := updates["max_runs"].(float64); ok {
		job.MaxRuns = int(maxRuns)
	}
	if enabled, ok := updates["enabled"].(bool); ok {
		job.Enabled = enabled
		if !enabled {
			job.State = StateDisabled
		} else if job.State == StateDisabled {
			job.State = StateActive
		}
	}

	if err := m.store.Save(job); err != nil {
		return nil, errs.Wrap(err, "failed to save job")
	}
	return job, nil
}

// DeleteJob 删除一个 Job。
func (m *Manager) DeleteJob(id string) error {
	if err := m.store.Delete(id); err != nil {
		return err
	}
	m.logger.Infow("cron: job deleted", "job_id", id)
	return nil
}

// PauseJob 暂停一个 Job。
func (m *Manager) PauseJob(id string) error {
	return m.setState(id, StatePaused)
}

// resumeFailureState 在任务被恢复/重新激活时清空失败计数与重试时间，
// 否则熔断后第一次失败会立即再次熔断，退避机制形同虚设（报告 5333）。
func resumeFailureState(job *Job) {
	job.ConsecutiveFailures = 0
	job.NextRetryAt = nil
}

// ResumeJob 恢复一个暂停的 Job 并重新计算下次执行时间。
func (m *Manager) ResumeJob(id string) error {
	job, ok := m.store.Get(id)
	if !ok {
		return errs.Newf("job %q not found", id)
	}
	if job.State != StatePaused {
		return errs.Newf("job %q is not paused", id)
	}
	// 重新计算 NextRunAt
	now := time.Now().UTC()
	fromLocal := now.In(m.loc)
	switch job.ScheduleKind {
	case ScheduleInterval:
		if dur, err := parseDuration(job.Schedule[6:]); err == nil {
			nr := fromLocal.Add(dur)
			job.NextRunAt = &nr
		}
	case ScheduleCron:
		if ce, err := parseCronExpr(job.Schedule); err == nil {
			if nr := ce.Next(fromLocal, m.loc); !nr.IsZero() {
				job.NextRunAt = &nr
			}
		}
	}
	job.State = StateActive
	resumeFailureState(job)
	return m.store.Save(job)
}

// TriggerJob 手动触发一个 Job（忽略 NextRunAt，立即执行）。
// 注意：实际执行需要通过 Scheduler 的 Executor。
// 此方法仅将 NextRunAt 设为现在。
func (m *Manager) TriggerJob(id string) error {
	job, ok := m.store.Get(id)
	if !ok {
		return errs.Newf("job %q not found", id)
	}
	now := time.Now().UTC()
	job.NextRunAt = &now
	if job.State == StateDone || job.State == StateFailed {
		job.State = StateActive
		resumeFailureState(job)
	}
	return m.store.Save(job)
}

func (m *Manager) setState(id string, state JobState) error {
	job, ok := m.store.Get(id)
	if !ok {
		return errs.Newf("job %q not found", id)
	}
	job.State = state
	if state == StateActive {
		resumeFailureState(job)
	}
	return m.store.Save(job)
}

// Summary 返回调度器的可读摘要。
func (s *Scheduler) Summary() string {
	jobs := s.store.List()
	active := 0
	done := 0
	failed := 0
	paused := 0
	for _, j := range jobs {
		switch j.State {
		case StateActive:
			active++
		case StateDone:
			done++
		case StateFailed:
			failed++
		case StatePaused:
			paused++
		}
	}
	return fmt.Sprintf("Scheduler(jobs=%d, active=%d, done=%d, failed=%d, paused=%d, tz=%s)",
		len(jobs), active, done, failed, paused, s.config.Location.String())
}
