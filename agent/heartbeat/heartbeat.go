package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Heartbeat — Per-Bot 自主唤醒模块
//
// 核心职责（重设计后，见 docs/heartbeat-redesign.md）：
//   - 定时给 bot 一个「被触发」的机会，让 bot 自己审视记忆/待办/关注项，
//     决定是否有需要主动处理的事（主动找事做 / 判断没事做就静默结束）。
//   - 走与 @bot 完全相同的 pipeline（工具/记忆/SOUL 全在线），bot 是行动主体。
//   - 两级发言闸门（平台策略优先，其次 bot 自主）+ 连续唤醒硬频控（防自激刷屏）。
//
// 设计：
//   - 复用 cron.Scheduler + cron.Executor 模式（与 Dreaming 同构）。
//   - Executor 持有 TriggerRunner（即 *pipeline.Pipeline），直接同步触发真实编排。
//   - 配置和日志使用文件系统存储（data/heartbeat/{botId}/）。
// ============================================================================

// Config 心跳配置。
type Config struct {
	// Enabled 是否启用心跳。
	Enabled bool `json:"enabled"`
	// Interval 心跳间隔（分钟）。范围 1-1440，默认 30。
	Interval int `json:"interval"`
	// AllowPost 平台级是否允许心跳主动对外发言（默认 false）。
	// 第一级闸门：false → 本轮 KVSuppressReply=true（bot 仍可思考+记笔记但不发言）。
	AllowPost bool `json:"allow_post"`
	// MaxConsecutiveWakes 连续「产生行动的唤醒」上限（默认 3）。
	// 超过后后续心跳降级为纯 inject（不发言），直到冷却窗过期重置预算。
	MaxConsecutiveWakes int `json:"max_consecutive_wakes"`
	// CooldownMin 两次「产生行动的唤醒」之间的最小冷却分钟数（默认 0=退化为心跳周期）。
	// 用于重置连续唤醒预算，专治「自激链」（醒来→发言→又触发→又醒）。
	CooldownMin int `json:"cooldown_min"`
	// IdleWakeEvery 准入关卡的空闲放行周期（默认 4，最小 1）。
	// 语义：自上次唤醒以来若无新信号（无新消息、无新笔记），本次唤醒被准入关卡拒绝
	// （0-step，不消耗主 LLM 调用）；但连续拒绝达到 IdleWakeEvery 次时强制放行一次，
	// 给「时间本身也是信号」（如 bot 自己定的待办到期）留通道。
	// 设为 1 = 每次心跳都放行（等价于关闭准入关卡）。
	IdleWakeEvery int `json:"idle_wake_every"`
}

// DefaultConfig 返回默认心跳配置。
func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		Interval:            30,
		AllowPost:           false,
		MaxConsecutiveWakes: 3,
		CooldownMin:         0,
		IdleWakeEvery:       4,
	}
}

// Log 单条心跳日志（语义：本次唤醒做了什么，而非健康状态）。
//
// 架构级不变量：每一次心跳唤醒都必须落一条日志——包括被准入关卡拒绝的
// 「0-step turn」（Admitted=false）——保证心跳不是黑盒，可完整复盘。
type Log struct {
	ID       string   `json:"id"`                // "hb-{unixMilli}"
	Status   string   `json:"status"`            // acted | note | silent | suppressed | error
	Time     string   `json:"time"`              // "2026/7/1 14:30:00"
	Cost     float64  `json:"cost"`              // 执行耗时（秒）
	Actions  []string `json:"actions"`           // 本次唤醒产生的 Action 类型，如 ["reply"] ["note"] []
	Result   string   `json:"result"`            // 行动摘要（bot 做了什么 / 为何静默），非健康描述
	Admitted bool     `json:"admitted"`          // 是否通过准入关卡进入真实编排（false=0-step，未消耗主 LLM）
	TraceID  string   `json:"traceId,omitempty"` // 关联的 trace，便于串日志排查
	Reason   string   `json:"reason,omitempty"`  // 压制原因：platform_policy | frequency_cap（仅 suppressed 时有值）
	Decision string   `json:"decision,omitempty"`// 结构化决策：silent | post | note
	Target   string   `json:"target,omitempty"`  // 选定的发帖目标（Label），仅 post/suppressed 时有值
}

// 心跳日志状态常量。
const (
	StatusActed      = "acted"      // 主动对外发言/行动
	StatusNote       = "note"       // 只记笔记，不发言
	StatusSilent     = "silent"     // bot 自主决定啥也不做
	StatusSuppressed = "suppressed" // 平台策略压制（allow_post=false / 频控降级），未发言
	StatusError      = "error"      // 执行失败
)

