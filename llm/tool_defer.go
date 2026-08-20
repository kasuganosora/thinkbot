package llm

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Default eviction policy for loaded deferred tools. A loaded tool only stays
// fully present (with its input schema) while it is relevant; once it goes idle
// it is unloaded so the model-facing context shrinks back down.
const (
	// DefaultMaxLoaded is the soft cap on simultaneously-loaded deferred tools.
	// It bounds the breadth of context growth regardless of how many tools get
	// discovered. 0/negative means unlimited.
	DefaultMaxLoaded = 12
	// DefaultIdleEvictSteps unloads a loaded deferred tool after this many
	// orchestration steps without use. It bounds the lifetime of context growth.
	DefaultIdleEvictSteps = 6
)

// ToolDeferral manages lazy loading (and unloading) of deferred tools, mirroring
// Anthropic's defer_loading / tool-search behavior:
//
//   - A tool marked DeferredLoad is shown to the model with only its name and
//     description (no Parameters / input schema) until it is "loaded".
//   - The model discovers deferred tools via the injected tool_search tool, or
//     the orchestrator auto-loads a tool the moment the model references it by
//     name. Once loaded, the tool's full schema is included so the model can
//     call it with arguments.
//   - Loaded state is NOT permanent: a loaded tool that is not used for
//     IdleEvict steps is unloaded (schema re-hidden), and the soft MaxLoaded cap
//     evicts least-recently-used tools when too many are loaded at once. This
//     keeps the model-facing context bounded instead of growing forever.
//   - Loaded state persists for the life of the ToolDeferral instance. Callers
//     typically create one per bot/session so discovered tools stay available
//     across turns (matching Claude's "once discovered, reuse in later turns").
type ToolDeferral struct {
	mu sync.Mutex

	// logger optionally records deferral activity (view/load) at DEBUG level
	// so the feature is observable from logs. Nil disables logging.
	logger *zap.SugaredLogger

	enabled bool
	loaded  map[string]bool
	// lastUsed records the orchestration step when a tool was last loaded or
	// touched (used since), for idle eviction.
	lastUsed map[string]int
	// recency tracks load/use order for LRU eviction (front = least recent).
	recency []string
	// maxLoaded is the soft cap on simultaneously-loaded deferred tools.
	maxLoaded int
	// idleEvict unloads a loaded tool unused for this many steps.
	idleEvict int
	// step is the current orchestration step, advanced via SetStep.
	step int
	full []Tool
}

// NewToolDeferral creates a deferral manager. When enabled is false, View
// returns all tools unchanged and no tool_search tool is injected. Loaded
// tools use the default eviction policy (DefaultMaxLoaded / DefaultIdleEvictSteps);
// call SetCapacity to override.
func NewToolDeferral(enabled bool) *ToolDeferral {
	return &ToolDeferral{
		enabled:   enabled,
		loaded:    make(map[string]bool),
		lastUsed:  make(map[string]int),
		maxLoaded: DefaultMaxLoaded,
		idleEvict: DefaultIdleEvictSteps,
	}
}

// SetLogger enables DEBUG-level logging of deferral activity (view/load).
// Optional; returns the receiver for chaining.
func (d *ToolDeferral) SetLogger(l *zap.SugaredLogger) *ToolDeferral {
	d.mu.Lock()
	d.logger = l
	d.mu.Unlock()
	return d
}

// CountDeferred returns how many tools in the installed list are deferred.
func (d *ToolDeferral) CountDeferred() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.countDeferredLocked()
}

func (d *ToolDeferral) countDeferredLocked() int {
	n := 0
	for i := range d.full {
		if d.full[i].DeferredLoad {
			n++
		}
	}
	return n
}

// SetTools installs the full tool list (with Parameters + Execute) used both to
// build the execution map and to search. Call once per orchestration request,
// after schemas are resolved and names are finalized (e.g. sandbox prefixes
// stripped).
func (d *ToolDeferral) SetTools(full []Tool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.full = full
}

