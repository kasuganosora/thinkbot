package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/traceid"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Pipeline — Stage 链式执行引擎
// ============================================================================

// Pipeline 按 Order 排序执行一系列 Stage，每个 Stage 处理消息信封。
// 支持中止、跳过、错误处理以及全程 OpenTelemetry 可观测性。
type Pipeline struct {
	stages []core.StageInfo
	tracer trace.Tracer
	logger *zap.SugaredLogger

	// metrics
	msgProcessed metric.Int64Counter
	msgErrors    metric.Int64Counter
	msgDropped   metric.Int64Counter
	stageLatency metric.Float64Histogram
}

// New 创建 Pipeline 实例。
// stages 通过 fx group 注入，tp/mp 是 OTel providers。
func New(stages []core.StageInfo, tp trace.TracerProvider, mp metric.MeterProvider, logger *zap.SugaredLogger) (*Pipeline, error) {
	// 按 Order 排序
	sorted := make([]core.StageInfo, len(stages))
	copy(sorted, stages)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Order < sorted[j].Order
	})

	tracer := tp.Tracer("github.com/kasuganosora/thinkbot/agent/pipeline")
	meter := mp.Meter("github.com/kasuganosora/thinkbot/agent/pipeline")

	processed, err := meter.Int64Counter("pipeline.messages.processed",
		metric.WithDescription("Total messages entering the pipeline"))
	if err != nil {
		return nil, errs.Wrap(err, "pipeline: create processed counter")
	}

	errCounter, err := meter.Int64Counter("pipeline.messages.errors",
		metric.WithDescription("Total message processing errors"))
	if err != nil {
		return nil, errs.Wrap(err, "pipeline: create error counter")
	}

	dropped, err := meter.Int64Counter("pipeline.messages.dropped",
		metric.WithDescription("Total messages dropped by stages"))
	if err != nil {
		return nil, errs.Wrap(err, "pipeline: create dropped counter")
	}

	latency, err := meter.Float64Histogram("pipeline.stage.duration_seconds",
		metric.WithDescription("Stage processing duration in seconds"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, errs.Wrap(err, "pipeline: create latency histogram")
	}

	return &Pipeline{
		stages:       sorted,
		tracer:       tracer,
		logger:       logger,
		msgProcessed: processed,
		msgErrors:    errCounter,
		msgDropped:   dropped,
		stageLatency: latency,
	}, nil
}

// Execute 执行完整的 Pipeline，按序依次调用每个已启用的 Stage。
//
// 行为约定：
//   - Stage 返回 nil Envelope → 消息被丢弃，Pipeline 终止
//   - Stage 返回 AbortError → Pipeline 立即终止并返回错误
//   - Stage 返回 SkipError → 跳过该 Stage，继续执行下一个
//   - Stage 返回其他 error → 记录错误后 Pipeline 继续
//   - Envelope.Aborted() == true → Pipeline 终止
func (p *Pipeline) Execute(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	// 从 context 中提取 trace ID，用于日志关联
	logger := traceid.WithLoggerFrom(ctx, p.logger)

	ctx, span := p.tracer.Start(ctx, "pipeline.execute",
		trace.WithAttributes(
			attribute.String("trace.id", traceid.FromContext(ctx)),
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.source", env.Message.Source),
			attribute.String("message.channel", env.Message.Channel),
		))
	defer span.End()

	p.msgProcessed.Add(ctx, 1)
	pipelineStart := time.Now()
	messageID := env.Message.ID // 保存，防止 Stage 返回 nil 后无法访问

	logger.Debugw("pipeline started",
		"message_id", env.Message.ID,
		"source", env.Message.Source,
		"stages", len(p.stages))

	for _, si := range p.stages {
		if !si.Enabled {
			continue
		}
		if env.Aborted() {
			logger.Infow("pipeline aborted by envelope",
				"message_id", env.Message.ID,
				"err", env.Err())
			break
		}

		var err error
		env, err = p.executeStage(ctx, si.Stage, env)
		if err != nil {
			if core.IsAbortError(err) {
				span.SetStatus(codes.Error, "aborted")
				span.RecordError(err)
				p.msgErrors.Add(ctx, 1)
				// 防御 nil env：AbortError 时 Stage 可能返回 nil envelope
				abortedMsgID := messageID
				if env != nil {
					abortedMsgID = env.Message.ID
				}
				logger.Errorw("pipeline aborted",
					"message_id", abortedMsgID,
					"stage", si.Stage.Name(),
					"err", err)
				return env, err
			}
			if core.IsSkipError(err) {
				logger.Debugw("stage skipped",
					"message_id", messageID,
					"stage", si.Stage.Name(),
					"reason", err.Error())
				continue
			}
			// 非致命错误：记录后继续
			logger.Warnw("stage error (continuing)",
				"message_id", messageID,
				"stage", si.Stage.Name(),
				"err", err)
		}
		if env == nil {
			// Stage 返回 nil → 消息被丢弃
			p.msgDropped.Add(ctx, 1)
			logger.Infow("message dropped by stage",
				"message_id", messageID,
				"stage", si.Stage.Name())
			span.SetAttributes(attribute.String("pipeline.dropped_by", si.Stage.Name()))
			return nil, nil
		}
	}

	totalDuration := time.Since(pipelineStart).Seconds()
	p.stageLatency.Record(ctx, totalDuration,
		metric.WithAttributes(attribute.String("stage", "_pipeline_total")))

	span.SetAttributes(
		attribute.Int("pipeline.actions", len(env.Actions())),
		attribute.Float64("pipeline.duration_seconds", totalDuration),
	)

	logger.Debugw("pipeline completed",
		"message_id", env.Message.ID,
		"actions", len(env.Actions()),
		"duration_s", totalDuration)

	return env, nil
}

