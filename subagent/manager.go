package subagent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
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

// Delegate 创建一个临时 SubAgent，执行任务后自动关闭。
// 这是一次性委托模式，适合不需要多轮交互的场景。
func (m *SubAgentManager) Delegate(ctx context.Context, systemPrompt, task string, opts ...Option) (string, error) {
	timeout, _, defaultOpts := m.snapshotConfig()

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	allOpts := mergeOptionLists(defaultOpts, systemPrompt, opts...)

	sa := New(m.provider, m.model, allOpts...)
	defer sa.Close()

	return sa.Chat(ctx, task)
}

// DelegateMany 并发执行多个一次性委托任务。
// 每个任务在独立的 SubAgent 中执行，互不影响。
// 返回每个任务的结果（顺序与输入一致）。
func (m *SubAgentManager) DelegateMany(ctx context.Context, systemPrompt string, tasks []string, opts ...Option) []TaskResult {
	// 快照配置（线程安全）
	timeout, maxConc, defaultOpts := m.snapshotConfig()

	// 预计算合并选项，避免在每个 goroutine 中重复
	allOpts := mergeOptionLists(defaultOpts, systemPrompt, opts...)

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
			if timeout > 0 {
				var cancel context.CancelFunc
				taskCtx, cancel = context.WithTimeout(ctx, timeout)
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
