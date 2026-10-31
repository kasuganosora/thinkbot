package memory

import (
	"context"
	"fmt"
)

// ForBotRepository confines tool access to a server-selected bot. Preserve the
// source scope (channel/user/bot) for profiling; ownership is a separate boundary.
func ForBotRepository(repo Repository, botID string) Repository {
	return &botRepository{Repository: repo, botID: botID}
}

type botRepository struct {
	Repository
	botID string
}

func (r *botRepository) BoundBotID() string { return r.botID }

func (r *botRepository) owns(e Entry) bool {
	if r.botID == "" {
		return false
	}
	if e.Scope.Kind == ScopeBot {
		owner := e.Metadata["bot_id"]
		return e.Scope.ID == r.botID && (owner == nil || owner == r.botID)
	}
	owner, _ := e.Metadata["bot_id"].(string)
	return owner == r.botID
}
func (r *botRepository) allowed(s Scope) bool {
	return r.botID != "" && (s.Kind != ScopeBot || s.ID == r.botID)
}
func (r *botRepository) filter(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if r.owns(e) {
			out = append(out, e)
		}
	}
	return out
}
func (r *botRepository) Retrieve(ctx context.Context, q Query) ([]Entry, error) {
	if r.botID == "" {
		return nil, fmt.Errorf("memory: bot identity is unavailable")
	}
	for _, s := range q.Scopes {
		if !r.allowed(s) {
			return nil, nil
		}
	}
	limit := q.Limit
	if q.Limit <= 0 {
		q.Limit = int(^uint(0) >> 1)
	}
	// Production SQLite enforces the ownership predicate before limit. The
	// in-memory/test implementation has no binding, so filter before truncation.
	if bound, ok := r.Repository.(interface{ BoundBotID() string }); ok && bound.BoundBotID() == r.botID {
		q.Scopes = []Scope{BotScope(r.botID)}
	} else {
		q.Scopes = nil
		q.Limit = int(^uint(0) >> 1)
	}
	entries, err := r.Repository.Retrieve(ctx, q)
	entries = r.filter(entries)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, err
}
func (r *botRepository) Recent(ctx context.Context, s Scope, n int) ([]Entry, error) {
	return r.Retrieve(ctx, Query{Scopes: []Scope{s}, Limit: n})
}
func (r *botRepository) Count(ctx context.Context, s Scope) (int, error) {
	if !r.allowed(s) {
		return 0, fmt.Errorf("memory: scope outside this bot")
	}
	if bound, ok := r.Repository.(interface{ BoundBotID() string }); ok && bound.BoundBotID() == r.botID {
		return r.Repository.Count(ctx, BotScope(r.botID))
	}
	entries, err := r.Retrieve(ctx, Query{Scopes: []Scope{s}})
	return len(entries), err
}
func (r *botRepository) stamp(e Entry) Entry {
	meta := make(map[string]any, len(e.Metadata)+1)
	for k, v := range e.Metadata {
		meta[k] = v
	}
	meta["bot_id"] = r.botID
	e.Metadata = meta
	return e
}
func (r *botRepository) Append(ctx context.Context, e Entry) error {
	if !r.allowed(e.Scope) || e.Scope.Kind == ScopeAll {
		return fmt.Errorf("memory: scope outside this bot")
	}
	return r.Repository.Append(ctx, r.stamp(e))
}
func (r *botRepository) find(ctx context.Context, id string) (Entry, bool, error) {
	if fetcher, ok := r.Repository.(EntryFetcher); ok {
		entries, err := fetcher.FetchEntries(ctx, []string{id})
		for _, e := range entries {
			if e.ID == id && r.owns(e) {
				return e, true, err
			}
		}
		return Entry{}, false, err
	}
	entries, err := r.Retrieve(ctx, Query{})
	for _, e := range entries {
		if e.ID == id {
			return e, true, err
		}
	}
	return Entry{}, false, err
}
func (r *botRepository) Delete(ctx context.Context, s Scope, id string) error {
	if !r.allowed(s) {
		return fmt.Errorf("memory: scope outside this bot")
	}
	e, ok, err := r.find(ctx, id)
	if err != nil || !ok {
		return err
	}
	return r.Repository.Delete(ctx, e.Scope, id)
}
func (r *botRepository) Clear(ctx context.Context, s Scope) error {
	if !r.allowed(s) {
		return fmt.Errorf("memory: scope outside this bot")
	}
	entries, err := r.Retrieve(ctx, Query{})
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := r.Repository.Delete(ctx, e.Scope, e.ID); err != nil {
			return err
		}
	}
	return nil
}
func (r *botRepository) Replace(ctx context.Context, s Scope, id string, e Entry) error {
	if !r.allowed(s) || !r.allowed(e.Scope) || s.Kind == ScopeAll || e.Scope.Kind == ScopeAll {
		return fmt.Errorf("memory: scope outside this bot")
	}
	old, exists, err := r.find(ctx, id)
	if err != nil {
		return err
	}
	if exists {
		s = old.Scope
		e.Scope = old.Scope
	}
	e = r.stamp(e)
	// Replacer may delete by ID in a shared raw scope. An ID absent from our
	// owned view must never be forwarded for deletion; mirrors use this as upsert.
	if !exists {
		return r.Repository.Append(ctx, e)
	}
	if replacer, ok := r.Repository.(Replacer); ok {
		return replacer.Replace(ctx, s, id, e)
	}
	if exists {
		if err := r.Repository.Delete(ctx, s, id); err != nil {
			return err
		}
	}
	if err := r.Repository.Append(ctx, e); err != nil {
		if exists {
			_ = r.Repository.Append(ctx, old)
		}
		return err
	}
	return nil
}
func (r *botRepository) FetchEntries(ctx context.Context, ids []string) ([]Entry, error) {
	fetcher, ok := r.Repository.(EntryFetcher)
	if !ok {
		return nil, nil
	}
	entries, err := fetcher.FetchEntries(ctx, ids)
	return r.filter(entries), err
}
func (r *botRepository) MemoryStats(ctx context.Context, scopes []Scope) (MemoryStatsInfo, error) {
	if r.botID == "" {
		return MemoryStatsInfo{}, fmt.Errorf("memory: bot identity is unavailable")
	}
	for _, s := range scopes {
		if !r.allowed(s) {
			return MemoryStatsInfo{}, fmt.Errorf("memory: scope outside this bot")
		}
	}
	if bound, ok := r.Repository.(interface{ BoundBotID() string }); ok && bound.BoundBotID() == r.botID {
		if stats, ok := r.Repository.(MemoryStatsProvider); ok {
			return stats.MemoryStats(ctx, []Scope{BotScope(r.botID)})
		}
	}
	entries, err := r.Retrieve(ctx, Query{Scopes: scopes})
	info := MemoryStatsInfo{Total: len(entries)}
	for _, e := range entries {
		observeSpan(&info, e.CreatedAt)
	}
	return info, err
}
func (r *botRepository) Vec() *VecIndex {
	if h, ok := r.Repository.(interface{ Vec() *VecIndex }); ok {
		return h.Vec()
	}
	return nil
}
