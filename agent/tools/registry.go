package tools

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ToolRegistry — 工具注册中心
// ============================================================================

// ToolRegistry 是工具的线程安全注册中心。
// 管理静态工具（ToolDef）和动态工具提供者（ToolProvider）。
type ToolRegistry struct {
	mu        sync.RWMutex
	tools     map[string]*ToolDef // name → definition
	providers []ToolProvider      // 动态提供者
}

// NewToolRegistry 创建空的注册中心。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]*ToolDef),
	}
}

// Register 注册一个静态工具。
// 如果 name 已存在则覆盖。
func (r *ToolRegistry) Register(def ToolDef) error {
	if def.Name == "" {
		return fmt.Errorf("tools: tool name cannot be empty")
	}
	if def.Description == "" {
		return fmt.Errorf("tools: tool %q: description cannot be empty", def.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[def.Name] = &def
	return nil
}

// RegisterMany 批量注册工具。遇到错误时停止，已注册的保留。
func (r *ToolRegistry) RegisterMany(defs ...ToolDef) error {
	for _, def := range defs {
		if err := r.Register(def); err != nil {
			return err
		}
	}
	return nil
}

// Unregister 注销指定名称的工具。
func (r *ToolRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// AddProvider 添加一个动态工具提供者。
// 提供者在 Resolve() 时被调用，返回的工具会与静态工具合并。
func (r *ToolRegistry) AddProvider(p ToolProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, p)
}

// Get 获取指定名称的静态工具定义。
func (r *ToolRegistry) Get(name string) (ToolDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.tools[name]
	if !ok {
		return ToolDef{}, false
	}
	return *def, true
}

// ListStatic 返回所有已注册的静态工具定义（按名称排序）。
func (r *ToolRegistry) ListStatic() []ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]ToolDef, 0, len(r.tools))
	for _, def := range r.tools {
		result = append(result, *def)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// ProviderCount 返回动态提供者数量。
func (r *ToolRegistry) ProviderCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}

// StaticCount 返回静态工具数量。
func (r *ToolRegistry) StaticCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// ============================================================================
// Resolution — 工具列表解析（静态 + 动态）
// ============================================================================

// Resolve 解析当前会话上下文下所有可用的工具列表。
//
// 解析流程：
//  1. 收集静态工具，过滤不适用场景的（Scopes）
//  2. 调用所有 ToolProvider 收集动态工具
//  3. 合并去重（同名的静态工具优先于动态工具）
//  4. 按 Name 排序返回
func (r *ToolRegistry) Resolve(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
	r.mu.RLock()
	static := make([]ToolDef, 0, len(r.tools))
	staticNames := make(map[string]bool, len(r.tools))
	for _, def := range r.tools {
		if !def.appliesTo(sctx) {
			continue
		}
		static = append(static, *def)
		staticNames[def.Name] = true
	}
	providers := make([]ToolProvider, len(r.providers))
	copy(providers, r.providers)
	r.mu.RUnlock()

	// 收集静态工具的 llm.Tool
	result := make([]llm.Tool, 0, len(static))
	for _, def := range static {
		result = append(result, def.Tool)
	}

	// 调用动态提供者
	for _, p := range providers {
		dynamic, err := p.Tools(ctx, sctx)
		if err != nil {
			continue // 提供者出错时跳过，不中断
		}
		for _, t := range dynamic {
			// 同名时静态工具优先
			if staticNames[t.Name] {
				continue
			}
			result = append(result, t)
			staticNames[t.Name] = true // 防止后续 provider 重复
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

// ResolveDefs 解析当前会话上下文下的工具定义（包含元数据）。
// 仅返回静态注册的工具定义（动态提供者只返回 llm.Tool）。
func (r *ToolRegistry) ResolveDefs(sctx *ToolSessionContext) []ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]ToolDef, 0, len(r.tools))
	for _, def := range r.tools {
		if !def.appliesTo(sctx) {
			continue
		}
		result = append(result, *def)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}
