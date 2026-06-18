package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/outbound"
)

// ============================================================================
// SQLiteNoteStore — outbound.NoteStore 的 SQLite 实现
// ============================================================================

// SQLiteNoteStoreConfig 配置 SQLite 备注存储。
type SQLiteNoteStoreConfig struct {
	// MaxNotes 最大备注数量（默认 10000）。超过时删除最旧的备注。
	MaxNotes int
	// TTL 备注存活时间（默认 0 = 不过期）。
	TTL time.Duration
}

// SQLiteNoteStore 使用 GORM/SQLite 持久化备注。
// 实现 outbound.NoteStore 接口。
type SQLiteNoteStore struct {
	db       *gorm.DB
	maxNotes int
	ttl      time.Duration
}

// NewSQLiteNoteStore 创建 SQLite 备注存储。
func NewSQLiteNoteStore(db *gorm.DB, opts ...SQLiteNoteStoreConfig) *SQLiteNoteStore {
	maxNotes := 10000
	var ttl time.Duration
	if len(opts) > 0 {
		if opts[0].MaxNotes > 0 {
			maxNotes = opts[0].MaxNotes
		}
		ttl = opts[0].TTL
	}
	return &SQLiteNoteStore{
		db:       db,
		maxNotes: maxNotes,
		ttl:      ttl,
	}
}

// Save 持久化一条备注。
// 自动清理过期备注和超过容量的旧备注。
func (s *SQLiteNoteStore) Save(ctx context.Context, note outbound.Note) error {
	// TTL 清理
	if s.ttl > 0 {
		cutoff := time.Now().Add(-s.ttl)
		s.db.WithContext(ctx).
			Where("created_at < ?", cutoff).
			Delete(&NoteModel{})
	}

	// 转为 model
	model := noteToModel(note)
	if err := s.db.WithContext(ctx).Create(&model).Error; err != nil {
		return fmt.Errorf("sqlite_note_store: save failed: %w", err)
	}

	// 容量限制：异步淘汰
	go s.evictIfNeeded()

	return nil
}

// Notes 返回所有已存储备注（按时间正序）。
func (s *SQLiteNoteStore) Notes(ctx context.Context) ([]outbound.Note, error) {
	var models []NoteModel
	if err := s.db.WithContext(ctx).Order("created_at ASC").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("sqlite_note_store: notes failed: %w", err)
	}
	return modelsToNotes(models), nil
}

// NotesByBot 返回指定 Bot 的备注（按时间倒序）。
func (s *SQLiteNoteStore) NotesByBot(ctx context.Context, botID string, limit int) ([]outbound.Note, error) {
	if limit <= 0 {
		limit = 100
	}
	var models []NoteModel
	if err := s.db.WithContext(ctx).
		Where("bot_id = ?", botID).
		Order("created_at DESC").
		Limit(limit).
		Find(&models).Error; err != nil {
		return nil, fmt.Errorf("sqlite_note_store: notes_by_bot failed: %w", err)
	}
	return modelsToNotes(models), nil
}

// NotesByChannel 返回指定 Channel 的备注（按时间倒序）。
func (s *SQLiteNoteStore) NotesByChannel(ctx context.Context, channel string, limit int) ([]outbound.Note, error) {
	if limit <= 0 {
		limit = 100
	}
	var models []NoteModel
	if err := s.db.WithContext(ctx).
		Where("channel = ?", channel).
		Order("created_at DESC").
		Limit(limit).
		Find(&models).Error; err != nil {
		return nil, fmt.Errorf("sqlite_note_store: notes_by_channel failed: %w", err)
	}
	return modelsToNotes(models), nil
}

// Count 返回备注总数。
func (s *SQLiteNoteStore) Count(ctx context.Context) (int, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&NoteModel{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("sqlite_note_store: count failed: %w", err)
	}
	return int(count), nil
}

// Clear 清空所有备注。
func (s *SQLiteNoteStore) Clear(ctx context.Context) error {
	if err := s.db.WithContext(ctx).Where("1 = 1").Delete(&NoteModel{}).Error; err != nil {
		return fmt.Errorf("sqlite_note_store: clear failed: %w", err)
	}
	return nil
}

// evictIfNeeded 检查是否超出容量，超出时淘汰最旧备注。
func (s *SQLiteNoteStore) evictIfNeeded() {
	var count int64
	s.db.Model(&NoteModel{}).Count(&count)

	if int(count) <= s.maxNotes {
		return
	}

	excess := int(count) - s.maxNotes

	var oldIDs []string
	s.db.Model(&NoteModel{}).
		Order("created_at ASC").
		Limit(excess).
		Pluck("id", &oldIDs)

	if len(oldIDs) > 0 {
		s.db.Where("id IN ?", oldIDs).Delete(&NoteModel{})
	}
}

// ============================================================================
// Conversion helpers
// ============================================================================

func noteToModel(note outbound.Note) NoteModel {
	metadataJSON := ""
	if note.Metadata != nil {
		if b, err := json.Marshal(note.Metadata); err == nil {
			metadataJSON = string(b)
		}
	}
	return NoteModel{
		ID:           note.ID,
		BotID:        note.BotID,
		Channel:      note.Channel,
		UserID:       note.UserID,
		MessageID:    note.MessageID,
		Text:         note.Text,
		Category:     note.Category,
		MetadataJSON: metadataJSON,
		CreatedAt:    note.CreatedAt,
	}
}

func modelToNote(m NoteModel) outbound.Note {
	var metadata map[string]any
	if m.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(m.MetadataJSON), &metadata)
	}
	return outbound.Note{
		ID:        m.ID,
		BotID:     m.BotID,
		Channel:   m.Channel,
		UserID:    m.UserID,
		MessageID: m.MessageID,
		Text:      m.Text,
		Category:  m.Category,
		Metadata:  metadata,
		CreatedAt: m.CreatedAt,
	}
}

func modelsToNotes(models []NoteModel) []outbound.Note {
	notes := make([]outbound.Note, len(models))
	for i, m := range models {
		notes[i] = modelToNote(m)
	}
	return notes
}
