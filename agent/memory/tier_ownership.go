package memory

import (
	"context"
	"errors"
	"fmt"

	"github.com/kasuganosora/thinkbot/dao"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const memoryOwnerSQL = "(CASE WHEN json_valid(metadata_json) THEN json_extract(metadata_json, '$.bot_id') ELSE NULL END)"
const memoryOwnershipSQL = "((scope_kind = ? AND scope_id = ? AND (" + memoryOwnerSQL + " IS NULL OR " + memoryOwnerSQL + " = ?)) OR (scope_kind <> ? AND " + memoryOwnerSQL + " = ?))"

// OwnedMemoryRows restricts memory_entries/tiered_memories queries to a trusted
// bot. Unattributed legacy channel/user/global rows remain quarantined.
func OwnedMemoryRows(tx *gorm.DB, botID string) *gorm.DB {
	if botID == "" {
		return tx.Where("1 = 0")
	}
	return tx.Where(memoryOwnershipSQL, string(ScopeBot), botID, botID, string(ScopeBot), botID)
}

func (s *TieredStore) ownedDB(tx *gorm.DB) *gorm.DB {
	if !s.bound {
		return tx
	}
	return OwnedMemoryRows(tx, s.botID)
}

func (s *TieredStore) ownedEntry(ctx context.Context, e TieredEntry) (TieredEntry, error) {
	if !s.bound {
		return e, nil
	}
	if s.botID == "" {
		return e, errors.New("tiered_store: missing bot owner")
	}
	if e.Scope.Kind == ScopeBot && e.Scope.ID != s.botID {
		return e, errors.New("tiered_store: foreign bot scope")
	}
	if owner, exists := e.Metadata["bot_id"]; exists && owner != s.botID {
		return e, errors.New("tiered_store: conflicting bot owner")
	}
	if e.ID != "" && s.db != nil {
		var total, owned int64
		tx := s.db.WithContext(ctx).Model(&dao.TieredMemoryModel{}).Where("id = ?", e.ID)
		if err := tx.Count(&total).Error; err != nil {
			return e, err
		}
		if total > 0 {
			if err := s.ownedDB(s.db.WithContext(ctx).Model(&dao.TieredMemoryModel{})).Where("id = ?", e.ID).Count(&owned).Error; err != nil {
				return e, err
			}
			if owned == 0 {
				return e, fmt.Errorf("tiered_store: entry ID belongs to another owner")
			}
		}
	}
	metadata := make(map[string]any, len(e.Metadata)+1)
	for k, v := range e.Metadata {
		metadata[k] = v
	}
	metadata["bot_id"] = s.botID
	e.Metadata = metadata
	return e, nil
}

func (s *TieredStore) persistOwnedEntry(ctx context.Context, model *dao.TieredMemoryModel) error {
	tx := s.db.WithContext(ctx)
	if !s.bound {
		return tx.Save(model).Error
	}
	// The conflict predicate protects a foreign ID even if it appears after the
	// input ownership check, without GORM Save's unscoped upsert fallback.
	result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, UpdateAll: true,
		Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: memoryOwnershipSQL, Vars: []any{string(ScopeBot), s.botID, s.botID, string(ScopeBot), s.botID}}}},
	}).Create(model)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("tiered_store: upsert refused for a foreign owner")
	}
	return nil
}

func (s *TieredStore) tombstoneScope(scope Scope) Scope {
	if s.bound {
		return BotScope(s.botID)
	}
	return scope
}

// BoundBotID exposes the trusted store owner; empty means no usable bot binding.
func (s *TieredStore) BoundBotID() string {
	if s == nil || !s.bound {
		return ""
	}
	return s.botID
}
