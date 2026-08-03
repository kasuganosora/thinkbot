package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// DefaultMaxConcurrency 是 DelegateMany 的默认并发上限。
// 与单次 spawn 的任务上限（5）对齐，使一次派多任务时真正全并发，
// 而非被旧默认 2 限流成"分批"。可用 SetMaxConcurrency 覆盖，
// 或全局配置 subagent.max_concurrency 覆盖。
const DefaultMaxConcurrency = 5

// ============================================================================
// SubAgentManager — 管理 SubAgent 生命周期
//
// SubAgentManager 让主 Agent 通过工具调用创建和管理 SubAgent。
// 每个被委托的任务在完全隔离的上下文中执行，不会污染主 Agent 的对话历史。
//
// 主要模式：
//   - Delegate（一次性）：创建临时 SubAgent → 执行任务 → 返回结果 → 自动关闭
//   - DelegateMany（并发批量）：同时创建多个 SubAgent 并行执行多个任务
//   - Spawn/Chat/Close（持久化）：创建有状态的 SubAgent → 多轮对话 → 手动关闭
// ============================================================================

// SubAgentManager 管理主 Agent 可调用的 SubAgent 实例。
type SubAgentManager struct {
	// mu 用 RWMutex：toolMgr/baseCtx/defaultToolSteps 属读多写少
	// （SetToolResolver 通常只在装配期写一次，而每次 Delegate 都要读），
	// 读路径用 RLock 可避免与写入构成 data race 又不串行化并发委托。
	mu              sync.RWMutex
	provider        llm.Provider // 从主 Agent 继承
	model           string       // 从主 Agent 继承
	subagents       map[string]*SubAgent
	counter         int64
	defaultOpts     []Option // 所有 SubAgent 默认继承的选项
	delegateTimeout time.Duration

	// 并发控制
	maxConcurrency int // DelegateMany 的最大并发数（0=不限制）

	// toolMgr 是可选的「主 Agent 工具解析器」。非 nil 时，委托创建的子 Agent
	// 会解析主 Agent 在子 Agent 场景下可用的全部工具（exec/读/写/列目录等，
	// 由 scope 自动排除 spawn 防套娃），并注入执行回路使其能像主 Agent 一样
	// 操作工作空间。nil 表示子 Agent 不携带任何工具（纯 LLM，旧行为，如 workflow 分析器）。
	toolMgr *tools.ToolManager
	// baseCtx 是解析子 Agent 工具时的会话上下文模板（通常只填 BotID；
	// IsSubagent 在 resolveTools 时强制置 true）。
	baseCtx tools.ToolSessionContext
	// defaultToolSteps 是带工具回路时的默认最大 LLM 步数预算（0 = 包默认）。
	defaultToolSteps int
}

// SubAgentInfo 描述一个活跃的持久化 SubAgent。
type SubAgentInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Turns int    `json:"turns"`
}