// 心跳决策契约（结构化 JSON，解决 Bug 3：用确定枚举代替自由文本「静默」表达）。
//
// LLM 只输出如下 JSON，杜绝「LLM 换个说法说『没事』就被程序解析失败」的老问题：
//
//	{"decision":"silent"|"post"|"note", "channel":"<目标渠道名>",
//	 "conversation_id":"<目标会话ID>", "content":"<要发的内容>", "reason":"<为什么>"}
//
// 解析失败一律安全降级为 silent（宁可多睡，不可误发）。
const (
	DecisionSilent = "silent" // 主动静默：已知晓但无需对外发声
	DecisionPost   = "post"   // 主动对外发帖（到指定真实渠道）
	DecisionNote   = "note"   // 只记内部笔记，不对外发声
)

// ChannelTarget 描述一个心跳「可主动发帖」的真实目标（由 ChannelLister 枚举）。
type ChannelTarget struct {
	// Channel 真实渠道名（即 ChannelReplyHandler 注册的 Sender 名，如 "Misskey" / telegram 实例名）。
	Channel string `json:"channel"`
	// Type 渠道类型："misskey" | "telegram" | "web" 等。
	Type string `json:"type"`
	// ConversationID 平台级发帖目标：
	//   - telegram: chatID（字符串 int64）
	//   - misskey: noteID（回复某帖）；空串 = 时间线顶层新帖
	ConversationID string `json:"conversation_id,omitempty"`
	// Label 给 LLM 看的可读标签（如 "Misskey 时间线（发新帖）" / "Telegram: 群名"）。
	Label string `json:"label"`
}

// ChannelLister 枚举某 bot 当前可主动发帖的渠道/会话目标。
type ChannelLister func(ctx context.Context) ([]ChannelTarget, error)

// ChannelPoster 把心跳决策的内容发到指定真实目标，
// 绕过伪频道 "heartbeat" 的 dispatcher（杜绝 Bug 2：no sender for channel "heartbeat"）。
type ChannelPoster func(ctx context.Context, target ChannelTarget, content string) error

// NoteSaver 把心跳决策的内部笔记写入本 bot 的长期记忆（绕过伪频道 "heartbeat" 的 dispatcher）。
// 复用与 ActionNote 完全相同的记忆写入链路（bot 全局 scope），让 bot 自主记下的笔记可跨渠道召回。
type NoteSaver func(ctx context.Context, content string) error

// HeartbeatDecision 是 LLM 返回的结构化决策（见 buildHeartbeatPrompt）。
type HeartbeatDecision struct {
	Decision       string `json:"decision"`                   // silent | post | note
	Channel        string `json:"channel,omitempty"`          // 目标 ChannelTarget.Channel（post 时）
	ConversationID string `json:"conversation_id,omitempty"`  // 目标 ConversationID（post 时）
	Content        string `json:"content,omitempty"`          // 要发的内容（post 时必填）
	Reason         string `json:"reason,omitempty"`           // 为什么这样决定（审计/日志）
}

// TriggerRunner 触发 bot 真实编排的能力（由 *agent.Engine 实现）。
// 解耦 heartbeat 包对 agent 根包的依赖，避免循环 import。
//
// 注意：必须走 Engine 的完整链路（pipeline.Execute + dispatcher.Dispatch），
// 而非只调 pipeline.Execute——出站发送与 ActionNote 落记忆都发生在 Dispatcher 层，
// 只跑 pipeline 会让 bot「想了但什么都没落地」。
type TriggerRunner interface {
	ProcessSync(ctx context.Context, env *core.Envelope) (*core.Envelope, []core.Action, error)
}

// AdmissionFn 准入关卡的信号探测函数（见 docs/heartbeat-redesign.md §5.5）。
// since 为上次唤醒时间（零值表示进程启动后首次唤醒）。
// 返回 (是否存在值得开一轮的信号, 信号描述)。
type AdmissionFn func(ctx context.Context, since time.Time) (bool, string)

// Executor 实现 cron.Executor 接口，桥接 cron 调度器与 bot 自主唤醒。
type Executor struct {
	botID    string
	store    *Store
	location *time.Location
	logger   *zap.SugaredLogger
	// allowPostFn 平台/部署级策略：是否允许心跳对外发言（可动态，默认放行由配置决定）。
	// 与配置 AllowPost 取「更严格」者：任一 false 即压制。
	allowPostFn func() bool
	// admissionFn 准入信号探测（nil = 无探测能力，一律放行进入编排）。
	admissionFn AdmissionFn

	// channelLister 枚举本 bot 可主动发帖的真实渠道/会话（nil = 心跳只能 silent/内部 note）。
	channelLister ChannelLister
	// channelPoster 把决策内容发到选定真实渠道（绕过伪频道 "heartbeat" 的 dispatcher）。
	channelPoster ChannelPoster
	// noteSaver 把决策的内部笔记写入本 bot 长期记忆（DecisionNote 时调用，复用 ActionNote 链路）。
	noteSaver NoteSaver

	// runner 在 bot 构建完成后由 SetRunner 注入（Engine 在 bot.New 内部创建，
	// 而 Scheduler 必须在 bot.New 之前构造并传入 → 二者存在构建顺序倒挂）。
	runnerMu sync.RWMutex
	runner   TriggerRunner

	// 频控状态（单 bot 串行触发，cron runningJobs 防重叠，内存态足够；重启重置可接受）。
	wakeMu           sync.Mutex
	consecutiveWakes int
	lastActionAt     time.Time
	lastWakeAt       time.Time // 上次「进入编排」的唤醒时间，准入关卡的信号窗口起点
	idleRejects      int       // 连续被准入关卡拒绝的次数
}

