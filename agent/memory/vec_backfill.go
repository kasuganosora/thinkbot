package memory

import (
	"context"
	stdlog "log"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// Backfill log events (stable keywords, grep-friendly):
//
//	memory_vec_backfill_start  table=<tiered_memories|memory_entries> total present missing
//	memory_vec_backfill_done   table written failed missing took
//	memory_vec_backfill_error  table stage=<list|upsert> [entry_id] err
//
// "missing" counts indexable rows (id + content) without a vector. Vectors of
// rows that no longer exist are not touched here.
const (
	VecBackfillStartEvent = "memory_vec_backfill_start"
	VecBackfillDoneEvent  = "memory_vec_backfill_done"
	VecBackfillErrorEvent = "memory_vec_backfill_error"
	VecSchemeResetEvent   = "memory_vec_scheme_reset"

	// at most this many per-entry upsert errors are logged individually.
	vecBackfillErrorLogLimit = 5
)

// VecItem is one entry to index.
type VecItem struct{ ScopeKey, EntryID, Content string }

// VecBackfillStats is the result of one backfill.
type VecBackfillStats struct {
	Total, Present, Missing, Written, Failed int
	Took                                     time.Duration
}

func vecLog(logger *zap.SugaredLogger, level, event string, kv ...any) {
	if logger != nil {
		logger = logger.WithOptions(zap.AddCallerSkip(1))
		switch level {
		case "error":
			logger.Errorw(event, kv...)
		default:
			logger.Infow(event, kv...)
		}
		return
	}
	args := []any{level, event}
	format := "%s %s"
	for i := 0; i+1 < len(kv); i += 2 {
		format += " %v=%v"
		args = append(args, kv[i], kv[i+1])
	}
	stdlog.Printf(format, args...)
}

// BackfillMissing writes vectors only for entry_ids not yet in memory_vec
// (existing rows are left alone) and logs start / done / errors for table.
func (v *VecIndex) BackfillMissing(ctx context.Context, table string, items []VecItem, logger *zap.SugaredLogger) (VecBackfillStats, error) {
	var st VecBackfillStats
	if !v.Enabled() {
		return st, nil
	}
	// One backfill per DB at a time: the app-level run and a bot's TieredStore
	// run wait for each other; the later one finds the rows present.
	v.backfillMu.Lock()
	defer v.backfillMu.Unlock()
	started := time.Now()
	present, err := v.listEntryIDs(ctx)
	if err != nil {
		vecLog(logger, "error", VecBackfillErrorEvent, "table", table, "stage", "list", "err", err)
		return st, err
	}
	var todo []VecItem
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		if it.EntryID == "" || it.Content == "" {
			continue
		}
		if _, dup := seen[it.EntryID]; dup {
			continue
		}
		seen[it.EntryID] = struct{}{}
		st.Total++
		if _, ok := present[it.EntryID]; ok {
			st.Present++
			continue
		}
		todo = append(todo, it)
	}
	st.Missing = len(todo)
	vecLog(logger, "info", VecBackfillStartEvent, "table", table, "total", st.Total, "present", st.Present, "missing", st.Missing)
	var firstErr error
	for _, it := range todo {
		if err := ctx.Err(); err != nil {
			firstErr = err
			break
		}
		ok, err := v.upsert(it.ScopeKey, it.EntryID, it.Content, true)
		if err != nil {
			st.Failed++
			if firstErr == nil {
				firstErr = err
			}
			if st.Failed <= vecBackfillErrorLogLimit {
				vecLog(logger, "error", VecBackfillErrorEvent, "table", table, "stage", "upsert", "entry_id", it.EntryID, "err", err)
			}
			continue
		}
		if ok {
			st.Written++
		}
	}
	st.Took = time.Since(started)
	vecLog(logger, "info", VecBackfillDoneEvent, "table", table, "written", st.Written, "failed", st.Failed,
		"missing", st.Missing, "took", st.Took.Round(time.Millisecond).String())
	return st, firstErr
}

// BackfillTieredVectors indexes non-expired tiered_memories rows that have no
// vector (same rows a TieredStore loads at bot start), without any bot running.
func BackfillTieredVectors(ctx context.Context, gdb *gorm.DB, idx *VecIndex, logger *zap.SugaredLogger) (VecBackfillStats, error) {
	if gdb == nil || !idx.Enabled() {
		return VecBackfillStats{}, nil
	}
	var models []dao.TieredMemoryModel
	if err := gdb.WithContext(ctx).Find(&models).Error; err != nil {
		vecLog(logger, "error", VecBackfillErrorEvent, "table", "tiered_memories", "stage", "load", "err", err)
		return VecBackfillStats{}, err
	}
	now := time.Now()
	items := make([]VecItem, 0, len(models))
	for _, m := range models {
		if !m.ExpiresAt.IsZero() && now.After(m.ExpiresAt) {
			continue
		}
		te := modelToTiedEntry(m)
		items = append(items, VecItem{ScopeKey: te.Scope.Key(), EntryID: te.ID, Content: te.Content})
	}
	return idx.BackfillMissing(ctx, "tiered_memories", items, logger)
}
