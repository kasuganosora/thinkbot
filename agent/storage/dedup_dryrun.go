package storage

import (
	"context"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
	"go.uber.org/zap"
)

// DedupDryRunOptions configures DryRunDedupGroups. Zero values use the
// production defaults (DefaultDedup*).
type DedupDryRunOptions struct {
	Scopes         []memory.Scope // empty: every scope with active entries
	MinSimilarity  float64
	TopK           int
	MaxSeeds       int // per scope; <0 = unlimited
	MaxGroup       int
	MaxMergedChars int
	// AllSeeds treats every active entry (except compactor output, digests
	// and mirrors) as a seed, ignoring dedup_checked_at.
	AllSeeds bool
	// Reindex rebuilds memory_vec from the active entries first (writes the
	// DB: run on a copy).
	Reindex bool
}

// DedupDryRunNeighbour is one group member with its similarity to the seed.
type DedupDryRunNeighbour struct {
	Entry      memory.Entry
	Similarity float64
}

// DedupDryRunScope is the selection dedup would make for one scope.
type DedupDryRunScope struct {
	Scope     memory.Scope
	Mode      string // "vec", "recent" (memory_vec off) or "listen" (skipped)
	Seeds     int
	Oversized []string
	Lonely    []string
	Groups    [][]DedupDryRunNeighbour // seed first (Similarity 1)
}

// DryRunDedupGroups returns the groups the dedup pass would send to the LLM,
// without calling the LLM or writing dedup state. Same selection code as
// compactScope (buildDedupGroups / filterNeighbours / buildFallbackGroups).
func (r *SQLiteRepository) DryRunDedupGroups(ctx context.Context, opt DedupDryRunOptions) ([]DedupDryRunScope, error) {
	sel := dedupSelectConfig{
		topK: opt.TopK, minSim: opt.MinSimilarity, maxSeeds: opt.MaxSeeds,
		fallbackRecent: DefaultDedupFallbackRecent, maxGroup: opt.MaxGroup,
		maxMergedChars: opt.MaxMergedChars, noopSim: DefaultDedupNoopSimilarity,
	}
	if sel.topK <= 0 {
		sel.topK = DefaultDedupNeighbourTopK
	}
	if sel.minSim <= 0 {
		sel.minSim = DefaultDedupMinSimilarity
	}
	if sel.maxSeeds == 0 {
		sel.maxSeeds = DefaultDedupMaxSeeds
	}
	if sel.maxGroup < 2 {
		sel.maxGroup = 10
	}
	if sel.maxMergedChars <= 0 {
		sel.maxMergedChars = DefaultDedupMaxMergedChars
	}
	c := &SQLiteCompactor{repo: r, sel: sel, logger: zap.NewNop().Sugar()}

	scopes := opt.Scopes
	if len(scopes) == 0 {
		var rows []struct{ ScopeKind, ScopeID string }
		if err := r.owned(r.db.WithContext(ctx)).Model(&dao.EntryModel{}).Where(activeWhere, activeArgs...).
			Distinct("scope_kind", "scope_id").Order("scope_kind, scope_id").Find(&rows).Error; err != nil {
			return nil, errs.Wrap(err, "dedup dry-run: list scopes")
		}
		for _, row := range rows {
			scopes = append(scopes, memory.Scope{Kind: memory.ScopeKind(row.ScopeKind), ID: row.ScopeID})
		}
	}
	vec := r.Vec()
	if opt.Reindex && r.config.BotID != "" {
		return nil, errs.New("dedup dry-run: shared vector rebuild requires an unscoped maintenance repository")
	}
	if opt.Reindex && vec.Enabled() {
		if err := vec.Rebuild(); err != nil {
			return nil, errs.Wrap(err, "dedup dry-run: rebuild memory_vec")
		}
		var models []dao.EntryModel
		if err := r.owned(r.db.WithContext(ctx)).Where(activeWhere, activeArgs...).Find(&models).Error; err != nil {
			return nil, errs.Wrap(err, "dedup dry-run: load entries")
		}
		for _, e := range modelsToActiveEntries(models) {
			vec.Upsert(memory.VectorScopeKey(e.Scope, e.Metadata), e.ID, e.Content)
		}
	}

	var out []DedupDryRunScope
	for _, scope := range scopes {
		res := DedupDryRunScope{Scope: scope, Mode: "vec"}
		if isListenScope(scope) {
			res.Mode = "listen"
			out = append(out, res)
			continue
		}
		var cands []memory.Entry
		var err error
		limit := sel.maxSeeds
		if limit < 0 {
			limit = 0
		}
		if opt.AllSeeds {
			var models []dao.EntryModel
			q := r.scopeQuery(ctx, scope).Order("created_at ASC")
			if limit > 0 {
				q = q.Limit(limit)
			}
			err = q.Find(&models).Error
			for _, e := range modelsToActiveEntries(models) {
				if !precheckedForDedup(e) {
					cands = append(cands, e)
				}
			}
		} else {
			cands, err = r.UncheckedDedup(ctx, scope, limit)
		}
		if err != nil {
			return nil, errs.Wrap(err, "dedup dry-run: seeds")
		}
		var seeds []memory.Entry
		for _, e := range cands {
			if c.oversized(e) {
				res.Oversized = append(res.Oversized, e.ID)
				continue
			}
			seeds = append(seeds, e)
		}
		res.Seeds = len(seeds)
		var groups [][]memory.Entry
		if vec.Enabled() {
			groups, res.Lonely = buildDedupGroups(seeds, func(seed memory.Entry) []memory.Entry {
				return c.vecNeighbours(ctx, scope, vec, seed)
			}, sel.maxGroup)
		} else {
			res.Mode = "recent"
			recent, _ := r.RecentCheckedDedup(ctx, scope, sel.fallbackRecent*3)
			var small []memory.Entry
			for _, e := range recent {
				if !c.oversized(e) && len(small) < sel.fallbackRecent {
					small = append(small, e)
				}
			}
			groups, res.Lonely = buildFallbackGroups(seeds, small, sel.maxGroup)
		}
		for _, g := range groups {
			row := make([]DedupDryRunNeighbour, len(g))
			for i, e := range g {
				row[i] = DedupDryRunNeighbour{Entry: e, Similarity: memory.EmbedSimilarity(g[0].Content, e.Content)}
			}
			res.Groups = append(res.Groups, row)
		}
		out = append(out, res)
	}
	return out, nil
}
