package tools

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/subagent"
)

// ============================================================================
// SubAgentManager — 管理 SubAgent 生命周期
//
// SubAgentManager 让主 Agent 通过工具调用创建和管理 SubAgent。
// 每个被委托的任务在完全隔离的上下文中执行，不会污染主 Agent 的对话历史。
//
// 两种使用模式：
//   - Delegate（一次性）：创建临时 SubAgent → 执行任务 → 返回结果 → 自动关闭
//   - Spawn/Chat/Close（持久化）：创建有状态的 SubAgent → 多轮对话 → 手动关闭
// ============================================================================

// SubAgentManager 管理主 Agent 可调用的 SubAgent 实例。
type SubAgentManager struct {
	mu          sync.Mutex
	provider    llm.Provider // 从主 Agent 继承
	model       string       // 从主 Agent 继承
	subagents   map[string]*subagent.SubAgent
	counter     int64
	defaultOpts []subagent.Option // 所有 SubAgent 默认继承的选项
	delegateTimeout time.Duration  // delegate 超时（0=不超时）
}

// SubAgentInfo 描述一个活跃的持久化 SubAgent。
type SubAgentInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Turns int    `json:"turns"`
}

// NewSubAgentManager 创建 SubAgent 管理器。
//
// provider 和 model 从主 Agent 的 LLMBundle 继承。
// defaultOpts 是所有 SubAgent 默认继承的配置（如温度、滑动窗口大小）。
func NewSubAgentManager(provider llm.Provider, model string, defaultOpts ...subagent.Option) *SubAgentManager {
	return &SubAgentManager{
		provider:       provider,
		model:          model,
		subagents:      make(map[string]*subagent.SubAgent),
		defaultOpts:    defaultOpts,
		delegateTimeout: 120 * time.Second,
	}
}

// SetDelegateTimeout 设置 delegate 工具的超时时间。
func (m *SubAgentManager) SetDelegateTimeout(d time.Duration) {
	m.delegateTimeout = d
}

// Delegate 创建一个临时 SubAgent，执行任务后自动关闭。
// 这是一次性委托模式，适合不需要多轮交互的场景。
func (m *SubAgentManager) Delegate(ctx context.Context, systemPrompt, task string, opts ...subagent.Option) (string, error) {
	if m.delegateTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.delegateTimeout)
		defer cancel()
	}

	allOpts := m.mergeOpts(systemPrompt, opts...)

	sa := subagent.New(m.provider, m.model, allOpts...)
	defer sa.Close()

	return sa.Chat(ctx, task)
}

