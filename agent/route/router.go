package route

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	noop_metric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/llm"
)

const (
	ToolPreempt = "pipeline_preempt"
	ToolOpen    = "pipeline_open"
	ToolCancel  = "pipeline_cancel"

	defaultRouteTimeout = 8 * time.Second
	routeMaxSteps       = 3
)

// bindCodePattern matches an authorization code and nothing else, so those
// messages stay on the execution pipeline's existing code stage.
var bindCodePattern = regexp.MustCompile(`(?i)^TB-[A-Z2-9]{4}-[A-Z2-9]{4}$`)

// Action is the routing outcome for one user message.
type Action string

const (
	ActionNone    Action = ""
	ActionOpen    Action = "open"
	ActionPreempt Action = "preempt"
	ActionCancel  Action = "cancel"
	ActionRefuse  Action = "refuse"
)

// Decision is what L1 did with one message.
type Decision struct {
	Action     Action
	Intent     string
	Model      string
	Target     string
	DropReason string
}

// Continue reports whether the caller should run the execution pipeline for
// this same message. Preempt, cancel, and refuse must not.
func (d Decision) Continue() bool { return d.Action == ActionOpen }

// Config builds an L1 router.
type Config struct {
	Provider llm.Provider
	Model    *llm.Model
	Logger   *zap.SugaredLogger
	TP       trace.TracerProvider
	MP       metric.MeterProvider
	Limits   Limits
	Timeout  time.Duration
	// Cancel stops a running execution pipeline by bot and trace.
	// Nil means only a cancel func stored on the run is used.
	Cancel func(botID, traceID string) bool
	// LiveModel 返回这一回合钉住的路由模型。为空时用 Model。
	LiveModel func(ctx context.Context) *llm.Model
}

// Router is the L1 pipeline. It routes; it does not answer and it does not
// recall stored notes.
type Router struct {
	reg      *Registry
	pipe     *pipeline.Pipeline
	provider llm.Provider
	model    *llm.Model
	logger   *zap.SugaredLogger
	timeout  time.Duration
	cancel   func(botID, traceID string) bool
}