// ExecutorConfig 创建 Executor 的参数。
type ExecutorConfig struct {
	Runner        TriggerRunner
	BotID         string
	Store         *Store
	Location      *time.Location
	Logger        *zap.SugaredLogger
	AllowPostFn   func() bool
	AdmissionFn   AdmissionFn
	ChannelLister ChannelLister
	ChannelPoster ChannelPoster
	// NoteSaver 把决策的内部笔记写入本 bot 长期记忆（DecisionNote 时调用）。
	NoteSaver NoteSaver
}

// NewExecutor 创建心跳执行器。
func NewExecutor(cfg ExecutorConfig) *Executor {
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	allowPost := cfg.AllowPostFn
	if allowPost == nil {
		// 默认不额外收紧：发言与否交给 Config.AllowPost（默认 false）决定。
		allowPost = func() bool { return true }
	}
	return &Executor{
		runner:        cfg.Runner,
		botID:         cfg.BotID,
		store:         cfg.Store,
		location:      loc,
		logger:        cfg.Logger.With("component", "heartbeat_executor", "bot_id", cfg.BotID),
		allowPostFn:   allowPost,
		admissionFn:   cfg.AdmissionFn,
		channelLister: cfg.ChannelLister,
		channelPoster: cfg.ChannelPoster,
		noteSaver:     cfg.NoteSaver,
	}
}

// SetRunner 注入真实编排入口（bot.New 之后调用）。
func (e *Executor) SetRunner(r TriggerRunner) {
	e.runnerMu.Lock()
	e.runner = r
	e.runnerMu.Unlock()
}

// getRunner 读取当前 runner。
func (e *Executor) getRunner() TriggerRunner {
	e.runnerMu.RLock()
	defer e.runnerMu.RUnlock()
	return e.runner
}

// appendLog 持久化一条心跳日志；落库失败时不再静默丢弃，而是 Warn 出来，
// 否则审计轨迹丢失且无任何信号（data/heartbeat/{bot}/logs.json 写入失败场景）。
func (e *Executor) appendLog(entry Log) {
	if err := e.store.AppendLog(e.botID, entry); err != nil {
		e.logger.Warnw("heartbeat: failed to persist log entry",
			"err", err, "status", entry.Status, "trace_id", entry.TraceID)
	}
}

// NotifyUserActivity 通知「有真实外部消息进来了」，重置连续唤醒预算（设计文档 §9.3）。
//
// 语义：频控的目的是掐断「bot 自己把自己越唤越兴奋」的自激链。一旦有真实外部活动，
// 说明 bot 不处于自激真空，预算应立即恢复，让 bot 能及时对新情况做主动反应。
// 该方法必须廉价（纯内存），因为它挂在每条入站消息路径上。
func (e *Executor) NotifyUserActivity() {
	e.wakeMu.Lock()
	hadBudget := e.consecutiveWakes > 0 || !e.lastActionAt.IsZero()
	e.consecutiveWakes = 0
	e.lastActionAt = time.Time{}
	e.wakeMu.Unlock()
	// 仅当状态确实发生变更才记日志，避免对每条入站消息刷屏；
	// 这条日志是排查「下一次心跳为何不再降级/压制」的关键信号。
	if hadBudget {
		e.logger.Infow("heartbeat: user activity reset wake budget",
			"bot_id", e.botID)
	}
}

