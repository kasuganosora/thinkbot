package tools

import (
	"context"
	"sort"
	"sync"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ToolManager — 工具管理器（统一入口）
// ============================================================================

// ToolManager 整合 ToolRegistry + ToolPromptManager，
// 是 Bot 使用工具的统一入口。
//
// 职责：
//   - 注册/管理工具（静态 + 动态）
//   - 管理工具提示词（自动同步到 prompt.Registry）
//   - 为 Pipeline Stage 提供工具列表解析能力
//   - 管理工具提示词的全局段落（header + rules）
type ToolManager struct {
	mu        sync.RWMutex
	registry  *ToolRegistry
	promptMgr *ToolPromptManager
	promptReg *prompt.Registry
	logger    *zap.SugaredLogger
	headerSec *ToolPromptSection
	rulesSec  *ToolPromptSection

	// policyProvider 工具权限策略提供者（nil 表示不做策略过滤）。
	// 构造时从 PolicyStore 自动接入，运行时实时读取。
	policyProvider ToolPolicyProvider

	// accessEval 基于完整会话上下文的工具访问控制（nil 表示不启用）。
	// 若设置，ResolveTools 优先使用它（取代 legacy policyProvider），
	// 因为它能感知 platform（SourceChannelType）与 userID，匹配 bot 维度的
	// 工具权限表（bot_tool_permissions）。
	accessEval ToolAccessEvaluator

	// 是否在注册工具时自动生成描述段落
	autoDescribe bool
}

// NewToolManager 创建工具管理器。
//
// promptReg 是 prompt 模块的 Registry，工具提示词段落会注册到这里。
// store 是配置存储（通常 *config.Store），用于自动加载工具权限策略。
// 传 nil 则不做策略过滤（全部工具可用）。
func NewToolManager(promptReg *prompt.Registry, store PolicyStore, logger *zap.SugaredLogger) *ToolManager {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	m := &ToolManager{
		registry:     NewToolRegistry(),
		promptMgr:    NewToolPromptManager(promptReg, "tool_"),
		promptReg:    promptReg,
		logger:       logger.With("component", "tool_manager"),
		autoDescribe: true,
	}

	// 自动接入策略：从 store 实时读取，无需手动调用 SetPolicyProvider
	if store != nil {
		m.policyProvider = NewStorePolicyProvider(store)
	}

	return m
}

// Registry 返回内部 ToolRegistry（高级用法）。
func (m *ToolManager) Registry() *ToolRegistry {
	return m.registry
}

// Register 注册一个工具并同步其提示词段落。
func (m *ToolManager) Register(def ToolDef) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.registry.Register(def); err != nil {
		return err
	}

	// 注册自定义提示词段落
	if def.PromptSection != nil {
		m.promptMgr.RegisterToolPrompt(def.PromptSection)
	}

	// 自动生成描述段落
	if m.autoDescribe {
		desc := BuildToolDescriptionSection(&def)
		if desc != nil {
			m.promptMgr.RegisterToolPrompt(desc)
		}
	}

	m.logger.Debugw("tool registered",
		"tool", def.Name,
		"category", def.Category,
		"scopes", def.Scopes,
	)

	return nil
}

// RegisterMany 批量注册工具。
func (m *ToolManager) RegisterMany(defs ...ToolDef) error {
	for _, def := range defs {
		if err := m.Register(def); err != nil {
			return err
		}
	}
	return nil
}

// Unregister 注销工具并移除其提示词段落。
func (m *ToolManager) Unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.registry.Unregister(name)

	// 移除提示词段落（自定义 + 自动描述）
	m.promptReg.Unregister(m.promptMgr.prefix + name)
	m.promptReg.Unregister(m.promptMgr.prefix + name + "_desc")
}

// AddProvider 添加动态工具提供者。
func (m *ToolManager) AddProvider(p ToolProvider) {
	m.registry.AddProvider(p)
}

// SetHeaderSection 设置工具总标题段落（Order=300）。
func (m *ToolManager) SetHeaderSection(section *ToolPromptSection) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 移除旧的
	if m.headerSec != nil {
		m.promptReg.Unregister(m.promptMgr.prefix + m.headerSec.Name)
	}

	m.headerSec = section
	if section != nil {
		m.promptMgr.RegisterToolPrompt(section)
	}
}

// SetRulesSection 设置工具通用规则段落（Order=301）。
func (m *ToolManager) SetRulesSection(section *ToolPromptSection) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.rulesSec != nil {
		m.promptReg.Unregister(m.promptMgr.prefix + m.rulesSec.Name)
	}

	m.rulesSec = section
	if section != nil {
		m.promptMgr.RegisterToolPrompt(section)
	}
}

// EnableAutoDescribe 开启/关闭自动生成工具描述段落。
func (m *ToolManager) EnableAutoDescribe(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoDescribe = enabled
}

// SetPolicyProvider 设置工具权限策略提供者（高级覆盖）。
// 通常构造时已从 PolicyStore 自动接入，无需手动调用。
// 仅在需要自定义策略来源（非 config.Store）时使用。
// 传入 nil 可取消策略过滤。
func (m *ToolManager) SetPolicyProvider(pp ToolPolicyProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policyProvider = pp
}

// ToolAccessEvaluator 是基于完整会话上下文的工具访问控制。
// 相比旧 ToolPolicyProvider（只看 channel/chatType），它能感知
// platform（SourceChannelType）与 userID，用于匹配 bot 维度的工具权限表。
type ToolAccessEvaluator interface {
	// FilterTools 按当前会话上下文过滤工具列表，仅返回允许使用的工具。
	FilterTools(ctx context.Context, tools []llm.Tool, sctx *ToolSessionContext) ([]llm.Tool, error)
}