// New builds the L1 pipeline. Provider is required.
func New(cfg Config) (*Router, error) {
	if cfg.Provider == nil {
		return nil, errNoProvider
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if cfg.TP == nil {
		cfg.TP = noop_trace.NewTracerProvider()
	}
	if cfg.MP == nil {
		cfg.MP = noop_metric.NewMeterProvider()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultRouteTimeout
	}
	reg := NewRegistry(cfg.Limits)
	stage := &l1Stage{
		reg:      reg,
		provider: cfg.Provider,
		model:    cfg.Model,
		live:     cfg.LiveModel,
		logger:   logger,
		cancel:   cfg.Cancel,
	}
	pipe, err := pipeline.New(
		[]core.StageInfo{{Stage: stage, Order: 1, Enabled: true}},
		cfg.TP, cfg.MP, logger,
	)
	if err != nil {
		return nil, err
	}
	return &Router{
		reg:      reg,
		pipe:     pipe,
		provider: cfg.Provider,
		model:    cfg.Model,
		logger:   logger,
		timeout:  timeout,
		cancel:   cfg.Cancel,
	}, nil
}

var errNoProvider = errString("route: provider is required")

type errString string

func (e errString) Error() string { return string(e) }

// Registry returns the running-pipeline registry.
func (r *Router) Registry() *Registry { return r.reg }

// Pipeline returns the L1 pipeline.
func (r *Router) Pipeline() *pipeline.Pipeline { return r.pipe }

// OwnsReply reports whether a trace may write a reply stream.
func (r *Router) OwnsReply(traceID string) bool {
	if r == nil || r.reg == nil {
		return true
	}
	return r.reg.OwnsReply(traceID)
}

// ShouldRoute reports whether this message enters L1.
// System turns, slash commands, authorization codes, and messages that are
// already an L2 execution stay on the execution pipeline.
func (r *Router) ShouldRoute(env *core.Envelope) bool {
	if r == nil || env == nil {
		return false
	}
	if LevelOf(env) == LevelL2 {
		return false
	}
	msg := env.Message
	switch msg.Source {
	case core.SourceHeartbeat, core.SourceCron:
		return false
	}
	if env.IsOutreach() {
		return false
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, "/") {
		return false
	}
	if bindCodePattern.MatchString(text) {
		return false
	}
	return true
}

// Route runs the L1 pipeline. It never returns a decision that would both
// inject into a running pipeline and also execute this message as a new one.
func (r *Router) Route(ctx context.Context, env *core.Envelope) Decision {
	if r == nil || env == nil {
		return Decision{Action: ActionOpen, Intent: IntentTask, Model: ModelDefault}
	}
	SetLevel(env, LevelL1)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	_, err := r.pipe.Execute(ctx, env)
	if d, ok := decisionOf(env); ok {
		if d.DropReason != "" {
			env.Set(KVDropReason, d.DropReason)
		}
		if d.Action == ActionOpen {
			SetTier(env, d.Intent, d.Model)
		}
		r.logDecision(env, d, "model")
		return d
	}
	if err != nil {
		r.logger.Warnw("l1 route failed, using fallback", "err", err)
	}
	return r.fallback(env)
}

// logDecision makes the L1 choice observable. Without it, the tier that
// actually ran a turn can only be inferred from stream/no-stream side effects,
// which makes tier-related regressions impossible to audit from logs.
func (r *Router) logDecision(env *core.Envelope, d Decision, source string) {
	effective := d.Model
	if d.Action == ActionOpen {
		// normalizeTier may override an L1 "light" answer for task/followup;
		// log what actually runs, not just what the router model asked for.
		effective = ModelOf(env)
	}
	r.logger.Infow("l1 route decision",
		"trace_id", env.Message.TraceID,
		"bot_id", env.Message.BotID,
		"channel", env.Message.Channel,
		"action", string(d.Action),
		"intent", d.Intent,
		"model", d.Model,
		"effective_model", effective,
		"upgraded", Upgraded(env),
		"target", d.Target,
		"drop_reason", d.DropReason,
		"source", source)
}

func (r *Router) fallback(env *core.Envelope) (d Decision) {
	defer func() { r.logDecision(env, d, "fallback") }()
	key := ConversationKey(env)
	text := strings.TrimSpace(env.Message.Text)
	if id, err := r.reg.PreemptLatest(key, env.Message.TraceID, text); err == nil {
		d = Decision{Action: ActionPreempt, Target: id, DropReason: DropReasonPreempt}
		storeDecision(env, d)
		return d
	}
	if r.reg.AllowOpen(key) {
		d = Decision{Action: ActionOpen, Intent: IntentTask, Model: ModelDefault}
		SetTier(env, d.Intent, d.Model)
		storeDecision(env, d)
		return d
	}
	d = Decision{Action: ActionRefuse, DropReason: DropReasonRefuse}
	storeDecision(env, d)
	return d
}

// BeginL2 registers the execution pipeline that is about to run and returns
// a func that removes it. The returned func is safe to call on nil.
func (r *Router) BeginL2(ctx context.Context, env *core.Envelope) func() {
	if r == nil || env == nil {
		return func() {}
	}
	SetLevel(env, LevelL2)
	summary := clip(env.Message.Text, summaryLimit)
	if summary == "" {
		summary = clip(env.Message.InjectContext, summaryLimit)
	}
	id := r.reg.Add(Run{
		ID:        env.Message.TraceID,
		BotID:     env.Message.BotID,
		Key:       ConversationKey(env),
		TraceID:   env.Message.TraceID,
		Summary:   summary,
		System:    isSystem(env),
		Interrupt: core.InterruptChannelFromContext(ctx),
		Cancel:    r.cancelFunc(env),
	})
	return func() { r.reg.Finish(id) }
}

func (r *Router) cancelFunc(env *core.Envelope) context.CancelFunc {
	if r.cancel == nil {
		return nil
	}
	botID, traceID := env.Message.BotID, env.Message.TraceID
	return func() { r.cancel(botID, traceID) }
}

func isSystem(env *core.Envelope) bool {
	if env.Message.Source == core.SourceHeartbeat || env.Message.Source == core.SourceCron {
		return true
	}
	return env.IsOutreach()
}

type choice struct {
	mu     sync.Mutex
	picked Decision
	ok     bool
}

func (c *choice) take(d Decision) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok {
		return false
	}
	c.ok = true
	c.picked = d
	return true
}

const kvChoice = "route.choice"