// HasDeferred reports whether the installed tool list contains any deferred tool.
func (d *ToolDeferral) HasDeferred() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hasDeferredLocked()
}

func (d *ToolDeferral) hasDeferredLocked() bool {
	for i := range d.full {
		if d.full[i].DeferredLoad {
			return true
		}
	}
	return false
}

// Load marks a deferred tool as loaded and applies the eviction policy.
func (d *ToolDeferral) Load(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked(name)
}

// loadLocked marks name loaded, refreshes its recency, and enforces eviction.
// Caller must hold d.mu.
func (d *ToolDeferral) loadLocked(name string) {
	if !d.loaded[name] {
		d.loaded[name] = true
	}
	d.lastUsed[name] = d.step
	d.bumpRecencyLocked(name)
	d.evictLocked()
}

// IsLoaded reports whether name is currently loaded.
func (d *ToolDeferral) IsLoaded(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.loaded[name]
}

// Search returns deferred (not-yet-loaded) tools whose name, description, or
// keywords match query (case-insensitive substring), and marks them loaded so
// they become directly callable. An empty query matches all deferred tools.
func (d *ToolDeferral) Search(query string) []Tool {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	var hits []Tool
	for i := range d.full {
		t := d.full[i]
		if !t.DeferredLoad || d.loaded[t.Name] {
			continue
		}
		if q == "" || toolMatches(t, q) {
			hits = append(hits, t)
			d.loadLocked(t.Name)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Name < hits[j].Name })
	return hits
}

func toolMatches(t Tool, q string) bool {
	if strings.Contains(strings.ToLower(t.Name), q) {
		return true
	}
	if strings.Contains(strings.ToLower(t.Description), q) {
		return true
	}
	for _, k := range t.Keywords {
		if strings.Contains(strings.ToLower(k), q) {
			return true
		}
	}
	return false
}

// View builds the tool list shown to the model:
//   - non-deferred tools: unchanged (full schema)
//   - deferred tools: full schema if loaded, otherwise only name + description
//     (Parameters stripped)
//   - when enabled and there is still at least one deferred tool that is not
//     loaded, a tool_search tool is appended so the model can discover the rest
//     on demand.
//
// The returned slice is independent of the installed full list (struct copies),
// so callers may mutate it (e.g. stripSandboxPrefixes) without affecting the
// execution map.
func (d *ToolDeferral) View() []Tool {
	d.mu.Lock()
	// Disabled: return the full list unchanged (no stripping, no search tool).
	if !d.enabled {
		out := make([]Tool, len(d.full))
		copy(out, d.full)
		d.mu.Unlock()
		return out
	}
	out := make([]Tool, 0, len(d.full)+1)
	for _, t := range d.full {
		if t.DeferredLoad && !d.loaded[t.Name] {
			stripped := t
			stripped.Parameters = nil
			out = append(out, stripped)
			continue
		}
		out = append(out, t)
	}
	hasUnloaded := d.hasUnloadedLocked()
	deferredTotal := d.countDeferredLocked()
	loaded := len(d.loaded)
	d.mu.Unlock()

	if d.logger != nil && hasUnloaded {
		d.logger.Debugw("defer_loading: view",
			"deferred_total", deferredTotal,
			"loaded", loaded,
			"hidden", deferredTotal-loaded,
			"tool_search_injected", true)
	}

	if hasUnloaded {
		out = append(out, d.searchTool())
	}
	return out
}

// ExecTool returns the tool_search tool for inclusion in the execution map, so
// that when the model calls it the orchestrator can run the search. Its Execute
// closure captures this deferral.
func (d *ToolDeferral) ExecTool() Tool {
	return d.searchTool()
}