// Execute 实现 cron.Executor 接口。
// 构造心跳唤醒消息 → 进入真实 pipeline（工具/记忆/SOUL 全在线）→ 收集 Actions 生成日志。
func (e *Executor) Execute(ctx context.Context, _ *cron.Job) (*cron.ExecuteResult, error) {
	start := time.Now()
	now := start.In(e.location)

	// 1. 读取配置（interval / allow_post / 频控 / 准入）
	cfg, _ := e.store.LoadConfig(e.botID)
	interval := 30
	allowPost := false
	maxWakes := 3
	cooldown := 0
	idleWakeEvery := 4
	if cfg != nil {
		if cfg.Interval > 0 {
			interval = cfg.Interval
		}
		allowPost = cfg.AllowPost
		if cfg.MaxConsecutiveWakes > 0 {
			maxWakes = cfg.MaxConsecutiveWakes
		}
		cooldown = cfg.CooldownMin
		if cfg.IdleWakeEvery > 0 {
			idleWakeEvery = cfg.IdleWakeEvery
		}
	}
	// 平台策略优先：运行期 allowPostFn 与配置 AllowPost 取「更严格」者（任一 false 即压制）。
	if !e.allowPostFn() {
		allowPost = false
	}

	// 2. 频控：连续行动上限 + 冷却窗（重置窗口 = max(cooldown, interval)）。
	//    冷却窗过期 → 重置连续计数（预算恢复），专治自激刷屏。
	resetWindow := time.Duration(cooldown) * time.Minute
	if resetWindow <= 0 {
		resetWindow = time.Duration(interval) * time.Minute
	}
	e.wakeMu.Lock()
	if !e.lastActionAt.IsZero() && now.Sub(e.lastActionAt) >= resetWindow {
		e.consecutiveWakes = 0
	}
	// 超阈或冷却窗内：降级为纯 inject（不发言）
	degraded := (maxWakes > 0 && e.consecutiveWakes >= maxWakes) ||
		(!e.lastActionAt.IsZero() && now.Sub(e.lastActionAt) < resetWindow)
	e.wakeMu.Unlock()

	// 3. 准入关卡（Admission Guard，见设计文档 §5.5）。
	//    自上次唤醒以来无新信号 → 直接 0-step 结束，不消耗主 LLM 调用；
	//    但连续拒绝达到 idleWakeEvery 次时强制放行一次（时间本身也是信号）。
	if e.admissionFn != nil && idleWakeEvery > 1 {
		e.wakeMu.Lock()
		since := e.lastWakeAt
		rejects := e.idleRejects
		e.wakeMu.Unlock()

		hasSignal, signal := e.admissionFn(ctx, since)
		forced := rejects+1 >= idleWakeEvery
		if !hasSignal && !forced && !since.IsZero() {
			e.wakeMu.Lock()
			e.idleRejects++
			rejects = e.idleRejects
			e.wakeMu.Unlock()

			cost := time.Since(start).Seconds()
			entry := Log{
				ID:       fmt.Sprintf("hb-%d", now.UnixMilli()),
				Status:   StatusSilent,
				Time:     now.Format("2006/1/2 15:04:05"),
				Cost:     cost,
				Actions:  []string{},
				Admitted: false,
				Result: fmt.Sprintf("准入关卡拒绝（0-step，未调用 LLM）：自上次唤醒以来无新信号；连续跳过 %d/%d",
					rejects, idleWakeEvery),
			}
			e.appendLog(entry)
			e.logger.Infow("heartbeat rejected by admission guard",
				"idle_rejects", rejects,
				"idle_wake_every", idleWakeEvery,
				"since", since.Format(time.RFC3339),
				"cost_sec", cost)
			return &cron.ExecuteResult{
				Output: "[silent] 准入关卡拒绝：无新信号（0-step）",
			}, nil
		}
		if !hasSignal && forced {
			signal = "无新信号，但连续跳过已达上限 → 强制放行"
		}
		if signal != "" {
			e.logger.Infow("heartbeat admitted", "signal", signal)
		}
	}

	// 4. 构造心跳唤醒消息（走真实编排链路）。
	//    Text 留空（不污染 L0 记忆），唤醒提示走独立 InjectContext 通道。
	traceID := traceid.New()

	// 4.1 枚举本 bot 当前可主动发帖的真实渠道/会话，作为 LLM 决策的可选目标。
	var targets []ChannelTarget
	if e.channelLister != nil {
		if t, lerr := e.channelLister(ctx); lerr == nil {
			targets = t
		} else {
			e.logger.Warnw("heartbeat: list channels failed", "err", lerr, "trace_id", traceID)
		}
	}

	msg := core.Message{
		ID:            fmt.Sprintf("hb-%d", now.UnixMilli()),
		BotID:         e.botID,
		TraceID:       traceID,
		Source:        core.SourceHeartbeat,
		Channel:       "heartbeat", // 独立会话空间，不与任何真实对话混淆
		UserID:        "system:heartbeat",
		Text:          "",
		InjectContext: buildHeartbeatPrompt(targets),
		Mentioned:     false,
		CreatedAt:     now,
	}
	env := core.NewEnvelope(msg)

	// 进入编排 → 记录本次唤醒时间并重置空闲拒绝计数。
	e.wakeMu.Lock()
	e.lastWakeAt = now
	e.idleRejects = 0
	e.wakeMu.Unlock()

	// 5. 决策模式 + 两级闸门。
	//    - 恒设 KVHeartbeatMode：LLMStage 强制 JSON 结构化输出（silent/post/note + 目标）。
	//    - 恒设 KVSuppressReply：心跳决策的真实发帖由 Executor 经 ChannelPoster 手动路由，
	//      绝不走伪频道 "heartbeat" 的通用 dispatcher（Bug 2 根因：no sender for channel heartbeat）。
	//    - 平台策略压制（allow_post=false / 频控降级）仍记录 reason，用于日志与「想发却被拦」判定。
	env.Set(core.KVHeartbeatMode, true)
	env.Set(core.KVHeartbeatTargets, targets)
	env.Set(core.KVSuppressReply, true)
	reason := ""
	if !allowPost || degraded {
		reason = "platform_policy"
		if degraded {
			reason = "frequency_cap"
		}
		env.Set(core.KVSuppressReplyReason, reason)
	}

	// 6. 同步触发真实编排（Engine.ProcessSync：pipeline + dispatcher 全链路）。
	runner := e.getRunner()
	if runner == nil {
		entry := Log{
			ID:       fmt.Sprintf("hb-%d", now.UnixMilli()),
			Status:   StatusError,
			Time:     now.Format("2006/1/2 15:04:05"),
			Cost:     time.Since(start).Seconds(),
			Admitted: true,
			TraceID:  traceID,
			Result:   "心跳执行器未接入编排入口（runner 为 nil）",
		}
		e.appendLog(entry)
		return nil, fmt.Errorf("heartbeat: runner is nil")
	}

	_, _, err := runner.ProcessSync(ctx, env)
	if err != nil {
		entry := Log{
			ID:       fmt.Sprintf("hb-%d", now.UnixMilli()),
			Status:   StatusError,
			Time:     now.Format("2006/1/2 15:04:05"),
			Cost:     time.Since(start).Seconds(),
			Admitted: true,
			TraceID:  traceID,
			Result:   fmt.Sprintf("心跳编排失败: %v", err),
		}
		e.appendLog(entry)
		e.logger.Warnw("heartbeat orchestration failed", "err", err, "trace_id", traceID)
		return nil, err
	}

	// 7. 解析 LLM 结构化决策（从 env 的 llm.result 取出 JSON，绝不依赖自由文本）。
	//    解析失败 / 目标非法 / 内容空 → 一律安全降级为 silent（宁可多睡，不可误发）。
	decision := e.parseDecision(ctx, env, targets, traceID)

	// 8. 按决策路由。
	status := StatusSilent
	posted := false
	targetLabel := ""
	var actions []string
	switch decision.Decision {
	case DecisionPost:
		targetLabel = describeTarget(targets, decision.Channel, decision.ConversationID)
		if allowPost && !degraded {
			tgt := findTarget(targets, decision.Channel, decision.ConversationID)
			if tgt != nil && e.channelPoster != nil && strings.TrimSpace(decision.Content) != "" {
				if perr := e.channelPoster(ctx, *tgt, decision.Content); perr != nil {
					e.logger.Warnw("heartbeat: post to channel failed",
						"err", perr, "channel", tgt.Channel, "trace_id", traceID)
					cost := time.Since(start).Seconds()
					e.appendLog(Log{
						ID:       fmt.Sprintf("hb-%d", now.UnixMilli()),
						Status:   StatusError,
						Time:     now.Format("2006/1/2 15:04:05"),
						Cost:     cost,
						Admitted: true,
						TraceID:  traceID,
						Result:   fmt.Sprintf("发帖失败: %v", perr),
						Decision: decision.Decision,
						Target:   targetLabel,
					})
					return &cron.ExecuteResult{Output: fmt.Sprintf("[%s] %v", StatusError, perr)}, perr
				}
				posted = true
				status = StatusActed
				actions = []string{"post"}
			} else {
				// 无发帖器 / 目标非法 / 内容空 → 安全降级为 silent（绝不误发）。
				decision.Decision = DecisionSilent
				decision.Reason = "目标非法或发帖器缺失，安全降级为静默"
				targetLabel = ""
			}
		} else {
			// 想发但被平台策略 / 频控压制。
			status = StatusSuppressed
			actions = []string{"post-suppressed"}
		}
	case DecisionNote:
		status = StatusNote
		actions = []string{"note"}
		// 把决策的内部笔记写入本 bot 长期记忆（复用 ActionNote 链路，bot 全局 scope）。
		if e.noteSaver != nil && strings.TrimSpace(decision.Content) != "" {
			if nerr := e.noteSaver(ctx, decision.Content); nerr != nil {
				e.logger.Warnw("heartbeat: save note failed",
					"err", nerr, "trace_id", traceID)
			}
		}
	default:
		status = StatusSilent
	}

	// 9. 频控计数：本次真实发帖 → 连续唤醒 +1，并更新 lastActionAt。
	if posted {
		e.wakeMu.Lock()
		e.consecutiveWakes++
		e.lastActionAt = now
		e.wakeMu.Unlock()
	}

	// 10. 生成日志。
	cost := time.Since(start).Seconds()
	summary := decisionSummary(decision, targetLabel)
	if summary == "" {
		if status == StatusSuppressed {
			summary = "平台策略压制，bot 本想主动发言但被拦（可内部思考/记笔记）"
		} else {
			summary = "bot 审视后判断当前没有需要主动处理的事"
		}
	}
	entry := Log{
		ID:       fmt.Sprintf("hb-%d", now.UnixMilli()),
		Status:   status,
		Time:     now.Format("2006/1/2 15:04:05"),
		Cost:     cost,
		Actions:  actions,
		Result:   summary,
		Admitted: true,
		TraceID:  traceID,
		// reason 仅在「被压制」时有意义（bot 想发言但被平台策略/频控拦）。
		// 主动静默(silent)/主动发帖成功(acted)/只记笔记(note)时不应带 reason，
		// 否则读日志会误判成「被平台拦了」，歪曲决策语义。
		Reason:   conditionalReason(reason, status),
		Decision: decision.Decision,
		Target:   targetLabel,
	}
	e.appendLog(entry)

	e.logger.Infow("heartbeat completed",
		"status", status,
		"decision", decision.Decision,
		"cost_sec", cost,
		"degraded", degraded,
		"reason", reason,
		"target", targetLabel,
		"summary", summary,
		"trace_id", traceID,
		"consecutive_wakes", e.consecutiveWakes)

	return &cron.ExecuteResult{
		Output: fmt.Sprintf("[%s] %s", status, summary),
	}, nil
}

