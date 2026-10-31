package memory

import (
	"context"
	"errors"

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
	botID string
}

// NewNoteWriterAdapter 将写入归属绑定到组装时的机器人 ID。
// NoteEntry 的 scope 和 metadata 不作为归属授权来源。
func NewNoteWriterAdapter(store Store, botID string) *NoteWriterAdapter {
	return &NoteWriterAdapter{store: store, botID: botID}
}

// WriteNote 将 NoteEntry 转换为 memory.Entry 并追加到 Store。
// 实现 outbound.NoteWriter 接口。
func (a *NoteWriterAdapter) WriteNote(ctx context.Context, note outbound.NoteEntry) error {
	if a.botID == "" {
		return errors.New("memory note: bot ID is required")
	}
	metadata := make(map[string]any, len(note.Metadata)+1)
	for key, value := range note.Metadata {
		metadata[key] = value
	}
	metadata["bot_id"] = a.botID
	// 会话/用户 scope 仍供画像与巩固分类；bot/global 笔记绑定到当前机器人。
	scope := Scope{Kind: ScopeKind(note.ScopeKind), ID: note.ScopeID}
	switch scope.Kind {
	case ScopeBot, ScopeGlobal:
		scope = BotScope(a.botID)
	case ScopeChannel, ScopeUser:
		if scope.ID == "" {
			return errors.New("memory note: scope ID is required")
		}
	default:
		return errors.New("memory note: unknown scope kind")
	}
	entry := Entry{
		ID:             note.ID,
		Scope:          scope,
		Content:        note.Content,
		Category:       note.Category,
		Source:         note.Source,
		Importance:     note.Importance,
		Metadata:       metadata,
		CreatedAt:      note.CreatedAt,
		LastAccessedAt: note.LastAccessedAt,
	}
	return a.store.Append(ctx, entry)
}
