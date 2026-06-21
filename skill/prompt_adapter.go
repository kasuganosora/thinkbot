package skill

import (
	"strings"
	"sync"
)

// ============================================================================
// RegistryAdapter 的默认实现（适配 prompt.Registry）
//
// 用法：
//
//	adapter := skill.NewPromptRegistryAdapter(
//	    func(name string, order int, content string, enabled bool) {
//	        registry.Register(prompt.Section{
//	            Name:    name,
//	            Order:   order,
//	            Content: content,
//	            Enabled: enabled,
//	        })
//	    },
//	    func(name string) {
//	        registry.Unregister(name)
//	    },
//	)
//	mgr.SetRegistry(adapter)
//
// ============================================================================

// PromptRegistryAdapter 将回调函数适配为 RegistryAdapter 接口。
type PromptRegistryAdapter struct {
	registerFn   func(name string, order int, content string, enabled bool)
	unregisterFn func(name string)
}

// NewPromptRegistryAdapter 创建 PromptRegistryAdapter。
// registerFn: 将 Skill Content 注册为 prompt Section 的回调。
// unregisterFn: 移除指定名称 prompt Section 的回调。
func NewPromptRegistryAdapter(
	registerFn func(name string, order int, content string, enabled bool),
	unregisterFn func(name string),
) *PromptRegistryAdapter {
	return &PromptRegistryAdapter{
		registerFn:   registerFn,
		unregisterFn: unregisterFn,
	}
}

func (a *PromptRegistryAdapter) RegisterSection(name string, order int, content string, enabled bool) {
	if a.registerFn != nil {
		a.registerFn(name, order, content, enabled)
	}
}

func (a *PromptRegistryAdapter) UnregisterSection(name string) {
	if a.unregisterFn != nil {
		a.unregisterFn(name)
	}
}

// ============================================================================
// 直拼模式（Direct Injection）
//
// 除了 Section 模式（通过 prompt.Registry 注入）外，
// 还支持"直拼模式"：直接将 Skill Content 拼接到 system prompt 字符串。
//
// 适用场景：不使用 prompt.Registry 的轻量级 Bot，
// 或需要更精细控制注入位置的场景。
// ============================================================================

// DirectInjector 支持直拼模式的注入器。
type DirectInjector struct {
	mu            sync.RWMutex
	skillOrder    []string          // 已注入的 Skill 名称顺序
	skillContents map[string]string // 缓存已注入的 Skill Content
}

// NewDirectInjector 创建直拼注入器。
func NewDirectInjector() *DirectInjector {
	return &DirectInjector{
		skillContents: make(map[string]string),
	}
}

// Inject 将启用的 Skill Content 拼接到 system prompt。
// 返回拼接后的完整 system prompt。
func (d *DirectInjector) Inject(basePrompt string, skills ...*Skill) string {
	d.mu.Lock()
	for _, s := range skills {
		if !s.Enabled || s.Content == "" {
			continue
		}
		// 去重：如果已存在，先移除旧位置
		if _, exists := d.skillContents[s.Name]; exists {
			d.removeFromOrderLocked(s.Name)
		}
		d.skillOrder = append(d.skillOrder, s.Name)
		d.skillContents[s.Name] = s.Content
	}

	// 拼接
	var buf strings.Builder
	buf.WriteString(basePrompt)
	buf.WriteString("\n\n")

	for _, name := range d.skillOrder {
		buf.WriteString("## Skill: ")
		buf.WriteString(name)
		buf.WriteString("\n")
		buf.WriteString(d.skillContents[name])
		buf.WriteString("\n\n")
	}
	d.mu.Unlock()

	return buf.String()
}

// removeFromOrderLocked 从 order 切片中移除指定名称（调用方必须已持有写锁）。
func (d *DirectInjector) removeFromOrderLocked(name string) {
	newOrder := make([]string, 0, len(d.skillOrder))
	for _, n := range d.skillOrder {
		if n != name {
			newOrder = append(newOrder, n)
		}
	}
	d.skillOrder = newOrder
}

// Remove 从注入器中移除指定 Skill。
func (d *DirectInjector) Remove(names ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range names {
		delete(d.skillContents, name)
		d.removeFromOrderLocked(name)
	}
}

// Clear 清空所有已注入的 Skill。
func (d *DirectInjector) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.skillOrder = nil
	d.skillContents = make(map[string]string)
}

// SkillNames 返回当前已注入的 Skill 名称列表（按顺序）。
func (d *DirectInjector) SkillNames() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]string, len(d.skillOrder))
	copy(result, d.skillOrder)
	return result
}
