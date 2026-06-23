package pipeline

import (
	"context"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Predicate — 条件匹配器（定义在 core 包，此处为类型别名保持兼容）
// ============================================================================

// Predicate 是 core.Predicate 的类型别名，保持向后兼容。
type Predicate = core.Predicate

// PredicateFunc 是 core.PredicateFunc 的类型别名，保持向后兼容。
type PredicateFunc = core.PredicateFunc

// ============================================================================
// 内置谓词
// ============================================================================

// TextContains 当消息文本包含指定子串时匹配。
type TextContains struct {
	Substring string
}

func (p *TextContains) Match(env *core.Envelope) bool {
	return strings.Contains(env.Message.Text, p.Substring)
}

// TextHasPrefix 当消息文本以指定前缀开头时匹配。
type TextHasPrefix struct {
	Prefix string
}

func (p *TextHasPrefix) Match(env *core.Envelope) bool {
	return len(env.Message.Text) >= len(p.Prefix) && env.Message.Text[:len(p.Prefix)] == p.Prefix
}

// TextRegex 当消息文本匹配正则表达式时匹配。
type TextRegex struct {
	Pattern RegexpCompat
}

// RegexpCompat 兼容 *regexp.Regexp 或任何有 MatchString 方法的对象。
type RegexpCompat interface {
	MatchString(s string) bool
}

func (p *TextRegex) Match(env *core.Envelope) bool {
	return p.Pattern.MatchString(env.Message.Text)
}

// SourceEquals 当消息来源等于指定值时匹配。
type SourceEquals struct {
	Source string
}

func (p *SourceEquals) Match(env *core.Envelope) bool {
	return env.Message.Source == p.Source
}

// ChannelEquals 当消息频道等于指定值时匹配。
type ChannelEquals struct {
	Channel string
}

func (p *ChannelEquals) Match(env *core.Envelope) bool {
	return env.Message.Channel == p.Channel
}

// MetadataExists 当消息元数据中存在指定 key 时匹配。
type MetadataExists struct {
	Key string
}

func (p *MetadataExists) Match(env *core.Envelope) bool {
	if env.Message.Metadata == nil {
		return false
	}
	_, ok := env.Message.Metadata[p.Key]
	return ok
}

// MetadataEquals 当消息元数据中指定 key 的值等于指定值时匹配。
type MetadataEquals struct {
	Key   string
	Value any
}

func (p *MetadataEquals) Match(env *core.Envelope) bool {
	if env.Message.Metadata == nil {
		return false
	}
	v, ok := env.Message.Metadata[p.Key]
	if !ok {
		return false
	}
	return v == p.Value
}

// ValueExists 当 Envelope KV 存储中存在指定 key 时匹配。
// 用于检查前序 Stage 是否已设置某个值。
type ValueExists struct {
	Key string
}

func (p *ValueExists) Match(env *core.Envelope) bool {
	_, ok := env.Get(p.Key)
	return ok
}

// ============================================================================
// 组合谓词
// ============================================================================

// And 所有子谓词都匹配时才匹配。
type And struct {
	Predicates []Predicate
}

func (p *And) Match(env *core.Envelope) bool {
	for _, pred := range p.Predicates {
		if !pred.Match(env) {
			return false
		}
	}
	return true
}

// Or 任一子谓词匹配时即匹配。
type Or struct {
	Predicates []Predicate
}

func (p *Or) Match(env *core.Envelope) bool {
	for _, pred := range p.Predicates {
		if pred.Match(env) {
			return true
		}
	}
	return false
}

// Not 取反。
type Not struct {
	Inner Predicate
}

func (p *Not) Match(env *core.Envelope) bool {
	return !p.Inner.Match(env)
}

// ============================================================================
// 便捷构造函数
// ============================================================================

// MatchAll 返回一个始终匹配的谓词。
func MatchAll() Predicate {
	return PredicateFunc(func(*core.Envelope) bool { return true })
}

// MatchNone 返回一个始终不匹配的谓词。
func MatchNone() Predicate {
	return PredicateFunc(func(*core.Envelope) bool { return false })
}

// MatchTextContains 便捷构造 TextContains。
func MatchTextContains(s string) Predicate {
	return &TextContains{Substring: s}
}

// MatchSource 便捷构造 SourceEquals。
func MatchSource(source string) Predicate {
	return &SourceEquals{Source: source}
}

// MatchChannel 便捷构造 ChannelEquals。
func MatchChannel(ch string) Predicate {
	return &ChannelEquals{Channel: ch}
}

// ============================================================================
// Router — 条件路由分发器
// ============================================================================

// Route 一条路由规则：Predicate 匹配 → 执行对应 Stage 子链。
type Route struct {
	// Name 路由名称（用于日志和 tracing）。
	Name string
	// Predicate 匹配条件。
	Predicate Predicate
	// Stages 匹配后依次执行的 Stage 列表。
	Stages []core.Stage
	// Fallback 是否为兜底路由（无其他路由匹配时使用）。
	Fallback bool
}

// Router 本身是一个 Stage，根据 Predicate 将消息分发到不同的 Stage 子链。
type Router struct {
	name   string
	routes []Route
}

// NewRouter 创建条件路由器。
func NewRouter(name string, routes ...Route) *Router {
	return &Router{
		name:   name,
		routes: routes,
	}
}

// Name 返回 Router 名称。
func (r *Router) Name() string { return r.name }

// Process 根据路由规则分发处理。
func (r *Router) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	var fallback *Route

	for i := range r.routes {
		route := &r.routes[i]

		if route.Fallback {
			fallback = route
			continue
		}

		if route.Predicate != nil && route.Predicate.Match(env) {
			return r.executeRoute(ctx, route, env)
		}
	}

	// 无匹配：尝试 Fallback
	if fallback != nil {
		return r.executeRoute(ctx, fallback, env)
	}

	// 无匹配也无 Fallback：直接透传
	return env, nil
}

// executeRoute 执行路由中的 Stage 子链。
func (r *Router) executeRoute(ctx context.Context, route *Route, env *core.Envelope) (*core.Envelope, error) {
	for _, s := range route.Stages {
		var err error
		env, err = s.Process(ctx, env)
		if err != nil {
			return env, err
		}
		if env == nil || env.Aborted() {
			return env, nil
		}
	}
	return env, nil
}