// parseDecision 从 env 的 llm.result 解析结构化心跳决策。
// 任何解析异常都安全降级为 silent（不误发）。
func (e *Executor) parseDecision(ctx context.Context, env *core.Envelope, targets []ChannelTarget, traceID string) HeartbeatDecision {
	resI, ok := env.Get("llm.result")
	if !ok {
		return HeartbeatDecision{Decision: DecisionSilent, Reason: "llm 未产出结果（pipeline 可能在前序 stage 短路）"}
	}
	gr, ok := resI.(*llm.GenerateResult)
	if !ok {
		return HeartbeatDecision{Decision: DecisionSilent, Reason: "llm 结果类型异常"}
	}
	raw := strings.TrimSpace(gr.Text)
	if raw == "" {
		return HeartbeatDecision{Decision: DecisionSilent, Reason: "llm 返回空内容"}
	}
	jsonStr := extractJSON(raw)
	var d HeartbeatDecision
	if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
		e.logger.Warnw("heartbeat: decision JSON parse failed, downgrade to silent",
			"err", err, "trace_id", traceID, "raw_len", len(raw))
		return HeartbeatDecision{Decision: DecisionSilent, Reason: "决策 JSON 解析失败，安全降级为静默"}
	}
	// 归一化 decision 枚举（小写、去空）。
	d.Decision = strings.ToLower(strings.TrimSpace(d.Decision))
	switch d.Decision {
	case DecisionSilent, DecisionPost, DecisionNote:
		// 合法枚举
	default:
		e.logger.Warnw("heartbeat: unknown decision, downgrade to silent",
			"decision", d.Decision, "trace_id", traceID)
		d.Decision = DecisionSilent
		d.Reason = "未知决策枚举，安全降级为静默"
	}
	// post 必须校验目标在可发列表内，且内容非空；否则降级静默。
	if d.Decision == DecisionPost {
		if strings.TrimSpace(d.Content) == "" {
			d.Decision = DecisionSilent
			d.Reason = "post 但内容为空，安全降级为静默"
		} else if findTarget(targets, d.Channel, d.ConversationID) == nil {
			e.logger.Warnw("heartbeat: post target not in allowed list, downgrade to silent",
				"channel", d.Channel, "conversation_id", d.ConversationID, "trace_id", traceID)
			d.Decision = DecisionSilent
			d.Reason = "目标渠道不在可发列表内，安全降级为静默"
		}
	}
	return d
}

