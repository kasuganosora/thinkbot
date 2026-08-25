package memory

import (
	"context"
	"log"
	"sync"
	"time"
)

// l0DupTTL 是 L0 写入去重的窗口：同一 (scope, content) 在该窗口内的重复 Append
// 视为冗余（如模型对同一条观察连续调用记忆工具两次、或同帖经多路 ingest 写入），
// 直接丢弃，避免污染 memory_entries / tiered L0。窗口很短（30min），不伤害跨时段的
// 合法重复（如用户隔天说了同样的话）。
const l0DupTTL = 30 * time.Minute

// MultiStore 将写入广播到多个 Store 后端。
type MultiStore struct {
	stores []Store

	// dupMu/dupSeen 是 (scopeKey, content) → 上次写入时间的去重表（见 Append）。
	dupMu    sync.Mutex
	dupSeen  map[string]time.Time
}

// ============================================================================
// TieredStoreAdapter — 将 TieredStore 适配为 memory.Store 接口
//
// 用于将 NoteHandler 产出的记忆同步写入 TieredStore 的 L0 层，
// 使 Dreaming 梦境管线能消费到生产环境中实际产生的记忆数据。
//
// Append 委托给 TieredStore.AppendLegacy（写入 L0）。
// Delete / Clear 委托给 TieredStore 的同名方法（操作 L0）。
// ============================================================================

// TieredStoreAdapter 将 *TieredStore 包装为 memory.Store。
type TieredStoreAdapter struct {
	store *TieredStore
}

// NewTieredStoreAdapter 创建适配器。
func NewTieredStoreAdapter(store *TieredStore) *TieredStoreAdapter {
	return &TieredStoreAdapter{store: store}
}

// Append 将一条记忆写入 TieredStore 的 L0（工作记忆）。
func (a *TieredStoreAdapter) Append(ctx context.Context, entry Entry) error {
	return a.store.AppendLegacy(ctx, entry)
}

// Delete 从 TieredStore L0 中删除指定记忆。
func (a *TieredStoreAdapter) Delete(ctx context.Context, scope Scope, entryID string) error {
	return a.store.Delete(ctx, Tier0Working, scope, entryID)
}

// Clear 清空 TieredStore L0 中指定 scope 的所有记忆。
func (a *TieredStoreAdapter) Clear(ctx context.Context, scope Scope) error {
	return a.store.Clear(ctx, Tier0Working, scope)
}

// ============================================================================
// MultiStore — 将写入同时广播到多个 memory.Store
//
// 用于 Dreaming 开启时同时写入 MemoryRepository（检索用）和
// TieredStoreAdapter（梦境管线用），确保两套存储数据一致。
//
// Append 失败不中断——某一路失败只记日志，不阻塞其他路。
// ============================================================================

// NewMultiStore 创建多路写入 Store。
// 通常传入 MemoryRepository + TieredStoreAdapter。
func NewMultiStore(stores ...Store) *MultiStore {
	return &MultiStore{stores: stores, dupSeen: make(map[string]time.Time)}
}

// scopeKey 返回记忆作用域的稳定字符串键（供去重表索引）。
func scopeKey(s Scope) string {
	return string(s.Kind) + "\x00" + s.ID
}

// Append 写入所有后端，失败仅记录日志不中断。
//
// 写入前做 (scope, content) 短窗口去重：窗口内完全相同的重复写入（模型对同一条观察
// 连续调用记忆工具两次、或同帖经多路 ingest 落多条）直接丢弃，避免污染下游存储。
// 去重仅基于「完全相同的正文」，不影响任何带差异的合法写入。
func (m *MultiStore) Append(ctx context.Context, entry Entry) error {
	key := scopeKey(entry.Scope) + "\x00" + entry.Content
	m.dupMu.Lock()
	if ts, ok := m.dupSeen[key]; ok {
		if time.Since(ts) < l0DupTTL {
			m.dupMu.Unlock()
			return nil
		}
		// 过期项：顺手清理，避免 map 无限增长。
		if len(m.dupSeen) > 4096 {
			for k, t := range m.dupSeen {
				if time.Since(t) >= l0DupTTL {
					delete(m.dupSeen, k)
				}
			}
		}
	}
	m.dupSeen[key] = time.Now()
	m.dupMu.Unlock()

	var firstErr error
	for _, s := range m.stores {
		if err := s.Append(ctx, entry); err != nil {
			log.Printf("[MultiStore] Append failed for backend: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Delete 从所有后端删除。
func (m *MultiStore) Delete(ctx context.Context, scope Scope, entryID string) error {
	var firstErr error
	for _, s := range m.stores {
		if err := s.Delete(ctx, scope, entryID); err != nil {
			log.Printf("[MultiStore] Delete failed for backend: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Clear 清空所有后端。
func (m *MultiStore) Clear(ctx context.Context, scope Scope) error {
	var firstErr error
	for _, s := range m.stores {
		if err := s.Clear(ctx, scope); err != nil {
			log.Printf("[MultiStore] Clear failed for backend: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