// Spawn 创建一个持久化 SubAgent，返回其 ID。
// 该 SubAgent 会维护自己的对话上下文，适合需要多轮交互的场景。
func (m *SubAgentManager) Spawn(systemPrompt, name string, opts ...subagent.Option) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := fmt.Sprintf("sa-%d", atomic.AddInt64(&m.counter, 1))

	allOpts := make([]subagent.Option, 0, len(m.defaultOpts)+len(opts)+2)
	allOpts = append(allOpts, m.defaultOpts...)
	allOpts = append(allOpts, subagent.WithID(id))
	if name != "" {
		allOpts = append(allOpts, subagent.WithName(name))
	}
	if systemPrompt != "" {
		allOpts = append(allOpts, subagent.WithSystemPrompt(systemPrompt))
	}
	allOpts = append(allOpts, opts...)

	sa := subagent.New(m.provider, m.model, allOpts...)
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
	defer m.mu.Unlock()

	result := make([]SubAgentInfo, 0, len(m.subagents))
	for id, sa := range m.subagents {
		result = append(result, SubAgentInfo{
			ID:    id,
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

// mergeOpts 合并默认选项、系统提示词和额外选项。
func (m *SubAgentManager) mergeOpts(systemPrompt string, opts ...subagent.Option) []subagent.Option {
	allOpts := make([]subagent.Option, 0, len(m.defaultOpts)+len(opts)+1)
	allOpts = append(allOpts, m.defaultOpts...)
	if systemPrompt != "" {
		allOpts = append(allOpts, subagent.WithSystemPrompt(systemPrompt))
	}
	allOpts = append(allOpts, opts...)
	return allOpts
}

// ============================================================================
// Tool Definitions — LLM 可调用的工具
// ============================================================================

// subAgentToolPromptSection 是所有 SubAgent 工具共享的提示词段落。
var subAgentToolPromptSection = &ToolPromptSection{
	Name:  "subagent_tools",
	Order: 305,
	Content: `# 子 Agent 委托

你可以通过子 Agent 工具将任务委托给拥有独立上下文的子 Agent 执行。

## 何时使用

- **任务复杂、需要大量中间推理**：委托给子 Agent 可以避免中间步骤污染你的对话上下文
- **需要不同角色/视角**：为子 Agent 设置专门的系统提示词（如"你是安全审计专家"）
- **需要隔离上下文**：子 Agent 的对话历史与你完全隔离

## 工具说明

- **delegate**：一次性委托。创建子 Agent → 执行任务 → 返回结果 → 自动销毁。适合简单任务。
- **spawn_subagent**：创建持久化子 Agent，返回 ID。适合需要多轮交互的复杂任务。
- **chat_subagent**：向持久化子 Agent 发送消息。
- **close_subagent**：关闭不再需要的持久化子 Agent。
- **list_subagents**：查看当前活跃的子 Agent 列表。

## 使用原则

- 简单任务用 delegate（一次性），复杂多步任务用 spawn + chat
- 使用完持久化子 Agent 后，记得调用 close_subagent 释放资源
- 在 system_prompt 参数中清晰描述子 Agent 的角色和职责
- 不要过度委托——如果你自己能轻松回答，直接回答即可`,
	Enabled: true,
}

// DelegateToolDef 返回一次性委托工具。
// 创建临时 SubAgent 执行任务，完成后自动关闭。
func DelegateToolDef(mgr *SubAgentManager) ToolDef {
	return ToolDef{
		Category: "subagent",
		Scopes:   []string{"private", "group"},
		Tool: buildTool(
			"delegate",
			"将一个任务一次性委托给拥有独立上下文的子 Agent 执行。子 Agent 会用给定的系统提示词处理任务，返回结果后自动销毁。适合不需要多轮交互的场景。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"description": "要委托给子 Agent 执行的任务。应当是完整、自包含的描述，包含所有必要的上下文和背景信息。",
					},
					"system_prompt": map[string]any{
						"type":        "string",
						"description": "子 Agent 的系统提示词，定义其角色、专业领域和行为规范。例如：\"你是一个专业的代码审查专家，专注于发现潜在的安全漏洞和逻辑错误。\"如果留空，子 Agent 将使用通用助手角色。",
					},
				},
				"required": []string{"task"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				task, _ := m["task"].(string)
				if task == "" {
					return nil, fmt.Errorf("task is required")
				}
				systemPrompt, _ := m["system_prompt"].(string)

				reply, err := mgr.Delegate(ctx, systemPrompt, task)
				if err != nil {
					return map[string]any{
						"success": false,
						"error":   err.Error(),
					}, nil
				}
				return map[string]any{
					"success": true,
					"result":  reply,
				}, nil
			},
		),
		PromptSection: subAgentToolPromptSection,
	}
}

// SpawnSubAgentToolDef 返回创建持久化子 Agent 的工具。
func SpawnSubAgentToolDef(mgr *SubAgentManager) ToolDef {
	return ToolDef{
		Category: "subagent",
		Scopes:   []string{"private", "group"},
		Tool: buildTool(
			"spawn_subagent",
			"创建一个持久化的子 Agent，返回其 ID。该子 Agent 会维护独立的对话上下文，适合需要多轮交互的复杂任务。使用完毕后请调用 close_subagent 释放资源。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"system_prompt": map[string]any{
						"type":        "string",
						"description": "子 Agent 的系统提示词，定义其角色和行为。例如：\"你是一个 Python 性能优化专家。\"",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "子 Agent 的显示名称（可选）。便于在 list_subagents 中识别。",
					},
				},
				"required": []string{"system_prompt"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				systemPrompt, _ := m["system_prompt"].(string)
				name, _ := m["name"].(string)

				id, err := mgr.Spawn(systemPrompt, name)
				if err != nil {
					return map[string]any{
						"success": false,
						"error":   err.Error(),
					}, nil
				}
				return map[string]any{
					"success": true,
					"id":      id,
					"name":    name,
					"message": "子 Agent 已创建，使用 chat_subagent 发送消息，完成后用 close_subagent 关闭。",
				}, nil
			},
		),
	}
}