// findTarget 在可发目标列表中定位 LLM 选定的目标。
// 匹配规则：Channel 名必须相等；ConversationID 必须相等，除非 LLM 未指定会话且该渠道只有一个目标
// （如 Misskey 时间线 / Telegram 单聊），此时退化为命中该唯一目标。多聊场景下会话不明确则不匹配（安全）。
func findTarget(targets []ChannelTarget, channel, convID string) *ChannelTarget {
	var fallback *ChannelTarget
	count := 0
	for i := range targets {
		if targets[i].Channel != channel {
			continue
		}
		count++
		fallback = &targets[i]
		if targets[i].ConversationID == convID {
			return &targets[i]
		}
	}
	if count == 1 && convID == "" {
		return fallback
	}
	return nil
}

// describeTarget 返回目标的可读标签（用于日志「想发到 X 但被拦」）。
func describeTarget(targets []ChannelTarget, channel, convID string) string {
	if t := findTarget(targets, channel, convID); t != nil {
		return t.Label
	}
	if channel != "" {
		return channel
	}
	return ""
}

// conditionalReason 仅在 status=suppressed 时保留压制原因，否则置空。
// 避免「bot 主动静默」被误读为「被平台策略拦下」。
func conditionalReason(reason string, status string) string {
	if status == StatusSuppressed {
		return reason
	}
	return ""
}

