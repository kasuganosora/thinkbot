package core

// ============================================================================
// Predicate — 条件匹配器
// ============================================================================

// Predicate 判断 Envelope 是否满足某个条件。
// 用于 Router 的条件路由和 FilterStage 的消息过滤。
type Predicate interface {
	Match(env *Envelope) bool
}

// PredicateFunc 将普通函数适配为 Predicate 接口。
type PredicateFunc func(*Envelope) bool

func (f PredicateFunc) Match(env *Envelope) bool { return f(env) }