func decisionOf(env *core.Envelope) (Decision, bool) {
	v, ok := env.Get(kvChoice)
	if !ok {
		return Decision{}, false
	}
	c, ok := v.(*choice)
	if !ok || c == nil {
		return Decision{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.picked, c.ok
}

func storeDecision(env *core.Envelope, d Decision) {
	v, ok := env.Get(kvChoice)
	c, is := v.(*choice)
	if !ok || !is || c == nil {
		c = &choice{}
		env.Set(kvChoice, c)
	}
	c.take(d)
	if d.DropReason != "" {
		env.Set(KVDropReason, d.DropReason)
	}
}

type l1Stage struct {
	reg      *Registry
	provider llm.Provider
	model    *llm.Model
	live     func(ctx context.Context) *llm.Model
	logger   *zap.SugaredLogger
	cancel   func(botID, traceID string) bool
}

func (s *l1Stage) Name() string { return "l1-route" }

func (s *l1Stage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ch := &choice{}
	env.Set(kvChoice, ch)
	env.ClearActions()
	key := ConversationKey(env)
	sys, user := s.prompt(env, key)
	model := s.model
	if s.live != nil {
		if live := s.live(ctx); live != nil {
			model = live
		}
	}
	params := llm.GenerateParams{
		Model:    model,
		System:   sys,
		Messages: []llm.Message{llm.UserMessage(user)},
		Tools:    s.tools(env, ch, key),
	}
	// 采样跟随模型配置。没有配置就省略；-1 由编码器省略。
	// 不再把路由分类改写成温度 0，也不再套一个写死的输出上限。
	if model != nil {
		params.Temperature = model.Temperature
	}
	// L1 只做一次三选一的路由分类，不需要深度推理。推理型模型在未指定
	// reasoning_effort 时会按默认档思考，单次调用会稳定超过 defaultRouteTimeout，
	// 使整条消息白等超时后才走 fallback。发给该模型真正会压低思考的档位：
	// GLM-5.2 接受 low 但按 high 执行，必须发 minimal；三档词表的最低档是 low；
	// 不支持该参数的模型族返回空值，保持不发送。
	modelID := ""
	if model != nil {
		modelID = model.ID
	}
	if effort := llm.LowestReasoningEffort(modelID); effort != "" {
		params.ReasoningEffort = &effort
	}
	// 推理档按这一次的模型算。采样和输出上限交给跟随层，从模型配置填。
	ctx = llm.WithKeptParams(ctx, llm.KeptParams{ReasoningEffort: true})
	cfg := &llm.OrchestrateConfig{
		Params:       params,
		MaxSteps:     routeMaxSteps,
		HardMaxSteps: routeMaxSteps,
		ToolChoiceForStep: func(step int, _ bool) any {
			if decided(ch) {
				return "none"
			}
			if step == 0 {
				return "required"
			}
			return "required"
		},
	}
	if _, err := llm.OrchestrateGenerate(ctx, s.provider, cfg); err != nil && !decided(ch) {
		s.logger.Warnw("l1 model call failed", "err", llm.SafeErrorText(err))
	}
	env.ClearActions()
	return env, nil
}

func decided(ch *choice) bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.ok
}

func (s *l1Stage) prompt(env *core.Envelope, key string) (string, string) {
	var b strings.Builder
	b.WriteString("You route one user message. You do not answer it and you do not recall stored notes.\n")
	b.WriteString("Call exactly one tool.\n")
	b.WriteString("- pipeline_preempt: the message continues, corrects, or adds context to a running pipeline, including a short follow-up on that same task. That pipeline keeps the only reply stream.\n")
	b.WriteString("- pipeline_open: a new task, a progress question, or a follow-up that is not context for a running pipeline. Casual chat sets intent=chat and model=light. A progress question sets intent=progress. If you are unsure, use model=default.\n")
	b.WriteString("- pipeline_cancel: the user wants a running pipeline stopped. Do not open another pipeline.\n")
	b.WriteString("Running pipelines in this conversation:\n")
	snaps := s.reg.List(env.Message.BotID, key)
	if len(snaps) == 0 {
		b.WriteString("(none)\n")
	}
	for _, snap := range snaps {
		b.WriteString("- id=")
		b.WriteString(snap.ID)
		b.WriteString(" state=running")
		if snap.System {
			b.WriteString(" system=true")
		}
		if snap.Summary != "" {
			b.WriteString(" summary=")
			b.WriteString(snap.Summary)
		}
		b.WriteString("\n")
	}
	return b.String(), strings.TrimSpace(env.Message.Text)
}

func (s *l1Stage) tools(env *core.Envelope, ch *choice, key string) []llm.Tool {
	schemaID := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pipeline_id": map[string]any{"type": "string", "description": "id of a running pipeline"},
		},
		"required": []any{"pipeline_id"},
	}
	return []llm.Tool{
		{
			Name:        ToolPreempt,
			Description: "Inject this user message into a running execution pipeline and let that pipeline keep the reply stream.",
			Parameters:  schemaID,
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				id := strArg(input, "pipeline_id")
				if id == "" {
					return nil, errString("pipeline_id is required")
				}
				if !ch.claim() {
					return nil, errString("route already chosen")
				}
				if err := s.reg.Preempt(id, key, env.Message.TraceID, env.Message.Text); err != nil {
					ch.undo()
					return nil, err
				}
				ch.set(Decision{Action: ActionPreempt, Target: id, DropReason: DropReasonPreempt})
				halt(ctx)
				return map[string]any{"status": "preempted", "pipeline_id": id}, nil
			},
		},
		{
			Name:        ToolOpen,
			Description: "Open a new execution pipeline for this message. intent is task, progress, followup, or chat. model is default or light.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"intent": map[string]any{"type": "string", "enum": []any{IntentTask, IntentProgress, IntentFollowup, IntentChat}},
					"model":  map[string]any{"type": "string", "enum": []any{ModelDefault, ModelLight}},
				},
			},
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				intent, model := normalizeTier(strArg(input, "intent"), strArg(input, "model"))
				if !ch.claim() {
					return nil, errString("route already chosen")
				}
				if !s.reg.AllowOpen(key) {
					if id, err := s.reg.PreemptLatest(key, env.Message.TraceID, env.Message.Text); err == nil {
						ch.set(Decision{Action: ActionPreempt, Target: id, DropReason: DropReasonPreempt})
						halt(ctx)
						return map[string]any{"status": "preempted", "pipeline_id": id, "reason": "pipeline budget"}, nil
					}
					ch.set(Decision{Action: ActionRefuse, DropReason: DropReasonRefuse})
					halt(ctx)
					return map[string]any{"status": "refused", "reason": "pipeline budget"}, nil
				}
				ch.set(Decision{Action: ActionOpen, Intent: intent, Model: model})
				SetTier(env, intent, model)
				if intent == IntentProgress {
					env.Set(KVStatus, formatSnapshots(s.reg.List(env.Message.BotID, key)))
				}
				halt(ctx)
				return map[string]any{"status": "opened", "intent": intent, "model": model}, nil
			},
		},
		{
			Name:        ToolCancel,
			Description: "Stop a running execution pipeline. Do not open a replacement.",
			Parameters:  schemaID,
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				id := strArg(input, "pipeline_id")
				if id == "" {
					return nil, errString("pipeline_id is required")
				}
				if !ch.claim() {
					return nil, errString("route already chosen")
				}
				if err := s.cancelRun(id, key); err != nil {
					ch.undo()
					return nil, err
				}
				ch.set(Decision{Action: ActionCancel, Target: id, DropReason: DropReasonCancel})
				halt(ctx)
				return map[string]any{"status": "cancelled", "pipeline_id": id}, nil
			},
		},
	}
}