// decisionSummary 生成心跳日志/前端展示用的行动摘要。
func decisionSummary(d HeartbeatDecision, targetLabel string) string {
	switch d.Decision {
	case DecisionPost:
		content := d.Content
		if len(content) > 120 {
			content = content[:120] + "..."
		}
		if targetLabel != "" {
			return fmt.Sprintf("bot 决定在 %s 主动发言：%s", targetLabel, content)
		}
		return fmt.Sprintf("bot 决定主动发言：%s", content)
	case DecisionNote:
		content := d.Content
		if len(content) > 120 {
			content = content[:120] + "..."
		}
		if content == "" {
			return "bot 记下一条内部笔记"
		}
		return fmt.Sprintf("bot 记下内部笔记：%s", content)
	case DecisionSilent:
		if d.Reason != "" {
			return "bot 审视后判断当前没有需要主动处理的事（" + d.Reason + "）"
		}
		return "bot 审视后判断当前没有需要主动处理的事"
	default:
		return ""
	}
}

// buildHeartbeatPrompt 构造心跳唤醒提示：基础审视指令 + JSON 决策契约 + 可发目标列表。
// 作为 InjectContext 注入 LLM（不污染 L0 记忆）。
func buildHeartbeatPrompt(targets []ChannelTarget) string {
	var b strings.Builder
	b.WriteString(heartbeatWakePrompt)
	b.WriteString("\n\n# 你的输出格式（必须严格遵守）\n")
	b.WriteString("你只能输出一个 JSON 对象，不要输出任何解释性文字，也不要使用 markdown 代码块。结构如下：\n")
	b.WriteString("{\n  \"decision\": \"silent\" | \"post\" | \"note\",\n")
	b.WriteString("  \"channel\": \"<目标渠道名，仅 post 时填>\",\n")
	b.WriteString("  \"conversation_id\": \"<目标会话ID，仅 post 时填，可留空表示时间线/默认>\",\n")
	b.WriteString("  \"content\": \"<要发的内容，post 时必填>\",\n")
	b.WriteString("  \"reason\": \"<你为什么这样决定，便于审计>\"\n}\n")
	if len(targets) == 0 {
		b.WriteString("\n当前没有任何可主动发帖的渠道（未连接 Misskey/Telegram 或尚无可发会话），" +
			"因此你只能输出 {\"decision\":\"silent\"} 或 {\"decision\":\"note\"}。\n")
	} else {
		b.WriteString("\n当前你可以主动发帖的渠道/会话（post 时 channel/conversation_id 必须取下列之一）：\n")
		for _, t := range targets {
			fmt.Fprintf(&b, "- channel=%q, conversation_id=%q, 说明=%q\n", t.Channel, t.ConversationID, t.Label)
		}
	}
	b.WriteString("\n决策指引：\n")
	b.WriteString("- silent：审视后认为此刻无需对外做任何事（最常见）。\n")
	b.WriteString("- post：你确实想对外说点什么（如提醒主人带伞、在群里分享观察）。必须从上面列表选一个真实目标，并写好 content。\n")
	b.WriteString("- note：只想记一条内部笔记供以后回忆，不对外发声。\n")
	return b.String()
}

// extractJSON 从 LLM 原始输出中 tolerant 抽取 JSON 子串（防 markdown 围栏/前后缀干扰）。
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return s // 退化：原样返回，交给 json.Unmarshal 报错（上层会降级为 silent）
	}
	return s[start : end+1]
}

// heartbeatWakePrompt 心跳唤醒提示（作为 InjectContext 注入 LLM，不污染 L0 记忆）。
const heartbeatWakePrompt = `这是一次系统自主心跳唤醒（heartbeat），不是用户发来的消息。

请审视你的长期记忆、待办事项、关注的人与话题，判断此刻是否有需要你主动处理的事（例如：跟进未完成的任务、回复重要的人、发布你一直想发的观察、检查某个状态）。

如果你判断当前没有值得主动处理的事，请安静结束，不要为了刷存在感而发言——直接产出静默（不回复、不主动发帖）即可。

注意：你的任何对外输出都可能在公开渠道被发布，请自行判断其重要性。若你选择发言，请确保内容值得公开。`

