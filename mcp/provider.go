package mcp

import (
	"context"
	"sync"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Provider — 将 MCP 工具适配为 tools.ToolProvider
// ============================================================================

// Provider 实现 tools.ToolProvider 接口，
// 在每次 Resolve 时从所有已连接的 MCP 服务器动态获取工具列表。
type Provider struct {
	manager *Manager
	logger  *zap.SugaredLogger

	// 工具列表缓存（避免每次 Resolve 都重新请求 MCP 服务器）
	mu          sync.RWMutex
	cache       []llm.Tool
	cacheDirty  bool
}

// NewProvider 从 MCP Manager 创建一个 ToolProvider。
func NewProvider(mgr *Manager) *Provider {
	return &Provider{
		manager:    mgr,
		logger:     mgr.logger,
		cacheDirty: true, // 初始需要刷新
	}
}

// Tools 实现 tools.ToolProvider 接口。
// 返回所有已连接 MCP 服务器的工具列表。
func (p *Provider) Tools(ctx context.Context, sctx *tools.ToolSessionContext) ([]llm.Tool, error) {
	// SubAgent 场景不暴露 MCP 工具
	if sctx != nil && sctx.IsSubagent {
		return nil, nil
	}

	// 使用缓存（如果可用）
	if cached := p.getCached(); cached != nil {
		return cached, nil
	}

	// 刷新工具列表
	llmTools, err := p.refresh(ctx)
	if err != nil {
		return nil, err
	}
	return llmTools, nil
}

// getCached 返回缓存的工具列表，如果缓存无效返回 nil。
func (p *Provider) getCached() []llm.Tool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.cacheDirty && len(p.cache) > 0 {
		return p.cache
	}
	return nil
}

// InvalidateCache 标记缓存失效，下次 Tools() 调用会重新获取。
func (p *Provider) InvalidateCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cacheDirty = true
}

// refresh 从所有已连接的 MCP 服务器获取最新工具列表并更新缓存。
func (p *Provider) refresh(ctx context.Context) ([]llm.Tool, error) {
	serverTools, err := p.manager.ListAllTools(ctx)
	if err != nil {
		return nil, err
	}

	var result []llm.Tool
	for serverName, mcpTools := range serverTools {
		client, ok := p.manager.GetClient(serverName)
		if !ok {
			continue
		}
		for _, t := range mcpTools {
			result = append(result, mcpToolToLLM(t, client, serverName))
		}
	}

	p.mu.Lock()
	p.cache = result
	p.cacheDirty = false
	p.mu.Unlock()

	p.logger.Debugw("mcp tools refreshed",
		"total_tools", len(result),
		"servers", len(serverTools))
	return result, nil
}

// ============================================================================
// RegisterTools — 将 MCP Provider 注册到 ToolManager
// ============================================================================

// RegisterTools 创建一个 MCP Provider 并注册到 ToolManager。
// 注册后，所有已连接 MCP 服务器的工具会自动出现在 LLM 的工具列表中。
//
// 同时注册一个工具提示词段落，告知 LLM 可用 MCP 工具的存在。
func RegisterTools(toolMgr *tools.ToolManager, mgr *Manager) error {
	if mgr == nil {
		return nil
	}
	provider := NewProvider(mgr)
	mgr.SetOnServerChange(provider.InvalidateCache) // 服务器开关时自动失效缓存
	toolMgr.AddProvider(provider)

	// 注册提示词段落
	toolMgr.SetRulesSection(&tools.ToolPromptSection{
		Name:  "mcp_rules",
		Order: 305,
		Content: `## MCP 工具

部分工具来自外部 MCP 服务器（名称格式: mcp__<server>__<tool>）。
这些工具的参数 schema 由 MCP 服务器定义，请严格按照 schema 调用。
MCP 工具的执行结果为纯文本格式。`,
		Enabled: true,
	})

	return nil
}