// ChatSubAgentToolDef 返回与持久化子 Agent 对话的工具。
func ChatSubAgentToolDef(mgr *SubAgentManager) ToolDef {
	return ToolDef{
		Category: "subagent",
		Scopes:   []string{"private", "group"},
		Tool: buildTool(
			"chat_subagent",
			"向已存在的持久化子 Agent 发送消息并获取回复。子 Agent 会维护自己的对话上下文，支持多轮交互。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "子 Agent 的 ID（由 spawn_subagent 返回）。",
					},
					"message": map[string]any{
						"type":        "string",
						"description": "要发送给子 Agent 的消息内容。",
					},
				},
				"required": []string{"id", "message"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				id, _ := m["id"].(string)
				message, _ := m["message"].(string)
				if id == "" || message == "" {
					return nil, fmt.Errorf("id and message are required")
				}

				reply, turns, err := mgr.Chat(ctx, id, message)
				if err != nil {
					return map[string]any{
						"success": false,
						"error":   err.Error(),
					}, nil
				}
				return map[string]any{
					"success": true,
					"reply":   reply,
					"turns":   turns,
				}, nil
			},
		),
	}
}

// CloseSubAgentToolDef 返回关闭持久化子 Agent 的工具。
func CloseSubAgentToolDef(mgr *SubAgentManager) ToolDef {
	return ToolDef{
		Category: "subagent",
		Scopes:   []string{"private", "group"},
		Tool: buildTool(
			"close_subagent",
			"关闭一个持久化子 Agent，释放其资源。关闭后该子 Agent 的上下文将被丢弃，无法再发送消息。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "要关闭的子 Agent ID。",
					},
				},
				"required": []string{"id"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				id, _ := m["id"].(string)
				if id == "" {
					return nil, fmt.Errorf("id is required")
				}

				if err := mgr.Close(id); err != nil {
					return map[string]any{
						"success": false,
						"error":   err.Error(),
					}, nil
				}
				return map[string]any{
					"success": true,
					"id":      id,
					"message": "子 Agent 已关闭，上下文已释放。",
				}, nil
			},
		),
	}
}

// ListSubAgentsToolDef 返回列出活跃子 Agent 的工具。
func ListSubAgentsToolDef(mgr *SubAgentManager) ToolDef {
	return ToolDef{
		Category: "subagent",
		Scopes:   []string{"private", "group"},
		Tool: buildTool(
			"list_subagents",
			"列出当前所有活跃的持久化子 Agent。返回每个子 Agent 的 ID、名称和对话轮数。",
			map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				list := mgr.List()
				if len(list) == 0 {
					return map[string]any{
						"success":  true,
						"count":    0,
						"subagents": []SubAgentInfo{},
						"message":  "当前没有活跃的子 Agent。",
					}, nil
				}
				return map[string]any{
					"success":   true,
					"count":     len(list),
					"subagents": list,
				}, nil
			},
		),
	}
}

// ============================================================================
// 便捷注册函数
// ============================================================================

// RegisterSubAgentTools 将所有子 Agent 工具注册到 ToolManager。
//
// 使用示例：
//
//	saMgr := tools.NewSubAgentManager(bundle.Main, bundle.MainDef.Model)
//	tools.RegisterSubAgentTools(toolMgr, saMgr)
//
// 然后在 Bot 启动时调用 saMgr.CloseAll() 清理资源。
func RegisterSubAgentTools(mgr *ToolManager, saMgr *SubAgentManager) error {
	return mgr.RegisterMany(
		DelegateToolDef(saMgr),
		SpawnSubAgentToolDef(saMgr),
		ChatSubAgentToolDef(saMgr),
		CloseSubAgentToolDef(saMgr),
		ListSubAgentsToolDef(saMgr),
	)
}
