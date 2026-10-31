package storage

import (
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/util/errs"
	"gorm.io/gorm"
)

// Ownership is checked in SQL before limit/count/update/delete, including ID
// lookup paths. Unattributed legacy scopes are never assigned to a bot by guess.
const metadataBotID = "(CASE WHEN json_valid(metadata_json) THEN json_extract(metadata_json, '$.bot_id') ELSE NULL END)"

// BoundBotID exposes the trusted repository owner to access wrappers.
func (r *SQLiteRepository) BoundBotID() string { return r.config.BotID }

func (r *SQLiteRepository) owned(tx *gorm.DB) *gorm.DB {
	return ownedByBot(tx, r.config.BotID)
}

func ownedByBot(tx *gorm.DB, bot string) *gorm.DB {
	if bot == "" {
		return tx
	}
	return tx.Where("((scope_kind = ? AND scope_id = ? AND ("+metadataBotID+" IS NULL OR "+metadataBotID+" = ?)) OR (scope_kind <> ? AND "+metadataBotID+" = ?))", string(memory.ScopeBot), bot, bot, string(memory.ScopeBot), bot)
}

func (r *SQLiteRepository) forScope(tx *gorm.DB, scope memory.Scope) *gorm.DB {
	return r.forScopes(tx, []memory.Scope{scope})
}

func (r *SQLiteRepository) forScopes(tx *gorm.DB, scopes []memory.Scope) *gorm.DB {
	return scopesForBot(tx, r.config.BotID, scopes)
}

func scopesForBot(tx *gorm.DB, bot string, scopes []memory.Scope) *gorm.DB {
	tx = ownedByBot(tx, bot)
	if len(scopes) == 0 {
		return tx
	}
	if bot == "" {
		return tx.Where("(scope_kind, scope_id) IN ?", scopeConditions(scopes))
	}
	var allowed []memory.Scope
	for _, scope := range scopes {
		if scope.Kind == memory.ScopeAll || (scope.Kind == memory.ScopeBot && scope.ID == bot) {
			return tx
		}
		if scope.Kind != memory.ScopeBot {
			allowed = append(allowed, scope)
		}
	}
	if len(allowed) == 0 {
		return tx.Where("1 = 0")
	}
	return tx.Where("(scope_kind, scope_id) IN ?", scopeConditions(allowed))
}

func (r *SQLiteRepository) ownedEntry(entry memory.Entry) (memory.Entry, error) {
	bot := r.config.BotID
	if bot == "" {
		return entry, nil
	}
	if entry.Scope.Kind == memory.ScopeBot && entry.Scope.ID != bot {
		return entry, errs.New("sqlite_repository: memory belongs to another bot")
	}
	if id, exists := entry.Metadata["bot_id"]; exists && id != bot {
		return entry, errs.New("sqlite_repository: conflicting memory bot owner")
	}
	meta := make(map[string]any, len(entry.Metadata)+1)
	for k, v := range entry.Metadata {
		meta[k] = v
	}
	meta["bot_id"] = bot
	entry.Metadata = meta
	return entry, nil
}

func (r *SQLiteRepository) dedupStateKey(key string) string {
	if r.config.BotID == "" {
		return key
	}
	return "bot:" + r.config.BotID + "|" + key
}