func (d *ToolDeferral) searchTool() Tool {
	return Tool{
		Name: "tool_search",
		Description: "Search for additional tools by keyword. Use this when you need a capability " +
			"that is not in your current tool list — for example, tools that were lazily loaded and " +
			"only expose their name and a short description until discovered. Returns the matching " +
			"tool names and descriptions; once found, a tool becomes directly callable with its full " +
			"parameters and input schema.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Keywords to search tool names, descriptions, and tags (case-insensitive substring).",
				},
			},
			"required": []any{"query"},
		},
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			query := ""
			if m, ok := input.(map[string]any); ok {
				if v, ok := m["query"].(string); ok {
					query = v
				}
			}
			hits := d.Search(query)
			if len(hits) == 0 {
				if d.logger != nil {
					d.logger.Debugw("defer_loading: tool_search", "query", query, "loaded", []string{})
				}
				return "No matching tools found. Try different keywords, or describe the capability you need.", nil
			}
			if d.logger != nil {
				names := make([]string, len(hits))
				for i, h := range hits {
					names[i] = h.Name
				}
				d.logger.Debugw("defer_loading: tool_search", "query", query, "loaded", names)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Found %d tool(s). They are now available for direct use:\n", len(hits))
			for _, t := range hits {
				fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
			}
			return b.String(), nil
		},
	}
}

// loadNote builds a user message telling the model that the named deferred
// tools were just loaded and should be called again with proper arguments.
func loadNote(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("Tool %q was lazily loaded and is now available with its full parameters and input schema. "+
			"Please call it now with the arguments you intended.", names[0])
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Sprintf("Tools %s were lazily loaded and are now available with their full parameters and input schema. "+
		"Please call them now with the arguments you intended.", strings.Join(quoted, ", "))
}

// SetCapacity configures the eviction policy for loaded deferred tools.
// maxLoaded is the soft upper bound on simultaneously-loaded deferred tools
// (0 or negative = unlimited). idleEvict unloads a loaded deferred tool that
// has not been used for idleEvict steps (0 or negative = disabled). This keeps
// the model-facing context bounded: a tool is only fully present (with its
// input schema) while it is relevant, then reverts to name+description only.
func (d *ToolDeferral) SetCapacity(maxLoaded, idleEvict int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maxLoaded = maxLoaded
	d.idleEvict = idleEvict
	d.evictLocked()
}

// SetStep advances the orchestration step counter and runs eviction. Call once
// per orchestration step (before refreshing the view) so idle tools are
// unloaded and the model-facing context shrinks.
func (d *ToolDeferral) SetStep(step int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.step = step
	d.evictLocked()
}

// Touch marks a loaded deferred tool as recently used, keeping it from being
// idle-evicted. Call after a deferred tool is actually executed.
func (d *ToolDeferral) Touch(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loaded[name] {
		d.lastUsed[name] = d.step
		d.bumpRecencyLocked(name)
	}
}

// Unload reverts a previously loaded deferred tool back to name+description only.
func (d *ToolDeferral) Unload(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unloadLocked(name)
}

// HasUnloaded reports whether any deferred tool is not yet loaded (so it is
// still hidden behind name+description only). Used to decide whether the
// tool_search discovery tool should be injected.
func (d *ToolDeferral) HasUnloaded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hasUnloadedLocked()
}

func (d *ToolDeferral) hasUnloadedLocked() bool {
	for i := range d.full {
		t := d.full[i]
		if t.DeferredLoad && !d.loaded[t.Name] {
			return true
		}
	}
	return false
}

// LoadedCount returns how many deferred tools are currently loaded (full schema
// exposed to the model). Useful for tests and diagnostics.
func (d *ToolDeferral) LoadedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, l := range d.loaded {
		if l {
			n++
		}
	}
	return n
}

// bumpRecencyLocked moves name to the most-recent end of d.recency.
// Caller must hold d.mu.
func (d *ToolDeferral) bumpRecencyLocked(name string) {
	for i, n := range d.recency {
		if n == name {
			d.recency = append(d.recency[:i], d.recency[i+1:]...)
			break
		}
	}
	d.recency = append(d.recency, name)
}

