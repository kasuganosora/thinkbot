package memory

import (
	"context"

	"github.com/kasuganosora/thinkbot/agent/outbound"
)

// ============================================================================
// NoteWriterAdapter — 将 memory.Store 适配为 outbound.NoteWriter
// ============================================================================

// NoteWriterAdapter 适配 memory.Store 为 outbound.NoteWriter 接口。
// NoteHandler 通过 NoteWriter 接口写入，不直接依赖 memory 包（避免循环依赖）。
// 此适配器在组装阶段（Bot 初始化时）桥接两者。
type NoteWriterAdapter struct {
	store Store
}

// NewNoteWriterAdapter 创建 NoteWriter 适配器。
func NewNoteWriterAdapter(store Store) *NoteWriterAdapter {
	return &NoteWriterAdapter{store: store}
}

// WriteNote 将 NoteEntry 转换为 memory.Entry 并追加到 Store。
// 实现 outbound.NoteWriter 接口。
func (a *NoteWriterAdapter) WriteNote(ctx context.Context, note outbound.NoteEntry) error {
	entry := Entry{
		ID: note.ID,
		Scope: Scope{
			Kind: ScopeKind(note.ScopeKind),
			ID:   note.ScopeID,
		},
		Content:        note.Content,
		Category:       note.Category,
		Source:         note.Source,
		Importance:     note.Importance,
		Metadata:       note.Metadata,
		CreatedAt:      note.CreatedAt,
		LastAccessedAt: note.LastAccessedAt,
	}
	return a.store.Append(ctx, entry)
}