// executeStage 执行单个 Stage，包装 OTel span 和 panic recovery。
func (p *Pipeline) executeStage(ctx context.Context, s core.Stage, env *core.Envelope) (result *core.Envelope, retErr error) {
	logger := traceid.WithLoggerFrom(ctx, p.logger)

	ctx, span := p.tracer.Start(ctx, "stage."+s.Name(),
		trace.WithAttributes(
			attribute.String("trace.id", traceid.FromContext(ctx)),
			attribute.String("stage.name", s.Name()),
			attribute.String("message.id", env.Message.ID),
		))
	defer span.End()

	stageStart := time.Now()
	defer func() {
		duration := time.Since(stageStart).Seconds()
		p.stageLatency.Record(ctx, duration,
			metric.WithAttributes(attribute.String("stage", s.Name())))

		span.SetAttributes(attribute.Float64("stage.duration_seconds", duration))
	}()

	// panic recovery
	defer func() {
		if r := recover(); r != nil {
			// 捕获完整 stack trace
			stack := make([]byte, 4096)
			n := runtime.Stack(stack, false)
			stack = stack[:n]

			panicErr := fmt.Errorf("panic in stage %s: %v\n\nstack trace:\n%s", s.Name(), r, string(stack))
			span.SetStatus(codes.Error, "panic")
			span.RecordError(panicErr)
			p.msgErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", s.Name())))
			logger.Errorw("stage panic recovered",
				"stage", s.Name(),
				"message_id", env.Message.ID,
				"panic", r,
				"stack", string(stack))
			result = env
			retErr = &core.PipelineError{
				Stage:   s.Name(),
				Message: "panic recovered",
				Cause:   panicErr,
			}
		}
	}()

	result, retErr = s.Process(ctx, env)
	if retErr != nil && !core.IsSkipError(retErr) {
		span.SetStatus(codes.Error, retErr.Error())
		span.RecordError(retErr)
		p.msgErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", s.Name())))
	}
	return result, retErr
}

// StageNames 返回所有已启用 Stage 的名称列表（按执行顺序）。
func (p *Pipeline) StageNames() []string {
	var names []string
	for _, si := range p.stages {
		if si.Enabled {
			names = append(names, si.Stage.Name())
		}
	}
	return names
}

// Len 返回已注册的 Stage 总数（包括未启用的）。
func (p *Pipeline) Len() int {
	return len(p.stages)
}

// ============================================================================
// 内部工具
// ============================================================================

// IsNilEnvelope 安全检查 nil envelope（避免 dropped 场景的 log 读 nil）。
var errDropped = errors.New("message dropped")
