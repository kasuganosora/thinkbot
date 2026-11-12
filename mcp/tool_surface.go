package mcp

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

var (
	errNoSurface    = errors.New("mcp: tool surface is not configured")
	errDisconnected = errors.New("browser mcp disconnected")
)

// ToolSurface 在连接成功后把 MCP 工具挂到 ToolManager，并保证只挂一次 provider。
// 断线时清空快照：已解析出的闭包会返回明确错误，而不是继续拿着旧客户端。
type ToolSurface struct {
	mu       sync.Mutex
	toolMgr  *tools.ToolManager
	provider *surfaceProvider
	added    bool
	installs int
	names    []string
	ready    atomic.Bool
}

type surfaceProvider struct {
	mu    sync.RWMutex
	tools []llm.Tool
}

// NewToolSurface 创建一个尚未注册的工具面。toolMgr 为 nil 时 Install 会失败。
func NewToolSurface(toolMgr *tools.ToolManager) *ToolSurface {
	return &ToolSurface{toolMgr: toolMgr}
}

// Install 拉取当前已连接服务器的工具并替换快照。并发调用串行化，provider 只添加一次。
func (s *ToolSurface) Install(ctx context.Context, mgr *Manager) error {
	if s == nil || s.toolMgr == nil || mgr == nil {
		return errNoSurface
	}
	listed, err := mgr.ListAllTools(ctx)
	if err != nil {
		return err
	}
	var built []llm.Tool
	seen := make(map[string]bool)
	for serverName, mcpTools := range listed {
		for _, t := range mcpTools {
			lt := mcpToolToLLM(t, mgr, serverName)
			if lt.Name == "" || seen[lt.Name] {
				continue
			}
			seen[lt.Name] = true
			orig := lt.Execute
			lt.Execute = func(ctx *llm.ToolExecContext, input any) (any, error) {
				if !s.ready.Load() {
					return nil, errDisconnected
				}
				if orig == nil {
					return nil, errDisconnected
				}
				return orig(ctx, input)
			}
			built = append(built, lt)
		}
	}
	sort.Slice(built, func(i, j int) bool { return built[i].Name < built[j].Name })

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.added {
		s.provider = &surfaceProvider{}
		s.toolMgr.AddProvider(s.provider)
		ensureMCPRules(s.toolMgr)
		s.added = true
	}
	names := make([]string, len(built))
	for i := range built {
		names[i] = built[i].Name
	}
	s.provider.set(built)
	s.names = names
	s.installs++
	s.ready.Store(true)
	return nil
}

// Clear 使工具立即不可用。下一次 Resolve 不再列出它们，已发出的调用返回断线错误。
// ready 在清掉快照之后才落下，避免和 Install 交错时把「已清空」又标成可用。
func (s *ToolSurface) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil {
		s.provider.set(nil)
	}
	s.names = nil
	s.ready.Store(false)
}

// Ready 报告最近一次成功 Install 之后是否仍可用。
func (s *ToolSurface) Ready() bool {
	return s != nil && s.ready.Load()
}

// InstallCount 是成功 Install 的次数（含重连后的重新注册）。
func (s *ToolSurface) InstallCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installs
}

// Names 返回当前快照里的工具名（按名称排序的副本）。
func (s *ToolSurface) Names() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.names) == 0 {
		return nil
	}
	out := make([]string, len(s.names))
	copy(out, s.names)
	return out
}

func (p *surfaceProvider) set(tools []llm.Tool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(tools) == 0 {
		p.tools = nil
		return
	}
	p.tools = tools
}

// Tools 实现 tools.ToolProvider。未连接时返回空列表，不把过期工具留给模型。
func (p *surfaceProvider) Tools(context.Context, *tools.ToolSessionContext) ([]llm.Tool, error) {
	if p == nil {
		return nil, nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.tools) == 0 {
		return nil, nil
	}
	out := make([]llm.Tool, len(p.tools))
	copy(out, p.tools)
	return out, nil
}
