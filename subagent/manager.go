package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

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
	mu              sync.Mutex
	provider        llm.Provider // 从主 Agent 继承
	model           string       // 从主 Agent 继承
	subagents       map[string]*SubAgent
	counter         int64
	defaultOpts     []Option // 所有 SubAgent 默认继承的选项
	delegateTimeout time.Duration

	// 并发控制
	maxConcurrency int // DelegateMany 的最大并发数（0=不限制）
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
		maxConcurrency:  2,
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
// 参数：
//   - WithStuckTimeout(d)：覆盖卡死阈值（默认 180s）；WithCallTimeout 对本方法无效。
//
// 返回完整文本；被看门狗终止时返回带 stuck/hard 区分的错误。
func (m *SubAgentManager) DelegateStream(ctx context.Context, systemPrompt, task string, opts ...Option) (string, error) {
	_, _, defaultOpts := m.snapshotConfig()
	allOpts := mergeOptionLists(defaultOpts, systemPrompt, opts...)
	sa := New(m.provider, m.model, allOpts...)
	defer sa.Close()

	// 解析卡死阈值（stuck）与硬上限（hard = stuck × factor，派生，不写死）
	stuck := sa.stuckTimeout
	if stuck <= 0 {
		stuck = defaultDelegateStuckTimeout
	}
	hard := stuck * delegateHardTimeoutFactor

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := sa.Stream(streamCtx, task)
	if err != nil {
		return "", errs.Wrapf(err, "subagent stream failed")
	}

	var (
		lastActivity int64 // atomic: 上次收到 token 的 unix nano
		gotFirst     int32 // atomic: 是否已收到首个 token
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
				// 注意：基于「总时长」而非「空闲时长」——持续吐 token 的模型不会触发卡死，
				// 但若永远不结束（idle 始终很小），靠总时长硬上限强制终止。
				if elapsed > hard {
					killReason = "hard"
					cancel()
					return
				}
				// 首 token 宽限期内不判卡死：LLM 读长输入 + 推理阶段可能长时间无输出
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
		switch p := part.(type) {
		case *llm.TextDeltaPart:
			// token 到达 = 活跃信号
			atomic.StoreInt64(&lastActivity, time.Now().UnixNano())
			atomic.StoreInt32(&gotFirst, 1)
			textBuf.WriteString(p.Text)
		case *llm.ReasoningDeltaPart:
			// 推理 token 也算活跃信号
			atomic.StoreInt64(&lastActivity, time.Now().UnixNano())
			atomic.StoreInt32(&gotFirst, 1)
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
		return "", errs.Newf("subagent LLM 卡死：连续 %s 无 token 输出（卡死看门狗终止）", stuck)
	case killReason == "hard":
		return "", errs.Newf("subagent LLM 超过硬上限 %s 被强制终止（看门狗兜底）", hard)
	case streamErr != nil:
		return "", errs.Wrapf(streamErr, "subagent stream failed")
	}
	return textBuf.String(), nil
}
// 每个任务在独立的 SubAgent 中执行，互不影响。
// 返回每个任务的结果（顺序与输入一致）。
func (m *SubAgentManager) DelegateMany(ctx context.Context, systemPrompt string, tasks []string, opts ...Option) []TaskResult {
	// 快照配置（线程安全）
	timeout, maxConc, defaultOpts := m.snapshotConfig()

	// 预计算合并选项（WithCallTimeout 可能覆盖默认超时）
	allOpts := mergeOptionLists(defaultOpts, systemPrompt, opts...)

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

			// 每个 SubAgent 有独立的超时上下文
			taskCtx := ctx
			if effectiveTimeout > 0 {
				var cancel context.CancelFunc
				taskCtx, cancel = context.WithTimeout(ctx, effectiveTimeout)
				defer cancel()
			}

			sa := New(m.provider, m.model, allOpts...)
			defer sa.Close()

			reply, err := sa.Chat(taskCtx, t)
			if err != nil {
				results[idx] = TaskResult{
					Task:    t,
					Success: false,
					Error:   err.Error(),
				}
				return
			}
			results[idx] = TaskResult{
				Task:    t,
				Text:    reply,
				Success: true,
			}
		}(i, task)
	}

	wg.Wait()
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
