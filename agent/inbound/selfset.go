package inbound

import "sync"

// SelfIDSet 是 Bot 自身用户 ID 的线程安全集合。
//
// 它被 Ingress 和 Engagement 共享：
//   - Channel 在 Start 时通过 Add 注册 Bot 的自身 ID
//   - Ingress 在 Receive 时通过 Contains 检查并丢弃自消息
//   - Engagement 的 SelfExclusionRule 通过 Contains 排除自消息
//
// 这种共享设计确保无论何时 Channel 注册了新的自身 ID，
// Ingress 和 Engagement 两层防线同时生效，无需时序协调。
type SelfIDSet struct {
	mu  sync.RWMutex
	ids map[string]struct{}
}

// NewSelfIDSet 创建一个空的 SelfIDSet。
func NewSelfIDSet() *SelfIDSet {
	return &SelfIDSet{ids: make(map[string]struct{})}
}

// Add 注册一个 Bot 自身用户 ID。空值会被忽略。
func (s *SelfIDSet) Add(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	s.ids[id] = struct{}{}
	s.mu.Unlock()
}

// Remove 移除一个已注册的自身用户 ID。
func (s *SelfIDSet) Remove(id string) {
	s.mu.Lock()
	delete(s.ids, id)
	s.mu.Unlock()
}

// Contains 检查给定的用户 ID 是否属于 Bot 自身。
// 空值永远返回 false。
func (s *SelfIDSet) Contains(id string) bool {
	if id == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.ids[id]
	s.mu.RUnlock()
	return ok
}

// Len 返回已注册的自身用户 ID 数量。
func (s *SelfIDSet) Len() int {
	s.mu.RLock()
	n := len(s.ids)
	s.mu.RUnlock()
	return n
}