// evictLocked applies the eviction policy:
//   - idle eviction: unload loaded tools unused for idleEvict steps;
//   - soft capacity: evict least-recently-used loaded tools beyond maxLoaded,
//     but never a tool used in the current step (so a freshly tool_search-loaded
//     batch stays available this step even if it temporarily exceeds the cap).
//
// Caller must hold d.mu.
func (d *ToolDeferral) evictLocked() {
	if d.idleEvict > 0 {
		for name := range d.loaded {
			if d.step-d.lastUsed[name] > d.idleEvict {
				d.unloadLocked(name)
			}
		}
	}
	if d.maxLoaded > 0 {
		for len(d.loaded) > d.maxLoaded {
			if len(d.recency) == 0 {
				break
			}
			cand := d.recency[0]
			if !d.loaded[cand] {
				d.recency = d.recency[1:]
				continue
			}
			// Don't evict a tool used in the current step — protects search
			// results returned this step from being yanked out from under the
			// model before it can call them.
			if d.step-d.lastUsed[cand] <= 0 {
				break
			}
			d.unloadLocked(cand)
			d.recency = d.recency[1:]
		}
	}
}

// unloadLocked reverts name to the unloaded state. Caller must hold d.mu.
func (d *ToolDeferral) unloadLocked(name string) {
	delete(d.loaded, name)
	delete(d.lastUsed, name)
	for i, n := range d.recency {
		if n == name {
			d.recency = append(d.recency[:i], d.recency[i+1:]...)
			break
		}
	}
}

// ============================================================================
// Per-session deferral store
// ============================================================================

// DeferralStore owns one ToolDeferral per conversation (session) so that
// deferred-tool load state is isolated per conversation instead of leaking
// across concurrent conversations that share the same bot. This matches
// Claude's per-session defer_loading semantics and avoids step-counter races
// when several conversations run at once.
//
// Callers create one store per bot (enabled) and resolve it per request via
// ForSession(sessionID). An empty session id falls back to a single shared
// deferral, preserving the previous behavior rather than disabling deferral.
type DeferralStore struct {
	mu sync.Mutex
	// enabled mirrors ToolDeferral.enabled; ForSession returns nil when false.
	enabled bool
	// logger, if set, is inherited by every ToolDeferral the store creates.
	logger *zap.SugaredLogger
	// sessions maps a session id to its ToolDeferral, created lazily.
	sessions map[string]*ToolDeferral
	// fallback is used when no session id is available (rare); it keeps the
	// previous per-bot semantics instead of turning deferral off.
	fallback *ToolDeferral
}

// NewDeferralStore creates a per-session deferral store. When enabled is false,
// ForSession returns nil and the orchestrator bypasses deferral.
func NewDeferralStore(enabled bool) *DeferralStore {
	return &DeferralStore{
		enabled:  enabled,
		sessions: make(map[string]*ToolDeferral),
	}
}

// SetLogger enables DEBUG logging for all ToolDeferrals created by the store.
// Optional; returns the receiver for chaining.
func (s *DeferralStore) SetLogger(l *zap.SugaredLogger) *DeferralStore {
	s.mu.Lock()
	s.logger = l
	s.mu.Unlock()
	return s
}

// ForSession returns the ToolDeferral for the given session id, creating it on
// first use. An empty session id falls back to a single shared deferral.
// Returns nil when the store is disabled.
func (s *DeferralStore) ForSession(sid string) *ToolDeferral {
	if !s.enabled {
		return nil
	}
	if sid == "" {
		s.mu.Lock()
		if s.fallback == nil {
			s.fallback = NewToolDeferral(true).SetLogger(s.logger)
		}
		fb := s.fallback
		s.mu.Unlock()
		return fb
	}
	s.mu.Lock()
	d, ok := s.sessions[sid]
	if !ok {
		d = NewToolDeferral(true).SetLogger(s.logger)
		s.sessions[sid] = d
	}
	s.mu.Unlock()
	return d
}