// SetAccessEvaluator 设置基于完整上下文的工具访问控制。
// 设置后，ResolveTools 优先使用它（取代 legacy policyProvider）。
// 传入 nil 可取消。
func (m *ToolManager) SetAccessEvaluator(e ToolAccessEvaluator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accessEval = e
}

// ResolveTools 解析当前会话上下文下的工具列表。
// 返回可供 LLM 使用的 []llm.Tool。
// 若设置了 ToolAccessEvaluator，按完整上下文过滤；否则回退到 legacy ToolPolicyProvider。
func (m *ToolManager) ResolveTools(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
	tools, err := m.registry.Resolve(ctx, sctx)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return tools, nil
	}

	m.mu.RLock()
	ae := m.accessEval
	pp := m.policyProvider
	m.mu.RUnlock()

	if ae != nil {
		return ae.FilterTools(ctx, tools, sctx)
	}
	if pp != nil {
		policy := pp.GetPolicy(sctx.BotID)
		tools = policy.FilterTools(tools, sctx)
	}

	return tools, nil
}

// ResolveForEnvelope 从 Envelope 构建会话上下文并解析工具。
// 返回工具列表，如果没有任何工具返回 nil。
func (m *ToolManager) ResolveForEnvelope(ctx context.Context, env *core.Envelope) ([]llm.Tool, error) {
	sctx := envelopeToSessionContext(env)
	return m.ResolveTools(ctx, sctx)
}

// StaticCount 返回静态注册的工具数量。
func (m *ToolManager) StaticCount() int {
	return m.registry.StaticCount()
}

// ProviderCount 返回动态提供者数量。
func (m *ToolManager) ProviderCount() int {
	return m.registry.ProviderCount()
}

// ListTools 返回所有已注册静态工具的详情快照（按名称排序）。
//
// 返回的 ToolInfo 包含名称、描述、分类、适用场景等元数据，
// 适合用于调试输出、工具列表展示或自省。
// 仅包含静态注册的工具，不包括动态 ToolProvider 在运行时提供的工具。
func (m *ToolManager) ListTools() []ToolInfo {
	defs := m.registry.ListStatic()
	result := make([]ToolInfo, 0, len(defs))
	for i := range defs {
		d := &defs[i]
		result = append(result, ToolInfo{
			Name:             d.Name,
			Description:      d.Description,
			Category:         d.Category,
			Scopes:           d.Scopes,
			RequireApproval:  d.RequireApproval,
			HasPromptSection: d.PromptSection != nil,
			Parameters:       d.Parameters,
		})
	}
	return result
}

// ListAllTools 返回静态工具 + 动态 provider 工具的合并快照（按名称排序）。
//
// 用于工具权限管理界面：动态工具（MCP / 浏览器等）在 ResolveTools 中同样会过
// toolperm 评估，若管理界面看不到它们，管理员就无法为其配置允许/禁止规则 ——
// 表现为「工具确实受管控，但 UI 上无从下手」。
//
// 动态工具只有 llm.Tool（名称/描述/参数），没有 Category / Scopes 等静态元数据，
// 因此 Category 统一置为 dynamicCategory，由 API 层映射为中文分组名。
// 同名时静态工具优先（与 Resolve 的去重语义保持一致）。
func (m *ToolManager) ListAllTools(ctx context.Context) []ToolInfo {
	result := m.ListTools()
	staticNames := make(map[string]bool, len(result))
	for i := range result {
		staticNames[result[i].Name] = true
	}

	// sctx 传空上下文：管理界面要看到「全部挂载的动态工具」，
	// 而非某个具体会话可见的子集。
	for _, t := range m.registry.ListDynamic(ctx, &ToolSessionContext{}) {
		if staticNames[t.Name] {
			continue
		}
		result = append(result, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			Category:    DynamicCategory,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// envelopeToSessionContext 从 Pipeline Envelope 构建工具会话上下文。
func envelopeToSessionContext(env *core.Envelope) *ToolSessionContext {
	sctx := &ToolSessionContext{
		BotID:     env.Message.BotID,
		Channel:   env.Message.Channel,
		ChatType:  env.Message.ChatType,
		UserID:    env.Message.UserID,
		MessageID: env.Message.ID,
	}

	// 从 Envelope KV 读取额外信息
	if v, ok := env.Get("bot.id"); ok {
		if s, ok := v.(string); ok {
			sctx.BotID = s
		}
	}
	if v, ok := env.Get("subagent.active"); ok {
		if b, ok := v.(bool); ok {
			sctx.IsSubagent = b
		}
	}

	// 从消息 Metadata 读取 channel_type（由 Channel 在 handleUpdate/handleNote 中注入）
	if env.Message.Metadata != nil {
		if ct, ok := env.Message.Metadata["channel_type"].(string); ok {
			sctx.SourceChannelType = ct
		}
		// 前端会话 ID（web 渠道注入）。搬进 Extra 供工具按会话归属做决策，
		// 例如工作流提交时记录来源会话，使前端刷新后能恢复工作流卡片。
		if sid, ok := env.Message.Metadata[ExtraKeyChatSessionID].(string); ok && sid != "" {
			if sctx.Extra == nil {
				sctx.Extra = make(map[string]any, 1)
			}
			sctx.Extra[ExtraKeyChatSessionID] = sid
		}
	}

	return sctx
}
