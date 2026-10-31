package storage

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Incremental memory dedup state
//
// memory_entries.dedup_checked_at marks entries that went through a dedup
// pass without being merged; memory_compact_states persists the per-scope
// cooldown. Together they make memory_dedup incremental: only new
// (unchecked) entries and their nearest neighbours are sent to the LLM.
// ============================================================================

// Defaults of the incremental dedup trigger.
const (
	// DefaultDedupTriggerNew is the number of unchecked entries in a scope
	// that triggers a dedup pass even when the scope is under its char budget.
	DefaultDedupTriggerNew = 8
	// DefaultDedupCooldown is the minimum interval between two automatic
	// dedup passes of one scope (persisted, survives restarts).
	DefaultDedupCooldown = compactCooldown
)

// activeWhere filters out archived and compact-skipped entries.
const activeWhere = "(metadata_json IS NULL OR (metadata_json NOT LIKE ? AND metadata_json NOT LIKE ?))"

var activeArgs = []any{"%\"archived\":true%", "%\"compact_skipped\":true%"}

// precheckedForDedup reports whether an entry is stored as already checked:
// compactor output (it is the result of a dedup pass) and cross-channel
// mirror copies (deduplicated in their own channel scope).
func precheckedForDedup(e memory.Entry) bool {
	return e.Source == "compactor" || e.Source == DailyDigestSource || memory.IsMirrorID(e.ID)
}

// isListenScope reports whether scope is a passive listen feed (see
// core.RegisterListenChannel): digested daily, excluded from dedup.
func isListenScope(scope memory.Scope) bool {
	return scope.Kind == memory.ScopeChannel && core.IsListenChannel(scope.ID)
}

func (r *SQLiteRepository) scopeQuery(ctx context.Context, scope memory.Scope) *gorm.DB {
	return r.forScope(r.db.WithContext(ctx).Model(&dao.EntryModel{}), scope).
		Where(activeWhere, activeArgs...)
}

// CountUncheckedDedup counts active entries of scope not yet seen by dedup.
func (r *SQLiteRepository) CountUncheckedDedup(ctx context.Context, scope memory.Scope) (int, error) {
	var n int64
	if err := r.scopeQuery(ctx, scope).Where("dedup_checked_at IS NULL AND (source IS NULL OR source <> ?)", "compactor").Count(&n).Error; err != nil {
		return 0, errs.Wrap(err, "sqlite_repository: count unchecked failed")
	}
	return int(n), nil
}

// UncheckedDedup returns up to limit active unchecked entries, oldest first.
func (r *SQLiteRepository) UncheckedDedup(ctx context.Context, scope memory.Scope, limit int) ([]memory.Entry, error) {
	var models []dao.EntryModel
	// Compactor output is never a seed (only a neighbour): re-seeding merged
	// entries made them absorb each other in chains (misskey:timeline 10/15
	// active entries were compactor output, chains up to 84 deep).
	q := r.scopeQuery(ctx, scope).Where("dedup_checked_at IS NULL AND (source IS NULL OR source <> ?)", "compactor").Order("created_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: unchecked entries failed")
	}
	return modelsToActiveEntries(models), nil
}

// ActiveByIDs returns the active entries of scope among ids.
func (r *SQLiteRepository) ActiveByIDs(ctx context.Context, scope memory.Scope, ids []string) ([]memory.Entry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var models []dao.EntryModel
	if err := r.scopeQuery(ctx, scope).Where("id IN ?", ids).Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: entries by id failed")
	}
	return modelsToActiveEntries(models), nil
}

// RecentCheckedDedup returns up to limit of the newest active entries that
// were already checked (the fallback neighbourhood when memory_vec is off).
func (r *SQLiteRepository) RecentCheckedDedup(ctx context.Context, scope memory.Scope, limit int) ([]memory.Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	var models []dao.EntryModel
	if err := r.scopeQuery(ctx, scope).Where("dedup_checked_at IS NOT NULL").
		Where("id NOT LIKE ?", "xch:%").
		Order("created_at DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, errs.Wrap(err, "sqlite_repository: recent checked failed")
	}
	return modelsToActiveEntries(models), nil
}

// MarkDedupChecked stamps dedup_checked_at on the given entries.
func (r *SQLiteRepository) MarkDedupChecked(ctx context.Context, scope memory.Scope, ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	res := r.forScope(r.db.WithContext(ctx).Model(&dao.EntryModel{}), scope).
		Where("id IN ?", ids).
		Update("dedup_checked_at", time.Now())
	if res.Error != nil {
		return 0
	}
	return int(res.RowsAffected)
}

// lastDedupRun returns the persisted time of the last automatic dedup pass.
func (r *SQLiteRepository) lastDedupRun(ctx context.Context, key string) (time.Time, bool) {
	key = r.dedupStateKey(key)
	if v, ok := r.lastCompact.Load(key); ok {
		if t, ok := v.(time.Time); ok {
			return t, true
		}
	}
	var st dao.MemoryCompactState
	if err := r.db.WithContext(ctx).Where("scope_key = ?", key).Limit(1).Find(&st).Error; err != nil || st.ScopeKey == "" {
		return time.Time{}, false
	}
	r.lastCompact.Store(key, st.LastRunAt)
	return st.LastRunAt, true
}

// storeDedupRun persists the time of an automatic dedup pass.
func (r *SQLiteRepository) storeDedupRun(ctx context.Context, key string, t time.Time) {
	key = r.dedupStateKey(key)
	r.lastCompact.Store(key, t)
	_ = r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_run_at"}),
	}).Create(&dao.MemoryCompactState{ScopeKey: key, LastRunAt: t}).Error
}

func modelsToActiveEntries(models []dao.EntryModel) []memory.Entry {
	out := make([]memory.Entry, 0, len(models))
	for _, m := range models {
		e := modelToEntry(m)
		if isEntryArchived(e.Metadata) || isEntryCompactSkipped(e.Metadata) {
			continue
		}
		out = append(out, e)
	}
	return out
}