// ============================================================================
// Bundle — 封装心跳子系统的完整组件
// ============================================================================

// Bundle 封装心跳子系统的完整组件。
type Bundle struct {
	Executor  *Executor
	Scheduler *cron.Scheduler
	Store     *Store
	cronStore *cron.Store
}

// BundleConfig 创建 Bundle 的参数。
type BundleConfig struct {
	BotID string
	// Runner 真实编排入口（*agent.Engine）。可为 nil，稍后用 Bundle.SetRunner 注入
	// ——Engine 在 bot.New 内部创建，而 Scheduler 必须先于 bot.New 构造并传入。
	Runner   TriggerRunner
	Location *time.Location
	Logger   *zap.SugaredLogger
	// Store 复用外部（API Server）已有的 Store，保证配置/日志读写共享同一把 per-bot 锁。
	// 为 nil 时按 DataDir 新建。
	Store       *Store
	DataDir     string // 心跳数据根目录，默认 "data/heartbeat"
	AllowPostFn func() bool
	AdmissionFn AdmissionFn
	// ChannelLister 枚举本 bot 可主动发帖的真实渠道/会话（nil = 心跳只能 silent/内部 note）。
	ChannelLister ChannelLister
	// ChannelPoster 把决策内容发到选定真实渠道（绕过伪频道 "heartbeat" 的 dispatcher）。
	ChannelPoster ChannelPoster
	// NoteSaver 把决策的内部笔记写入本 bot 长期记忆（DecisionNote 时调用）。
	NoteSaver NoteSaver
}

// NewBundle 创建心跳子系统 Bundle。
// 如果配置为 disabled，返回 nil。
// 返回的 Bundle 中 Scheduler 已注册 Job 但尚未 Start。
func NewBundle(cfg BundleConfig) *Bundle {
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "data/heartbeat"
	}

	store := cfg.Store
	if store == nil {
		store = NewStore(dataDir)
	}

	// 加载配置，检查是否启用
	config, _ := store.LoadConfig(cfg.BotID)
	if config == nil {
		def := DefaultConfig()
		config = &def
		_ = store.SaveConfig(cfg.BotID, config)
	}
	if config.Interval <= 0 {
		config.Interval = DefaultConfig().Interval
	}
	if !config.Enabled {
		return nil
	}

	// 创建 Executor（接真实编排链路）
	executor := NewExecutor(ExecutorConfig{
		Runner:      cfg.Runner,
		BotID:         cfg.BotID,
		Store:         store,
		Location:      cfg.Location,
		Logger:        cfg.Logger,
		AllowPostFn:   cfg.AllowPostFn,
		AdmissionFn:   cfg.AdmissionFn,
		ChannelLister: cfg.ChannelLister,
		ChannelPoster: cfg.ChannelPoster,
		NoteSaver:     cfg.NoteSaver,
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

	// 清理同名残留 Job：CreateJob 用随机 UUID，不清理会导致
	// data/heartbeat/<botID>/.cron.json 跨重启累积多个同名 job（同一心跳被触发 N 次）。
	jobName := "heartbeat-" + cfg.BotID
	for _, existing := range mgr.ListJobs() {
		if existing.Name == jobName {
			if derr := mgr.DeleteJob(existing.ID); derr != nil {
				cfg.Logger.Warnw("heartbeat: failed to prune stale cron job",
					"job_id", existing.ID, "err", derr)
			}
		}
	}

	schedule := fmt.Sprintf("every %dm", config.Interval)
	_, err := mgr.CreateJob(cron.CreateJobRequest{
		Name:     jobName,
		Prompt:   "trigger heartbeat self-wake",
		Schedule: schedule,
		Feature:  "heartbeat",
		Tags:     []string{"heartbeat", "autonomous"},
	})
	if err != nil {
		cfg.Logger.Errorw("failed to create heartbeat cron job", "err", err, "bot_id", cfg.BotID)
		return nil
	}

	cfg.Logger.Infow("heartbeat bundle created",
		"bot_id", cfg.BotID,
		"interval_min", config.Interval,
		"allow_post", config.AllowPost)

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

// SetRunner 在 bot 构建完成后注入真实编排入口（*agent.Engine）。
// 必须在 Scheduler.Start 之前调用，否则首次心跳会以 runner=nil 失败。
func (b *Bundle) SetRunner(r TriggerRunner) {
	if b == nil || b.Executor == nil {
		return
	}
	b.Executor.SetRunner(r)
}

// NotifyUserActivity 转发到 Executor，重置连续唤醒预算。
func (b *Bundle) NotifyUserActivity() {
	if b == nil || b.Executor == nil {
		return
	}
	b.Executor.NotifyUserActivity()
}
