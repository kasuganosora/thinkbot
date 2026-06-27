package pipeline

import (
	"context"
	"sync"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// RunJournalRecorder — LLM 调用事件持久化记录器
//
// 借鉴 deer-flow 的 RunJournal + RunEventStore 设计：
//   - 每次 LLM 调用完成后，将关键维度写入数据库
//   - 批量写入（flush_threshold 控制）以减少 DB 压力
//   - 记录维度：trace_id, bot_id, model, token 用量, tool_calls 数, 耗时
//
// 同时实现两个接口：
//   - llm.UsageRecorder：LLMStage 在调用完成后自动调用 RecordUsage
//   - pipeline.Middleware：在每次 Pipeline 执行时提取上下文，并通过钩子自动记录
//
// 使用方式：
//
//	journal := NewRunJournalRecorder(db, RunJournalConfig{
//	    FlushThreshold: 20,
//	    Caller:         "lead_agent",
//	})
//
//	// 作为 UsageRecorder 注入 LLMStage
//	stage := stages.NewLLMStage(..., LLMConfig{UsageRecorder: journal})
//
//	// 同时作为 Middleware 包装 LLMStage，提取上下文
//	guarded := journal.Middleware()(stage)
// ============================================================================

// RunJournalConfig 配置 RunJournal 记录器。
type RunJournalConfig struct {
	// FlushThreshold 批量写入阈值。达到此数量后执行一次 FLUSH。默认 20。
	FlushThreshold int
	// Caller 调用方标识（"lead_agent" / "subagent" / "middleware"）。默认 "lead_agent"。
	Caller string
	// Feature 功能标识（"reply" / "chat" / "vision" / "memory"）。默认 ""。
	Feature string
}

// DefaultRunJournalConfig 返回默认配置。
func DefaultRunJournalConfig() RunJournalConfig {
	return RunJournalConfig{
		FlushThreshold: 20,
		Caller:         "lead_agent",
	}
}

// RunJournalRecorder 是 RunJournal 的核心记录器。
type RunJournalRecorder struct {
	db           *gorm.DB
	cfg          RunJournalConfig
	mu           sync.Mutex
	buffer       []dao.RunJournal
	flushCh      chan struct{}
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

// NewRunJournalRecorder 创建 RunJournal 记录器。
// db 为 nil 时跳过持久化（NoOp 模式）。
func NewRunJournalRecorder(db *gorm.DB, cfg RunJournalConfig) *RunJournalRecorder {
	if cfg.FlushThreshold <= 0 {
		cfg.FlushThreshold = 20
	}
	if cfg.Caller == "" {
		cfg.Caller = "lead_agent"
	}

	r := &RunJournalRecorder{
		db:         db,
		cfg:        cfg,
		buffer:     make([]dao.RunJournal, 0, cfg.FlushThreshold),
		flushCh:    make(chan struct{}, 1),
		shutdownCh: make(chan struct{}),
	}
	return r
}

// RecordUsage 实现 llm.UsageRecorder 接口。
// LLMStage 在每次 orchestration 完成后自动调用。
// 从 context 读取 Middleware 注入的 trace_id/message_id/channel/user_id，
// 确保 RunJournal 记录包含完整的追踪维度。
func (r *RunJournalRecorder) RecordUsage(ctx context.Context, metric llm.UsageMetric) {
	if r.db == nil {
		return
	}

	traceID, messageID, channel, userID := r.extractContextMeta(ctx)
	feature := metric.Feature
	if feature == "" {
		feature = r.cfg.Feature
	}

	record := dao.RunJournal{
		TraceID:         traceID,
		RunID:           traceID,
		BotID:           metric.BotID,
		Channel:         channel,
		UserID:          userID,
		MessageID:       messageID,
		Model:           metric.Model,
		Feature:         feature,
		Caller:          r.cfg.Caller,
		InputTokens:     metric.Usage.InputTokens,
		OutputTokens:    metric.Usage.OutputTokens,
		TotalTokens:     metric.Usage.TotalTokens,
		CacheReadTokens: metric.Usage.CachedInputTokens,
		ToolCalls:       metric.ToolCalls,
		Steps:           metric.Steps,
		Status:          "success",
	}

	r.mu.Lock()
	r.buffer = append(r.buffer, record)
	needFlush := len(r.buffer) >= r.cfg.FlushThreshold
	r.mu.Unlock()

	if needFlush {
		select {
		case r.flushCh <- struct{}{}:
		default:
		}
	}
}

// journalCtxKey is the context key type for journal metadata injected by Middleware.
type journalCtxKey struct{}

type journalMeta struct {
	TraceID   string
	MessageID string
	Channel   string
	UserID    string
}

// extractContextMeta reads journal metadata from context injected by Middleware.
// Returns empty strings if no metadata is present.
func (r *RunJournalRecorder) extractContextMeta(ctx context.Context) (traceID, messageID, channel, userID string) {
	if meta, ok := ctx.Value(journalCtxKey{}).(journalMeta); ok {
		return meta.TraceID, meta.MessageID, meta.Channel, meta.UserID
	}
	return "", "", "", ""
}

// Middleware 返回一个 pipeline.Middleware，用于包装 LLMStage 并自动提取上下文。
// 它会在 LLMStage 执行前从 Envelope 中提取 trace_id、message_id、channel 等，
// 并将这些信息暂存到 context 中，供 RecordUsage 调用时使用。
//
// 注意：由于 UsageRecorder 接口在调用时不携带 Envelope 引用，
// 中间件通过 context 传递补充的上下文信息。
func (r *RunJournalRecorder) Middleware() Middleware {
	if r.db == nil {
		return func(next core.Stage) core.Stage { return next }
	}

	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				// 注入上下文元数据
				ctx = context.WithValue(ctx, journalCtxKey{}, journalMeta{
					TraceID:   env.Message.TraceID,
					MessageID: env.Message.ID,
					Channel:   env.Message.Channel,
					UserID:    env.Message.UserID,
				})

				return next.Process(ctx, env)
			},
		}
	}
}

