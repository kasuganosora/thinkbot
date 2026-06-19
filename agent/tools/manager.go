package tools

import (
	"context"
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
	mu          sync.RWMutex
	registry    *ToolRegistry
	promptMgr   *ToolPromptManager
	promptReg   *prompt.Registry
	logger      *zap.SugaredLogger
	headerSec   *ToolPromptSection
	rulesSec    *ToolPromptSection

	// 是否在注册工具时自动生成描述段落
	autoDescribe bool
}

// NewToolManager 创建工具管理器。
//
// promptReg 是 prompt 模块的 Registry，工具提示词段落会注册到这里。
func NewToolManager(promptReg *prompt.Registry, logger *zap.SugaredLogger) *ToolManager {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &ToolManager{
		registry:    NewToolRegistry(),
		promptMgr:   NewToolPromptManager(promptReg, "tool_"),
		promptReg:   promptReg,
		logger:      logger.With("component", "tool_manager"),
		autoDescribe: true,
	}
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

// ResolveTools 解析当前会话上下文下的工具列表。
// 返回可供 LLM 使用的 []llm.Tool。
func (m *ToolManager) ResolveTools(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
	return m.registry.Resolve(ctx, sctx)
}

// ResolveForEnvelope 从 Envelope 构建会话上下文并解析工具。
// 返回工具列表，如果没有任何工具返回 nil。
func (m *ToolManager) ResolveForEnvelope(ctx context.Context, env *core.Envelope) ([]llm.Tool, error) {
	sctx := envelopeToSessionContext(env)
	tools, err := m.registry.Resolve(ctx, sctx)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return nil, nil
	}
	return tools, nil
}

// StaticCount 返回静态注册的工具数量。
func (m *ToolManager) StaticCount() int {
	return m.registry.StaticCount()
}

// ProviderCount 返回动态提供者数量。
func (m *ToolManager) ProviderCount() int {
	return m.registry.ProviderCount()
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

	return sctx
}


