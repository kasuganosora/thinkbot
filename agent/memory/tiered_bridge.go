package memory

import "context"

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

// MultiStore 将写入广播到多个 Store 后端。
type MultiStore struct {
	stores []Store
}

// NewMultiStore 创建多路写入 Store。
// 通常传入 MemoryRepository + TieredStoreAdapter。
func NewMultiStore(stores ...Store) *MultiStore {
	return &MultiStore{stores: stores}
}

// Append 写入所有后端，失败仅记录日志不中断。
func (m *MultiStore) Append(ctx context.Context, entry Entry) error {
	var firstErr error
	for _, s := range m.stores {
		if err := s.Append(ctx, entry); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Delete 从所有后端删除。
func (m *MultiStore) Delete(ctx context.Context, scope Scope, entryID string) error {
	var firstErr error
	for _, s := range m.stores {
		if err := s.Delete(ctx, scope, entryID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Clear 清空所有后端。
func (m *MultiStore) Clear(ctx context.Context, scope Scope) error {
	var firstErr error
	for _, s := range m.stores {
		if err := s.Clear(ctx, scope); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
