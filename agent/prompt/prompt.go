// Package prompt 提供系统提示词的模板化管理、变量替换和动态组装能力。
//
// 设计理念：
//   - Section 是提示词的最小组装单元（段落），每个 Section 拥有独立的 Order 排序权重
//   - Variable 是 Section 中可替换的变量占位符，支持多种来源（静态值、Envelope KV、动态函数）
//   - Registry 是 Section 的注册中心，线程安全，支持运行时动态增删
//   - Assembler 是核心组装器，负责解析变量、渲染模板、按 Order 拼接最终 prompt
//
// Pipeline 集成：
//   - PromptStage (Order=200) 在 MemoryStage 之后、LLMStage 之前执行
//   - 从 Envelope KV 读取上游注入的数据（memory.context、bot.config 等）
//   - 调用 Assembler 组装完整 system prompt
//   - 写入 env.Set("system.prompt", ...) 供下游 LLMStage/ReplyStage 消费
package prompt

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// Variable — 模板变量
// ============================================================================

// VariableSource 表示变量值的来源类型。
type VariableSource int

const (
	// SourceStatic 静态值：变量值在注册时确定，不随请求变化。
	SourceStatic VariableSource = iota
	// SourceEnvelopeKV 从 Envelope KV 中按 key 读取。
	SourceEnvelopeKV
	// SourceFunc 动态函数：每次组装时调用函数获取值。
	SourceFunc
)