// Flush 立即将缓冲区中的所有记录写入数据库。
func (r *RunJournalRecorder) Flush(ctx context.Context) error {
	if r.db == nil {
		return nil
	}

	r.mu.Lock()
	if len(r.buffer) == 0 {
		r.mu.Unlock()
		return nil
	}
	batch := make([]dao.RunJournal, len(r.buffer))
	copy(batch, r.buffer)
	r.buffer = r.buffer[:0]
	r.mu.Unlock()

	return r.db.WithContext(ctx).CreateInBatches(batch, 100).Error
}

// Shutdown 停止后台 Flush goroutine 并执行最后一次 Flush。
func (r *RunJournalRecorder) Shutdown(ctx context.Context) error {
	if r.db == nil {
		return nil
	}

	r.shutdownOnce.Do(func() {
		close(r.shutdownCh)
	})

	return r.Flush(ctx)
}

// Run 启动后台 Flush goroutine。
// 需要传入 context 以支持优雅关闭。
func (r *RunJournalRecorder) Run(ctx context.Context) {
	if r.db == nil {
		return
	}

	go func() {
		for {
			select {
			case <-r.flushCh:
				_ = r.Flush(ctx)
			case <-r.shutdownCh:
				_ = r.Flush(ctx)
				return
			case <-ctx.Done():
				_ = r.Flush(context.Background())
				return
			}
		}
	}()
}

// ============================================================================
// RunJournalWrapper — 替代方案：Middleware 直接捕获结果并记录
//
// 此包装器不依赖 UsageRecorder 接口，而是在 LLMStage 执行后直接从
// Envelope KV (llm.result) 中提取 GenerateResult 并记录。
// 适合不需要修改 LLMConfig 的场景。
// ============================================================================

// RunJournalMiddleware 返回一个 Middleware，直接从 LLMStage 的执行结果中提取并记录 RunJournal。
//
// 与 RunJournalRecorder.Middleware() 的区别：
//   - recorder 版本：实现 UsageRecorder，LLMStage 自动回调，更准确
//   - 此版本：从 env.Get("llm.result") 提取，无需修改 LLMConfig，但只捕获最后一步
//
// 推荐：优先使用 RunJournalRecorder（实现 UsageRecorder），
// 此版本作为不需要修改 LLMConfig 时的 fallback。
func RunJournalMiddleware(db *gorm.DB, cfg RunJournalConfig) Middleware {
	if db == nil {
		return func(next core.Stage) core.Stage { return next }
	}

	if cfg.FlushThreshold <= 0 {
		cfg.FlushThreshold = 20
	}

	var mu sync.Mutex
	buffer := make([]dao.RunJournal, 0, cfg.FlushThreshold)

	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				result, err := next.Process(ctx, env)

				if result != nil {
					if v, ok := result.Get("llm.result"); ok {
						if genResult, ok := v.(*llm.GenerateResult); ok && genResult != nil {
							record := dao.RunJournal{
								TraceID:         env.Message.TraceID,
								RunID:           env.Message.TraceID,
								BotID:           env.Message.BotID,
								Channel:         env.Message.Channel,
								UserID:          env.Message.UserID,
								MessageID:       env.Message.ID,
								Model:           "",
								Feature:         cfg.Feature,
								Caller:          cfg.Caller,
								InputTokens:     genResult.Usage.InputTokens,
								OutputTokens:    genResult.Usage.OutputTokens,
								TotalTokens:     genResult.Usage.TotalTokens,
								CacheReadTokens: genResult.Usage.CachedInputTokens,
								ToolCalls:       len(genResult.ToolCalls),
								Status:          "success",
							}

							if genResult.FinishReason == llm.FinishReasonError {
								record.Status = "error"
							}

							mu.Lock()
							buffer = append(buffer, record)
							needFlush := len(buffer) >= cfg.FlushThreshold
							if needFlush {
								batch := make([]dao.RunJournal, len(buffer))
								copy(batch, buffer)
								buffer = buffer[:0]
								// 异步写入避免阻塞 Pipeline
								go func(b []dao.RunJournal) {
									_ = db.WithContext(context.Background()).CreateInBatches(b, 100).Error
								}(batch)
							}
							mu.Unlock()
						}
					}
				}

				return result, err
			},
		}
	}
}
