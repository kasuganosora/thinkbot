package outbound

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Note — 备注/内部笔记数据结构
// ============================================================================

// Note 表示 Bot 的一条内部备注记录。
// 当 LLM 判定不需要回复用户但需要记住某些信息时，产出 ActionNote。
// NoteHandler 将其收集为 Note 结构，供后续记忆模块消费。
type Note struct {
	// ID 备注唯一标识（由 NoteHandler 自动生成）。
	ID string `json:"id"`
	// BotID 所属 Bot。
	BotID string `json:"botId"`
	// Channel 关联的会话空间标识。
	Channel string `json:"channel"`
	// UserID 关联的用户 ID。
	UserID string `json:"userId,omitempty"`
	// MessageID 触发此备注的原始消息 ID。
	MessageID string `json:"messageId,omitempty"`
	// Text 备注文本。
	Text string `json:"text"`
	// Category 备注分类（"observation" / "summary" / "todo" / "insight" 等）。
	Category string `json:"category,omitempty"`
	// Metadata 附加上下文。
	Metadata map[string]any `json:"metadata,omitempty"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"createdAt"`
}

// ============================================================================
// NoteStore — 备注持久化接口
// ============================================================================

// NoteStore 定义备注的持久化能力。
// 实现此接口可以对接不同的后端：内存、文件、数据库等。
// NoteHandler 收到 ActionNote 后调用 NoteStore.Save 持久化。
type NoteStore interface {
	// Save 持久化一条备注。
	Save(ctx context.Context, note Note) error
}

// ============================================================================
// MemoryNoteStore — 内存备注存储（测试/开发用）
// ============================================================================

// MemoryNoteStore 将备注存储在内存中，适用于测试和开发。
type MemoryNoteStore struct {
	mu    sync.Mutex
	notes []Note
}

// NewMemoryNoteStore 创建内存备注存储。
func NewMemoryNoteStore() *MemoryNoteStore {
	return &MemoryNoteStore{}
}

// Save 将备注追加到内存列表。
func (s *MemoryNoteStore) Save(_ context.Context, note Note) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, note)
	return nil
}

// Notes 返回所有已存储备注的副本。
func (s *MemoryNoteStore) Notes() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Note, len(s.notes))
	copy(out, s.notes)
	return out
}

// LastNote 返回最后一条备注，没有则返回 nil。
func (s *MemoryNoteStore) LastNote() *Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.notes) == 0 {
		return nil
	}
	n := s.notes[len(s.notes)-1]
	return &n
}

// Clear 清空所有备注。
func (s *MemoryNoteStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = nil
}

// Count 返回备注数量。
func (s *MemoryNoteStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.notes)
}

// ============================================================================
// NoteHandler — 备注处理器（ActionHandler 实现）
// ============================================================================

// NoteHandler 处理 ActionNote 类型的 Action。
// 它从 Action 中提取备注信息，构建 Note 结构，然后通过 NoteStore 持久化。
//
// 注册到 MultiDispatcher 后处理 ActionNote：
//
//	noteHandler := outbound.NewNoteHandler(store, logger, tp)
//	dispatcher.Register(core.ActionNote, noteHandler)
//
// Action 字段约定（ActionNote）：
//   - Action.Payload：备注文本（string）
//   - Action.Channel：关联的会话空间标识
//   - Action.UserID：关联的用户 ID
//   - Action.Metadata["source_channel"]：来源 Channel（通用路由字段）
//   - Action.Metadata["message_id"]：触发此备注的原始消息 ID
//   - Action.Metadata["category"]：备注分类（可选）
//   - Action.Metadata["bot_id"]：所属 Bot ID
type NoteHandler struct {
	store  NoteStore
	logger *zap.SugaredLogger
	tracer trace.Tracer

	// noteCounter 用于生成简单 ID（生产环境应使用 UUID）
	mu          sync.Mutex
	noteCounter int64
}

// NewNoteHandler 创建备注处理器。
func NewNoteHandler(store NoteStore, logger *zap.SugaredLogger, tp trace.TracerProvider) *NoteHandler {
	return &NoteHandler{
		store:  store,
		logger: logger.With("component", "note_handler"),
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/outbound/note"),
	}
}

// Handle 处理 ActionNote 类型的 Action。
// 实现 ActionHandler 接口。
func (h *NoteHandler) Handle(ctx context.Context, action core.Action) error {
	ctx, span := h.tracer.Start(ctx, "note.handle",
		trace.WithAttributes(
			attribute.String("action.type", string(action.Type)),
			attribute.String("action.channel", action.Channel),
		))
	defer span.End()

	// 提取备注文本
	text := ""
	if action.Payload != nil {
		if s, ok := action.Payload.(string); ok {
			text = s
		}
	}
	if text == "" {
		h.logger.Debugw("empty note, skipping", "channel", action.Channel)
		return nil
	}

	// 从 Metadata 提取关联信息
	botID := ""
	messageID := ""
	category := ""
	if action.Metadata != nil {
		if v, ok := action.Metadata["bot_id"]; ok {
			if s, ok := v.(string); ok {
				botID = s
			}
		}
		if v, ok := action.Metadata["message_id"]; ok {
			if s, ok := v.(string); ok {
				messageID = s
			}
		}
		if v, ok := action.Metadata["category"]; ok {
			if s, ok := v.(string); ok {
				category = s
			}
		}
	}

	// 生成 ID
	h.mu.Lock()
	h.noteCounter++
	noteID := h.noteCounter
	h.mu.Unlock()

	note := Note{
		ID:        fmt.Sprintf("note-%d", noteID),
		BotID:     botID,
		Channel:   action.Channel,
		UserID:    action.UserID,
		MessageID: messageID,
		Text:      text,
		Category:  category,
		Metadata:  action.Metadata,
		CreatedAt: time.Now(),
	}

	span.SetAttributes(
		attribute.String("note.id", note.ID),
		attribute.String("note.bot_id", note.BotID),
		attribute.String("note.category", note.Category),
	)

	h.logger.Infow("saving note",
		"note_id", note.ID,
		"bot_id", note.BotID,
		"channel", note.Channel,
		"user_id", note.UserID,
		"category", note.Category,
		"text_len", len(text))

	if err := h.store.Save(ctx, note); err != nil {
		h.logger.Errorw("note save failed",
			"note_id", note.ID, "err", err)
		return fmt.Errorf("note_handler: save failed: %w", err)
	}

	return nil
}