// String 返回来源类型的字符串表示。
func (s VariableSource) String() string {
	switch s {
	case SourceStatic:
		return "static"
	case SourceEnvelopeKV:
		return "envelope_kv"
	case SourceFunc:
		return "func"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// VariableFunc 是动态变量的求值函数。
// ctx 提供组装时的上下文信息（当前 Envelope KV snapshot）。
type VariableFunc func(ctx *AssemblyContext) string

// Variable 定义一个模板变量。
type Variable struct {
	// Name 变量名称（在模板中用 {{.Name}} 引用）。
	Name string

	// Source 变量值来源。
	Source VariableSource

	// StaticValue 静态值（Source=SourceStatic 时使用）。
	StaticValue string

	// EnvelopeKey Envelope KV 的 key（Source=SourceEnvelopeKV 时使用）。
	EnvelopeKey string

	// Func 动态求值函数（Source=SourceFunc 时使用）。
	Func VariableFunc

	// Required 是否必需。如果为 true 且无法解析，Assembler 会报错。
	Required bool

	// Default 默认值。变量无法解析时使用此值（仅 Required=false 时生效）。
	Default string
}

// ============================================================================
// Section — 提示词段落
// ============================================================================

// Section 是 system prompt 的一个独立段落。
// 每个 Section 有自己的排序权重（Order），组装时按 Order 从小到大拼接。
type Section struct {
	// Name 段落标识（唯一，用于注册/引用/日志）。
	Name string

	// Order 排序权重。越小越靠前。
	// 推荐范围：
	//   0-99: 核心身份/角色定义
	//   100-199: 行为规则/约束
	//   200-299: 上下文信息（记忆、会话历史等）
	//   300-399: 工具/能力声明
	//   400-499: 输出格式指令
	//   500+: 附加指令
	Order int

	// Content 段落内容。
	// 如果包含 {{.VarName}} 格式的变量占位符，将在组装时被替换。
	// 如果为空字符串，表示此 Section 为可选段落（仅当变量解析结果非空时才参与拼接）。
	Content string

	// Enabled 是否启用。禁用的段落不参与组装。
	Enabled bool

	// Conditional 条件函数。如果非 nil，只有返回 true 时此段落才参与组装。
	// 用于实现场景感知：例如群聊时才注入群聊规则段落。
	Conditional func(ctx *AssemblyContext) bool

	// Variables 此段落使用的变量列表。
	// Assembler 在渲染此段落时，只解析这些变量。
	Variables []Variable
}

// ============================================================================
// AssemblyContext — 组装上下文
// ============================================================================

// AssemblyContext 提供组装时的上下文信息，供 Variable.Func 和 Section.Conditional 使用。
type AssemblyContext struct {
	// Values 当前 Envelope KV 的快照（只读）。
	Values map[string]any

	// BotID 当前 Bot 标识。
	BotID string

	// Channel 当前消息的会话空间。
	Channel string

	// ChatType 当前会话类型（private/group/...）。
	ChatType string

	// UserID 发送者 ID。
	UserID string

	// Timestamp 组装时间戳。
	Timestamp time.Time
}

// GetString 从 Values 中获取 string 值，不存在或类型不匹配返回空串。
func (c *AssemblyContext) GetString(key string) string {
	if c.Values == nil {
		return ""
	}
	v, ok := c.Values[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// GetInt 从 Values 中获取 int 值。
func (c *AssemblyContext) GetInt(key string) (int, bool) {
	if c.Values == nil {
		return 0, false
	}
	v, ok := c.Values[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

// GetBool 从 Values 中获取 bool 值。
func (c *AssemblyContext) GetBool(key string) (bool, bool) {
	if c.Values == nil {
		return false, false
	}
	v, ok := c.Values[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// ============================================================================
// Registry — Section 注册中心
// ============================================================================

// Registry 是 Section 的线程安全注册中心。
// 支持运行时动态注册/注销段落，适配多 Bot 共享同一 Registry 的场景。
type Registry struct {
	mu       sync.RWMutex
	sections map[string]*Section // name → section

	// metrics
	registered   atomic.Int64
	unregistered atomic.Int64
}

// NewRegistry 创建一个空的 Registry。
func NewRegistry() *Registry {
	return &Registry{
		sections: make(map[string]*Section),
	}
}

// Register 注册一个 Section。如果 name 已存在则覆盖。
func (r *Registry) Register(s Section) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sections[s.Name] = &s
	r.registered.Add(1)
}

// RegisterMany 批量注册多个 Section。
func (r *Registry) RegisterMany(sections ...Section) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range sections {
		r.sections[sections[i].Name] = &sections[i]
		r.registered.Add(1)
	}
}

// Unregister 注销指定 name 的 Section。不存在时静默忽略。
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sections[name]; ok {
		delete(r.sections, name)
		r.unregistered.Add(1)
	}
}

// Get 获取指定 name 的 Section（副本）。
func (r *Registry) Get(name string) (Section, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sections[name]
	if !ok {
		return Section{}, false
	}
	return *s, true
}

// List 返回所有已注册的 Section 列表（已按 Order 排序）。
func (r *Registry) List() []Section {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Section, 0, len(r.sections))
	for _, s := range r.sections {
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Order < result[j].Order
	})
	return result
}

// Len 返回已注册的 Section 数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sections)
}

// RegistryMetrics 返回注册中心的度量数据。
type RegistryMetrics struct {
	Registered   int64
	Unregistered int64
	CurrentSize  int
}

// Metrics 返回 Registry 的度量信息。
func (r *Registry) Metrics() RegistryMetrics {
	return RegistryMetrics{
		Registered:   r.registered.Load(),
		Unregistered: r.unregistered.Load(),
		CurrentSize:  r.Len(),
	}
}

// ============================================================================
// Assembler — 提示词组装器
// ============================================================================

// AssemblerConfig 配置组装器行为。
type AssemblerConfig struct {
	// SectionSeparator 段落之间的分隔符（默认 "\n\n"）。
	SectionSeparator string

	// TrimEmpty 是否跳过渲染后为空的段落（默认 true）。
	TrimEmpty bool

	// StrictMode 严格模式。开启时，Required 变量解析失败会返回 error。
	// 关闭时，解析失败的 Required 变量用 Default 替代（如果有）。
	StrictMode bool

	// MaxPromptLength 最大 prompt 长度（字符数）。0 表示无限制。
	// 超限时从低优先级段落开始截断。
	MaxPromptLength int
}

// DefaultAssemblerConfig 返回合理的默认配置。
func DefaultAssemblerConfig() AssemblerConfig {
	return AssemblerConfig{
		SectionSeparator: "\n\n",
		TrimEmpty:        true,
		StrictMode:       false,
		MaxPromptLength:  0,
	}
}

// Assembler 负责将 Registry 中的 Section 解析、渲染并拼接为最终的 system prompt。
type Assembler struct {
	registry *Registry
	config   AssemblerConfig

	// metrics
	assemblies atomic.Int64
	errors     atomic.Int64
}

// NewAssembler 创建组装器。
func NewAssembler(registry *Registry, config AssemblerConfig) *Assembler {
	if config.SectionSeparator == "" {
		config.SectionSeparator = "\n\n"
	}
	return &Assembler{
		registry: registry,
		config:   config,
	}
}

// AssemblyResult 组装结果。
type AssemblyResult struct {
	// Prompt 组装后的完整 system prompt。
	Prompt string

	// SectionsUsed 参与组装的 Section 名称列表（按 Order 排列）。
	SectionsUsed []string

	// SectionsSkipped 被跳过的 Section 名称列表（Conditional 返回 false / 渲染后为空 / 被截断）。
	SectionsSkipped []string

	// VariablesResolved 成功解析的变量数。
	VariablesResolved int

	// VariablesFailed 解析失败的变量数（仅 StrictMode=false 时才不为 0）。
	VariablesFailed int

	// Truncated 是否因 MaxPromptLength 发生截断。
	Truncated bool

	// PromptLength 最终 prompt 的字符长度。
	PromptLength int
}

// Assemble 执行组装。
// ctx 提供当前请求的上下文信息，sections 为额外的临时段落（不会被注册到 Registry）。
func (a *Assembler) Assemble(ctx *AssemblyContext, extraSections ...Section) (*AssemblyResult, error) {
	a.assemblies.Add(1)

	// 获取所有已注册的 sections + 额外的临时 sections
	sections := a.registry.List()
	if len(extraSections) > 0 {
		sections = append(sections, extraSections...)
		sort.Slice(sections, func(i, j int) bool {
			return sections[i].Order < sections[j].Order
		})
	}

	result := &AssemblyResult{}
	var parts []string

	for _, sec := range sections {
		// 跳过禁用的段落
		if !sec.Enabled {
			result.SectionsSkipped = append(result.SectionsSkipped, sec.Name)
			continue
		}

		// 条件判断
		if sec.Conditional != nil && !sec.Conditional(ctx) {
			result.SectionsSkipped = append(result.SectionsSkipped, sec.Name)
			continue
		}

		// 渲染段落
		rendered, resolved, failed, err := a.renderSection(ctx, &sec)
		if err != nil {
			a.errors.Add(1)
			return nil, fmt.Errorf("prompt assembler: section %q: %w", sec.Name, err)
		}

		result.VariablesResolved += resolved
		result.VariablesFailed += failed

		// 跳过渲染后为空的段落
		if a.config.TrimEmpty && strings.TrimSpace(rendered) == "" {
			result.SectionsSkipped = append(result.SectionsSkipped, sec.Name)
			continue
		}

		parts = append(parts, rendered)
		result.SectionsUsed = append(result.SectionsUsed, sec.Name)
	}

	// 拼接
	prompt := strings.Join(parts, a.config.SectionSeparator)

	// 长度限制
	if a.config.MaxPromptLength > 0 && len(prompt) > a.config.MaxPromptLength {
		prompt = a.truncate(prompt, parts, result)
	}

	result.Prompt = prompt
	result.PromptLength = len(prompt)
	return result, nil
}

// renderSection 渲染单个段落的模板变量。
func (a *Assembler) renderSection(ctx *AssemblyContext, sec *Section) (string, int, int, error) {
	content := sec.Content
	resolved := 0
	failed := 0

	for _, v := range sec.Variables {
		placeholder := "{{." + v.Name + "}}"
		if !strings.Contains(content, placeholder) {
			continue
		}

		value, ok := a.resolveVariable(ctx, &v)
		if !ok {
			if v.Required && a.config.StrictMode {
				return "", resolved, failed, fmt.Errorf("required variable %q not resolved (source=%s)", v.Name, v.Source)
			}
			value = v.Default
			failed++
		} else {
			resolved++
		}

		content = strings.ReplaceAll(content, placeholder, value)
	}

	return content, resolved, failed, nil
}

// resolveVariable 根据来源解析变量值。
func (a *Assembler) resolveVariable(ctx *AssemblyContext, v *Variable) (string, bool) {
	switch v.Source {
	case SourceStatic:
		return v.StaticValue, true

	case SourceEnvelopeKV:
		if ctx.Values == nil {
			return "", false
		}
		val, ok := ctx.Values[v.EnvelopeKey]
		if !ok {
			return "", false
		}
		s, ok := val.(string)
		if !ok {
			// 尝试 fmt 转换
			return fmt.Sprintf("%v", val), true
		}
		return s, true

	case SourceFunc:
		if v.Func == nil {
			return "", false
		}
		result := v.Func(ctx)
		return result, true

	default:
		return "", false
	}
}

// truncate 从低优先级（高 Order）段落开始截断，直到满足长度限制。
func (a *Assembler) truncate(prompt string, parts []string, result *AssemblyResult) string {
	result.Truncated = true
	maxLen := a.config.MaxPromptLength
	sepLen := len(a.config.SectionSeparator)

	// 从后向前移除段落直到满足限制
	for len(prompt) > maxLen && len(parts) > 1 {
		// 移除最后一个段落
		removed := parts[len(parts)-1]
		parts = parts[:len(parts)-1]

		// 重新拼接
		prompt = strings.Join(parts, a.config.SectionSeparator)

		// 更新 sections 列表
		if len(result.SectionsUsed) > 0 {
			removedName := result.SectionsUsed[len(result.SectionsUsed)-1]
			result.SectionsUsed = result.SectionsUsed[:len(result.SectionsUsed)-1]
			result.SectionsSkipped = append(result.SectionsSkipped, removedName)
		}

		_ = removed
		_ = sepLen
	}

	// 如果单段落仍然超限，硬截断
	if len(prompt) > maxLen {
		prompt = prompt[:maxLen]
	}

	return prompt
}

// AssemblerMetrics 组装器度量。
type AssemblerMetrics struct {
	Assemblies int64
	Errors     int64
}

// Metrics 返回组装器度量。
func (a *Assembler) Metrics() AssemblerMetrics {
	return AssemblerMetrics{
		Assemblies: a.assemblies.Load(),
		Errors:     a.errors.Load(),
	}
}