// TaskResult 是 DelegateMany 中单个任务的执行结果。
type TaskResult struct {
	Task    string `json:"task"`
	Text    string `json:"text"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// NewSubAgentManager 创建 SubAgent 管理器。
//
// provider 和 model 从主 Agent 的 LLMBundle 继承。
// defaultOpts 是所有 SubAgent 默认继承的配置（如温度、滑动窗口大小）。
func NewSubAgentManager(provider llm.Provider, model string, defaultOpts ...Option) *SubAgentManager {
	return &SubAgentManager{
		provider:        provider,
		model:           model,
		subagents:       make(map[string]*SubAgent),
		defaultOpts:     defaultOpts,
		delegateTimeout: 120 * time.Second,
		maxConcurrency:  DefaultMaxConcurrency,
	}
}

// SetDelegateTimeout 设置 delegate 工具的超时时间。
// 应在 Delegate/DelegateMany 调用前设置。
func (m *SubAgentManager) SetDelegateTimeout(d time.Duration) {
	m.mu.Lock()
	m.delegateTimeout = d
	m.mu.Unlock()
}

// SetMaxConcurrency 设置 DelegateMany 的最大并发数。
// 应在 DelegateMany 调用前设置。
func (m *SubAgentManager) SetMaxConcurrency(n int) {
	if n > 0 {
		m.mu.Lock()
		m.maxConcurrency = n
		m.mu.Unlock()
	}
}

// SetToolResolver 让委托创建的子 Agent 继承主 Agent 的可用工具（经 scope 过滤，
// 自动排除 spawn 防套娃），从而能操作工作空间（exec/读/写/列目录等）。
// base 通常只填 BotID；解析时 IsSubagent 会被强制置 true。
// 不调用本方法（或传 nil toolMgr）则子 Agent 不携带工具（纯 LLM，旧行为）。
func (m *SubAgentManager) SetToolResolver(toolMgr *tools.ToolManager, base tools.ToolSessionContext) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolMgr = toolMgr
	m.baseCtx = base
}

// SetDefaultToolSteps 设置带工具执行回路时的默认最大 LLM 步数预算（0 = 包默认）。
// 应在 Delegate/DelegateMany/Spawn 调用前设置。
func (m *SubAgentManager) SetDefaultToolSteps(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultToolSteps = n
}

// resolveTools 解析子 Agent 场景下可用的工具列表（IsSubagent=true）。
// toolMgr 为 nil 时返回 nil（子 Agent 不带工具）。
//
// 并发安全：toolMgr/baseCtx 由 SetToolResolver 在写锁下修改，这里必须先在
// 读锁内快照，再到锁外调用 ResolveTools（外部调用可能耗时，不宜持锁）。
// 早期版本裸读这两个字段，与 SetToolResolver 构成 data race（-race 可复现）。
//
// 注意：本方法自带加锁，**不可**在已持有 m.mu 的路径中调用；
// 持锁路径（如 Spawn）请改用 resolveToolsLocked。
func (m *SubAgentManager) resolveTools(ctx context.Context) ([]llm.Tool, error) {
	m.mu.RLock()
	toolMgr, sctx := m.toolMgr, m.baseCtx
	m.mu.RUnlock()
	return resolveToolsWith(ctx, toolMgr, sctx)
}

// resolveToolsLocked 与 resolveTools 等价，但假定调用方已持有 m.mu（读或写锁），
// 因此自身不再加锁，避免重入死锁。
func (m *SubAgentManager) resolveToolsLocked(ctx context.Context) ([]llm.Tool, error) {
	return resolveToolsWith(ctx, m.toolMgr, m.baseCtx)
}

// resolveToolsWith 是解析逻辑的无状态实现，供加锁/已持锁两个入口复用。
func resolveToolsWith(ctx context.Context, toolMgr *tools.ToolManager, base tools.ToolSessionContext) ([]llm.Tool, error) {
	if toolMgr == nil {
		return nil, nil
	}
	sctx := base
	sctx.IsSubagent = true
	return toolMgr.ResolveTools(ctx, &sctx)
}

// toolStepsSnapshot 在读锁下读取 defaultToolSteps，避免与 SetDefaultToolSteps 竞态。
func (m *SubAgentManager) toolStepsSnapshot() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultToolSteps
}

// ============================================================================
// 流式委托 + 卡死看门狗
// ============================================================================

const (
	// defaultDelegateStuckTimeout DelegateStream 卡死看门狗默认阈值：
	// 流式 LLM 调用连续无 token 输出超过该时长即判定卡死并终止。默认 180s。
	defaultDelegateStuckTimeout = 180 * time.Second
	// delegateHardTimeoutFactor 硬上限 = 卡死阈值 × 该系数，派生而非写死。
	// 与 sandbox 的硬兜底策略一致（看门狗时间 ×3）。
	delegateHardTimeoutFactor = 3
	// delegateWatchdogTick 看门狗轮询间隔。
	delegateWatchdogTick = 5 * time.Second
	// delegateMaxStartupGrace 首 token 宽限期上限：尚未收到任何 token 时，
	// 启动后不足该时长不判卡死，容忍 LLM「思考」阶段（读长输入 + 推理）无输出。
	// 实际宽限期取 stuckTimeout/2，并受此上限约束。
	delegateMaxStartupGrace = 60 * time.Second
)

// Delegate 创建一个临时 SubAgent，执行任务后自动关闭。
// 这是一次性委托模式，适合不需要多轮交互的场景。
func (m *SubAgentManager) Delegate(ctx context.Context, systemPrompt, task string, opts ...Option) (string, error) {
	timeout, _, defaultOpts := m.snapshotConfig()

	// 合并选项（WithCallTimeout 可能覆盖默认超时）
	allOpts := mergeOptionLists(defaultOpts, systemPrompt, opts...)

	// 注入主 Agent 在子 Agent 场景可用的工具（如有），使其能操作工作空间。
	// 但若调用方显式要求跳过工具（如 Analyzer 纯 LLM 任务），则不注入。
	if !hasSkipTools(opts...) {
		if tools, err := m.resolveTools(ctx); err == nil && len(tools) > 0 {
			allOpts = append(allOpts, WithTools(tools...), WithToolSteps(m.toolStepsSnapshot()))
		}
	}

	// 创建临时 SubAgent 以提取 callTimeout 覆盖值
	sa := New(m.provider, m.model, allOpts...)
	defer sa.Close()

	// 使用 callTimeout 覆盖或回退到管理器默认值
	effectiveTimeout := timeout
	if sa.callTimeout > 0 {
		effectiveTimeout = sa.callTimeout
	}

	if effectiveTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, effectiveTimeout)
		defer cancel()
	}

	return sa.Chat(ctx, task)
}

// DelegateStream 创建临时 SubAgent，流式执行任务，并用卡死看门狗保护。
//
// 与 Delegate（固定超时一刀切）不同，DelegateStream 用「卡死看门狗」判断 LLM 是否真卡死：
//   - 只要 LLM 持续输出 token（哪怕很慢）就不杀——正常处理超长 prompt（如 86 个 lint 问题）
//     不会因固定超时被迫中断；
//   - 只有连续 stuckTimeout 无任何 token（且已过首 token 宽限期）才判定「卡死」并终止；
//   - 硬上限 = stuckTimeout × delegateHardTimeoutFactor（派生，不写死），作为绝对兜底，
//     防止无限挂起（如模型以极小间隔吐 token 骗过卡死检测）。
//
// 带工具的场景（子 Agent 注入主 Agent 工作空间工具）：同样启用看门狗，但任意流片段
// （含工具调用/结果）都算活跃信号——长 exec 在首尾产生工具片段、且自身有超时保护，
// 不会被误判卡死；只有 LLM 彻底沉默才触发卡死判定。
//
// 参数：
//   - WithStuckTimeout(d)：覆盖卡死阈值（默认 180s）；WithCallTimeout 对本方法无效。
//
// 返回完整文本；被看门狗终止时返回带 stuck/hard 区分的错误。
func (m *SubAgentManager) DelegateStream(ctx context.Context, systemPrompt, task string, opts ...Option) (string, error) {
	_, _, defaultOpts := m.snapshotConfig()
	allOpts := mergeOptionLists(defaultOpts, systemPrompt, opts...)

	// 注入主 Agent 在子 Agent 场景可用的工具（如有）。
	// 但若调用方显式要求跳过工具（如 Analyzer 纯 LLM 任务），则不注入，
	// 避免误走 OrchestrateStream 多步编排循环导致卡死或延迟。
	if !hasSkipTools(opts...) {
		if tools, err := m.resolveTools(ctx); err == nil && len(tools) > 0 {
			allOpts = append(allOpts, WithTools(tools...), WithToolSteps(m.toolStepsSnapshot()))
		}
	}

	sa := New(m.provider, m.model, allOpts...)
	defer sa.Close()

	// 带工具：同样走下方统一的卡死看门狗（streamWithWatchdog 把任意流片段视为活跃，
	// 长 exec 在首尾产生工具调用/结果片段、且其自身有 sandbox 超时保护，不会被误判卡死；
	// 只有 LLM 彻底沉默才触发卡死判定）。不再提前返回。

	// 解析卡死阈值（stuck）与硬上限（hard = stuck × factor，派生，不写死）
	stuck := sa.stuckTimeout
	if stuck <= 0 {
		stuck = defaultDelegateStuckTimeout
	}
	hard := stuck * delegateHardTimeoutFactor

	return m.streamWithWatchdog(ctx, sa, task, stuck, hard)
}

// streamWithWatchdog 在卡死看门狗保护下运行一个 SubAgent 的一次流式任务。
//
// 与 Delegate/DelegateMany 早期用的「context.WithTimeout 一刀切」不同，看门狗区分
// 「慢但活着」与「真卡死」：
//   - 只要流持续产出任意片段（文本/推理 token、工具调用、工具结果）即视为活跃，
//     正常处理超长 prompt 或慢思考模型不会被中断；带工具时，长 exec 在首尾产生工具
//     片段、且其自身有 sandbox 超时保护，也不会被误判卡死；
//   - 仅当连续 stuck 无任何片段输出（且已过首片段宽限期）才判定卡死并终止；
//   - 硬上限 = stuck × delegateHardTimeoutFactor 作为绝对兜底，防止无限挂起。
//
// DelegateStream 与 DelegateMany 共用此方法，使 spawn 等带工具的子 Agent 也能真正
// 享受看门狗保护，而非被旧的工具分支逻辑绕过。
func (m *SubAgentManager) streamWithWatchdog(ctx context.Context, sa *SubAgent, task string, stuck, hard time.Duration) (string, error) {
	// streamCtx 才是真正传给流的可取消上下文：看门狗触达 killReason 时 cancel()
	// 才能中止底层 LLM/工具流，否则消费者会永远阻塞在 stream.Stream 上。
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := sa.Stream(streamCtx, task)
	if err != nil {
		// provider 不支持流式（如仅实现 DoGenerate 的后端 / 测试 mock）：
		// 退化到单次生成 + 硬上限超时。无法做「慢但活着」探活，但至少受 hard 兜底，
		// 比原先 120s 一刀切略好；生产（GLM 等支持流式）仍走下方看门狗。
		if strings.Contains(err.Error(), "stream not supported") {
			return m.chatWithHardTimeout(ctx, sa, task, hard)
		}
		return "", errs.Wrapf(err, "subagent stream failed")
	}

	var (
		lastActivity int64 // atomic: 上次收到任意片段的 unix nano
		gotFirst     int32 // atomic: 是否已收到首个片段
		killReason   string
	)
	startTime := time.Now()
	atomic.StoreInt64(&lastActivity, startTime.UnixNano())

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(delegateWatchdogTick)
		defer ticker.Stop()
		grace := stuck / 2
		if grace > delegateMaxStartupGrace {
			grace = delegateMaxStartupGrace
		}
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				elapsed := now.Sub(startTime)
				// 硬上限 = 总运行时间上限（派生：stuck × factor），作为绝对兜底。
				if elapsed > hard {
					killReason = "hard"
					cancel()
					return
				}
				// 首片段宽限期内不判卡死：LLM 读长输入 + 推理阶段可能长时间无输出
				if atomic.LoadInt32(&gotFirst) == 0 && elapsed < grace {
					continue
				}
				idle := now.Sub(time.Unix(0, atomic.LoadInt64(&lastActivity)))
				if idle > stuck {
					killReason = "stuck"
					cancel()
					return
				}
			}
		}
	}()

	var textBuf strings.Builder
	var streamErr error
	for part := range stream.Stream {
		// 任意片段都刷新活跃时间戳：文本/推理 token 与工具调用/结果都算活着，
		// 避免一段较长的 exec（仅在首尾产生工具片段）被误判卡死。
		atomic.StoreInt64(&lastActivity, time.Now().UnixNano())
		atomic.StoreInt32(&gotFirst, 1)
		switch p := part.(type) {
		case *llm.TextDeltaPart:
			textBuf.WriteString(p.Text)
		case *llm.ErrorPart:
			streamErr = p.Error
			cancel() // 流内错误：停止消费，触发看门狗退出
		}
	}
	// 流已正常结束（或已被看门狗取消）：通知看门狗退出，避免其阻塞在 ticker 上。
	cancel()
	<-watchdogDone

	switch {
	case killReason == "stuck":
		return "", errs.Newf("subagent LLM 卡死：连续 %s 无输出（卡死看门狗终止）", stuck)
	case killReason == "hard":
		return "", errs.Newf("subagent 超过硬上限 %s 被强制终止（看门狗兜底）", hard)
	case streamErr != nil:
		return "", errs.Wrapf(streamErr, "subagent stream failed")
	}
	return textBuf.String(), nil
}

// chatWithHardTimeout 是不支持流式时的退化路径：单次生成 + 硬上限超时。
// 无流片段可探活，故不做「慢但活着」判定，仅以 hard 作为绝对上限兜底。
func (m *SubAgentManager) chatWithHardTimeout(ctx context.Context, sa *SubAgent, task string, hard time.Duration) (string, error) {
	if hard > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, hard)
		defer c()
	}
	res, err := sa.Chat(ctx, task)
	if err != nil {
		return "", errs.Wrapf(err, "subagent chat failed")
	}
	return res, nil
}

// 每个任务在独立的 SubAgent 中执行，互不影响。
// 返回每个任务的结果（顺序与输入一致）。
// DelegateProgressHandler 接收 DelegateMany 内每个子 Agent 的生命周期进度通知，
// 用于把「多个子 Agent 并行」的过程实时推到 UI（而非等全部完成后才一次性返回）。
//   - phase: "start"（子 Agent 实际开始执行）/ "done"（完成，res 非 nil）
//   - index/total: 当前第几个 / 总共几个（1-based）
//   - elapsed: 该子 Agent 耗时（start 时为 0）
type DelegateProgressHandler func(phase string, index, total int, task string, elapsed time.Duration, res *TaskResult)

type delegateProgressKey struct{}

// WithDelegateProgress 将进度回调挂到 ctx，DelegateMany 在执行时会按子 Agent 生命周期回调。
// spawn 工具用它把并行进度实时推到前端，解决「spawn 同步阻塞导致看不出并行」的体感问题。
func WithDelegateProgress(ctx context.Context, fn DelegateProgressHandler) context.Context {
	return context.WithValue(ctx, delegateProgressKey{}, fn)
}

func delegateProgressFromCtx(ctx context.Context) DelegateProgressHandler {
	if fn, ok := ctx.Value(delegateProgressKey{}).(DelegateProgressHandler); ok {
		return fn
	}
	return nil
}

func (m *SubAgentManager) DelegateMany(ctx context.Context, systemPrompt string, tasks []string, opts ...Option) []TaskResult {
	// 快照配置（线程安全）
	timeout, maxConc, defaultOpts := m.snapshotConfig()

	// 预计算合并选项（WithCallTimeout 可能覆盖默认超时）
	allOpts := mergeOptionLists(defaultOpts, systemPrompt, opts...)

	// 注入主 Agent 在子 Agent 场景可用的工具（如有），使其能操作工作空间。
	// 但若调用方显式要求跳过工具（如 Analyzer 纯 LLM 任务），则不注入。
	if !hasSkipTools(opts...) {
		if tools, err := m.resolveTools(ctx); err == nil && len(tools) > 0 {
			allOpts = append(allOpts, WithTools(tools...), WithToolSteps(m.toolStepsSnapshot()))
		}
	}

	// 创建临时实例提取 callTimeout
	dummy := New(m.provider, m.model, allOpts...)
	effectiveTimeout := timeout
	if dummy.callTimeout > 0 {
		effectiveTimeout = dummy.callTimeout
	}
	dummy.Close()

	results := make([]TaskResult, len(tasks))

	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		go func(idx int, t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 进度：子 Agent 实际开始执行（信号量放行后，真正并发起跑）
			if ph := delegateProgressFromCtx(ctx); ph != nil {
				ph("start", idx+1, len(tasks), t, 0, nil)
			}
			startAt := time.Now()

			// 每个 SubAgent 有独立的超时上下文。
			// 子 Agent 执行 ctx 直接继承传入的 ctx（即消息级 ctx / msgCtx）：
			//   - 用户点击 stop（handleChatAbort → AbortMessage）会取消 msgCtx，从而精确
			//     终止正在跑的子 Agent；
			//   - 客户端断连不再取消 msgCtx（handler 断连分支已不再调用 AbortMessage，
			//     Bot 在独立 botCtx 运行），因此关页面不会腰斩后台长任务。
			// 子 Agent 仍受自身 effectiveTimeout（delegateTimeout）+ 卡死看门狗兜底，
			// 临时 SubAgent 执行完自动 Close，不会永久泄漏。
			taskCtx := ctx
			if effectiveTimeout > 0 {
				var cancel context.CancelFunc
				taskCtx, cancel = context.WithTimeout(ctx, effectiveTimeout)
				defer cancel()
			}

			sa := New(m.provider, m.model, allOpts...)
			defer sa.Close()

			// 用卡死看门狗替代原先的 context.WithTimeout 一刀切：
			// 慢但活着（持续产出片段）不杀，只有彻底沉默超过 stuck 才终止，hard 兜底。
			// 这样带工具的 spawn 子 Agent（如代码审查专家）也能真正享受看门狗保护。
			stuck := effectiveTimeout / delegateHardTimeoutFactor
			if stuck <= 0 {
				stuck = defaultDelegateStuckTimeout
			}
			hard := stuck * delegateHardTimeoutFactor
			if effectiveTimeout > 0 && hard > effectiveTimeout {
				hard = effectiveTimeout // 硬上限不超过 delegateTimeout
			}

			reply, err := m.streamWithWatchdog(taskCtx, sa, t, stuck, hard)
			res := TaskResult{Task: t, Text: reply, Success: err == nil}
			if err != nil {
				res.Error = err.Error()
			}
			// 进度：子 Agent 完成，推实时结果，让前端看见「多个在并行、逐个完成」
			if ph := delegateProgressFromCtx(ctx); ph != nil {
				ph("done", idx+1, len(tasks), t, time.Since(startAt), &res)
			}
			results[idx] = res
		}(i, task)
	}

	wg.Wait()

	// 服务端汇总日志：即使是非 spawn 直接调用 DelegateMany（无进度回调）时，
	// 也能在日志里看到本轮并发执行的整体成败，便于排障。
	if l := traceid.L(ctx); l != nil {
		ok, fail := 0, 0
		for _, r := range results {
			if r.Success {
				ok++
			} else {
				fail++
			}
		}
		l.Infow("subagent: delegate many complete",
			"total", len(results), "success", ok, "failed", fail, "max_concurrency", maxConc)
	}

	return results
}

// Spawn 创建一个持久化 SubAgent，返回其 ID。
// 该 SubAgent 会维护自己的对话上下文，适合需要多轮交互的场景。
func (m *SubAgentManager) Spawn(systemPrompt, name string, opts ...Option) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := fmt.Sprintf("sa-%d", atomic.AddInt64(&m.counter, 1))

	allOpts := make([]Option, 0, len(m.defaultOpts)+len(opts)+2)
	allOpts = append(allOpts, m.defaultOpts...)
	allOpts = append(allOpts, WithID(id))
	if name != "" {
		allOpts = append(allOpts, WithName(name))
	}
	if systemPrompt != "" {
		allOpts = append(allOpts, WithSystemPrompt(systemPrompt))
	}
	allOpts = append(allOpts, opts...)

	// 注入主 Agent 在子 Agent 场景可用的工具（如有），使其能操作工作空间。
	// 与 Delegate 系保持一致：调用方显式 WithSkipTools() 时不注入工具。
	// Spawn 已持有 m.mu 写锁，故使用 resolveToolsLocked 避免重入死锁。
	if !hasSkipTools(opts...) {
		if tools, err := m.resolveToolsLocked(context.Background()); err == nil && len(tools) > 0 {
			allOpts = append(allOpts, WithTools(tools...), WithToolSteps(m.defaultToolSteps))
		}
	}

	sa := New(m.provider, m.model, allOpts...)
	m.subagents[id] = sa
	return id, nil
}

// Chat 向持久化 SubAgent 发送消息并返回回复。
func (m *SubAgentManager) Chat(ctx context.Context, id, message string) (string, int, error) {
	m.mu.Lock()
	sa, ok := m.subagents[id]
	m.mu.Unlock()
	if !ok {
		return "", 0, fmt.Errorf("subagent %q not found", id)
	}

	reply, err := sa.Chat(ctx, message)
	if err != nil {
		return "", 0, err
	}
	return reply, sa.TurnCount(), nil
}

// Close 关闭并移除一个持久化 SubAgent。
func (m *SubAgentManager) Close(id string) error {
	m.mu.Lock()
	sa, ok := m.subagents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("subagent %q not found", id)
	}
	delete(m.subagents, id)
	m.mu.Unlock()

	sa.Close()
	return nil
}

// List 返回所有活跃的持久化 SubAgent 信息。
func (m *SubAgentManager) List() []SubAgentInfo {
	m.mu.Lock()
	// 快照 SubAgent 引用，避免持 m.mu 时获取 sa.mu 造成锁层级依赖
	ids := make([]string, 0, len(m.subagents))
	agents := make([]*SubAgent, 0, len(m.subagents))
	for id, sa := range m.subagents {
		ids = append(ids, id)
		agents = append(agents, sa)
	}
	m.mu.Unlock()

	result := make([]SubAgentInfo, 0, len(agents))
	for i, sa := range agents {
		result = append(result, SubAgentInfo{
			ID:    ids[i],
			Name:  sa.Name(),
			Turns: sa.TurnCount(),
		})
	}
	return result
}

// CloseAll 关闭所有持久化 SubAgent。
func (m *SubAgentManager) CloseAll() {
	m.mu.Lock()
	for id, sa := range m.subagents {
		sa.Close()
		delete(m.subagents, id)
	}
	m.mu.Unlock()
}

// snapshotConfig 返回当前配置的安全快照（线程安全）。
func (m *SubAgentManager) snapshotConfig() (timeout time.Duration, maxConc int, defaultOpts []Option) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delegateTimeout, m.maxConcurrency, m.defaultOpts
}

// mergeOptionLists 合并默认选项、系统提示词和额外选项。
func mergeOptionLists(defaultOpts []Option, systemPrompt string, opts ...Option) []Option {
	allOpts := make([]Option, 0, len(defaultOpts)+len(opts)+1)
	allOpts = append(allOpts, defaultOpts...)
	if systemPrompt != "" {
		allOpts = append(allOpts, WithSystemPrompt(systemPrompt))
	}
	allOpts = append(allOpts, opts...)
	return allOpts
}

// hasSkipTools 检查选项列表中是否包含 WithSkipTools()。
// 用于 Delegate/DelegateStream/DelegateMany 在注入工具前判断调用方是否要求跳过工具。
func hasSkipTools(opts ...Option) bool {
	for _, opt := range opts {
		// 创建临时探测实例：仅应用当前 option，检查 skipTools 标志
		probe := &SubAgent{}
		opt(probe)
		if probe.skipTools {
			return true
		}
	}
	return false
}