func (s *l1Stage) cancelRun(id, key string) error {
	if s.cancel != nil {
		runBot, runTrace, ok := s.reg.lookup(id, key)
		if !ok {
			return errString("pipeline is not running")
		}
		s.cancel(runBot, runTrace)
		s.reg.Finish(id)
		return nil
	}
	return s.reg.Cancel(id, key)
}

func (c *choice) claim() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok {
		return false
	}
	c.ok = true
	return true
}

func (c *choice) set(d Decision) {
	c.mu.Lock()
	c.picked = d
	c.ok = true
	c.mu.Unlock()
}

func (c *choice) undo() {
	c.mu.Lock()
	c.ok = false
	c.picked = Decision{}
	c.mu.Unlock()
}

func halt(ctx *llm.ToolExecContext) {
	if ctx != nil {
		ctx.RequestHalt()
	}
}

func strArg(input any, key string) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func formatSnapshots(snaps []Snapshot) string {
	if len(snaps) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, snap := range snaps {
		b.WriteString("- id=")
		b.WriteString(snap.ID)
		if snap.Summary != "" {
			b.WriteString(" summary=")
			b.WriteString(snap.Summary)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (g *Registry) lookup(id, key string) (botID, traceID string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	run, found := g.runs[id]
	if !found || (key != "" && run.Key != key) {
		return "", "", false
	}
	return run.BotID, run.TraceID, true
}
