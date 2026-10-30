package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// Deep recall is the on-demand "think to recall" path of action=search.
//
// The model opts in by calling search with no query (and no time-browse
// params) or by setting deep=true. An explicit query stays on the cheap
// lexical path. Nothing here runs before the model asks: SelectImportant and
// the automatic related-memory inject are unchanged, and there is no
// session-sticky upgrade.
//
// The path may call the model twice (keywords, then refine), search the
// existing lexical index and the existing vector index when one is wired,
// then drop hits that are already covered by the last few visible turns.
// A per-turn call cap, a timeout, and a result-size cap are hard. Exceeding
// any of them returns a short closed result and does not hang the turn.

const (
	deepKeywordMarker = "TASK: memory-search-keywords"
	deepRefineMarker  = "TASK: memory-search-refine"

	deepRecallBudgetName     = "memory_deep_recall"
	deepRecallDefaultCalls   = 1
	deepRecallDefaultTimeout = 12 * time.Second
	deepRecallMaxResults     = 5
	deepRecallMaxChars       = 1200
	deepRecallEntryChars     = 280
	deepRecallRecentTurns    = 4
	deepRecallMaxKeywords    = 6
	deepRecallPerKeyword     = 8
	deepRecallMaxCandidates  = 24
	// deepCoverageMin is the share of a memory's tokens already present in
	// the recent visible window. Above this the memory is not new information.
	deepCoverageMin = 0.75
	// deepSummaryRunes is how thin the recent window must be before an older
	// topic summary is shown to the keyword model. The summary is never part
	// of the coverage window.
	deepSummaryRunes = 80
)

// VecQuerier is the existing hash-vector index. Deep recall uses it when one
// is already wired; it does not create an index.
type VecQuerier interface {
	Enabled() bool
	Search(scopeKey, query string, k int) []Hit
}

// DeepRecallConfig wires the on-demand loop. A nil Provider makes that loop
// fail closed; explicit queries do not consult it.
type DeepRecallConfig struct {
	Provider       llm.Provider
	Model          *llm.Model
	Policy         *llm.InternalPolicy
	Vec            VecQuerier
	ModelMaxTokens int

	// MaxCallsPerTurn, Timeout, MaxResults, and MaxResultChars override the
	// built-in budgets. Zero keeps the default.
	MaxCallsPerTurn int
	Timeout         time.Duration
	MaxResults      int
	MaxResultChars  int
}

func (c DeepRecallConfig) maxCalls() int {
	if c.MaxCallsPerTurn > 0 {
		return c.MaxCallsPerTurn
	}
	return deepRecallDefaultCalls
}

func (c DeepRecallConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return deepRecallDefaultTimeout
}

func (c DeepRecallConfig) maxResults() int {
	if c.MaxResults > 0 {
		return c.MaxResults
	}
	return deepRecallMaxResults
}

func (c DeepRecallConfig) maxChars() int {
	if c.MaxResultChars > 0 {
		return c.MaxResultChars
	}
	return deepRecallMaxChars
}

const (
	deepMsgNoModel = "Deep recall is unavailable. Pass query for a fast lexical search, or order/since/until to browse by time."
	deepMsgBudget  = "Deep recall budget exceeded for this turn. Pass query for a fast lexical search."
	deepMsgTimeout = "Deep recall timed out. Pass query for a fast lexical search."
	deepMsgFailed  = "Deep recall failed. Pass query for a fast lexical search."
	deepMsgEmpty   = "No related memory outside the recent conversation."
)

func deepClosed(msg string) map[string]any {
	return map[string]any{
		"count":   0,
		"type":    "search",
		"mode":    "deep",
		"message": msg,
		"entries": []EntryResult{},
	}
}

// deepRecallRequested reports whether this search call opted into the heavier
// loop. An explicit query stays light unless deep is true. order/since/until
// stay on the chronological browse even when query is empty.
func deepRecallRequested(m map[string]any) bool {
	if v, ok := optionalBool(m["deep"]); ok {
		return v
	}
	if stringField(m, "query") != "" {
		return false
	}
	if stringField(m, "order") != "" || stringField(m, "since") != "" || stringField(m, "until") != "" {
		return false
	}
	return true
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func optionalBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	}
	return false, false
}

// runDeepSearch is the tool entry. It enforces the turn cap and the timeout
// before any model call, and always returns to the caller.
func runDeepSearch(ctx *llm.ToolExecContext, repo Repository, cfg ToolConfig, scope Scope, m map[string]any, query string, since, until time.Time) (any, error) {
	parent := context.Context(context.Background())
	userRequest := ""
	if ctx != nil {
		userRequest = ctx.UserRequest
		if ctx.Context != nil {
			parent = ctx.Context
		}
	}
	recall := cfg.Recall
	n := llm.IncToolTurnBudget(parent, deepRecallBudgetName)
	if n > recall.maxCalls() {
		return deepClosed(deepMsgBudget), nil
	}
	if recall.Provider == nil {
		return deepClosed(deepMsgNoModel), nil
	}

	timeout := recall.timeout()
	tctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	type box struct {
		v   any
		err error
	}
	ch := make(chan box, 1)
	go func() {
		defer func() {
			if recover() != nil {
				ch <- box{v: deepClosed(deepMsgFailed)}
			}
		}()
		v, err := runDeepRecall(tctx, repo, recall, scope, m, query, userRequest, since, until)
		ch <- box{v: v, err: err}
	}()

	select {
	case got := <-ch:
		if got.err != nil {
			if errors.Is(got.err, context.DeadlineExceeded) || errors.Is(got.err, context.Canceled) || tctx.Err() != nil {
				return deepClosed(deepMsgTimeout), nil
			}
			return nil, got.err
		}
		if got.v == nil {
			return deepClosed(deepMsgFailed), nil
		}
		return got.v, nil
	case <-tctx.Done():
		return deepClosed(deepMsgTimeout), nil
	}
}

func runDeepRecall(ctx context.Context, repo Repository, recall DeepRecallConfig, scope Scope, m map[string]any, query, userRequest string, since, until time.Time) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	window, prompt := recentRecallView(ctx, userRequest, deepRecallRecentTurns)
	category := stringField(m, "category")

	planText, err := deepComplete(ctx, recall, deepKeywordSystem, prompt)
	if err != nil {
		return nil, err
	}
	var plan keywordPlan
	if err := parseJSONObject(planText, &plan); err != nil {
		return deepClosed(deepMsgFailed), nil
	}
	raw := plan.Keywords
	if t := strings.TrimSpace(plan.Topic); t != "" && len([]rune(t)) <= 40 {
		raw = append(raw, t)
	}
	keywords := normalizeKeywords(raw, query)
	if len(keywords) == 0 {
		return deepClosed(deepMsgFailed), nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	candidates, err := gatherDeepCandidates(ctx, repo, vecQuerier(recall, repo), scope, keywords, category, since, until)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errs.Wrap(err, "memory search failed")
	}
	if len(candidates) == 0 {
		return deepClosed(deepMsgEmpty), nil
	}

	refineText, err := deepComplete(ctx, recall, deepRefineSystem, refinePrompt(window, candidates))
	if err != nil {
		return nil, err
	}
	var refined refinePlan
	if err := parseJSONObject(refineText, &refined); err != nil {
		return deepClosed(deepMsgFailed), nil
	}
	byID := make(map[string]Entry, len(candidates))
	for _, e := range candidates {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}
	picked := make([]Entry, 0, len(refined.Keep))
	seen := map[string]struct{}{}
	for _, id := range refined.Keep {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		e, ok := byID[id]
		if !ok {
			continue
		}
		seen[id] = struct{}{}
		picked = append(picked, e)
	}
	picked = dropCoveredByRecent(picked, window)

	maxN := recall.maxResults()
	if l := toInt(m["limit"]); l > 0 && l < maxN {
		maxN = l
	}
	results := capDeepResults(picked, maxN, recall.maxChars())
	out := map[string]any{
		"count":   len(results),
		"type":    "search",
		"mode":    "deep",
		"entries": results,
	}
	if len(results) == 0 {
		out["message"] = deepMsgEmpty
	}
	return out, nil
}

const deepKeywordSystem = deepKeywordMarker + `
You expand a memory search. Reply with one JSON object only, no markdown:
{"keywords":["..."],"topic":""}
Provide 2 to 6 short keyword phrases that would match related past memories
(names, places, projects, preferences, aliases). Do not copy the utterance
verbatim. topic is a few words only when the recent turns are too thin to
show what the user means; otherwise leave it empty.`

const deepRefineSystem = deepRefineMarker + `
You filter memory candidates against the current utterance. Reply with one
JSON object only, no markdown: {"keep":["id",...]}.
Keep only candidates that add a fact the recent turns do not already state.
Prefer older, out-of-window facts. Drop chit-chat and near-copies of the
recent turns. At most 5 ids, most useful first. If none help, return {"keep":[]}.`

type keywordPlan struct {
	Keywords []string `json:"keywords"`
	Topic    string   `json:"topic"`
}

type refinePlan struct {
	Keep []string `json:"keep"`
}

func deepComplete(ctx context.Context, cfg DeepRecallConfig, system, user string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cfg.Provider == nil {
		return "", errors.New("deep recall model unavailable")
	}
	params := llm.GenerateParams{
		Model:    cfg.Model,
		System:   system,
		Messages: []llm.Message{llm.UserMessage(user)},
	}
	applyInternalCall(cfg.Policy, llm.PurposeMemoryRecall, &params, cfg.ModelMaxTokens)
	res, err := cfg.Provider.DoGenerate(llm.WithStatsFeature(ctx, llm.PurposeMemoryRecall), params)
	if err != nil {
		return "", err
	}
	if warnIfTruncated(nil, llm.PurposeMemoryRecall, res, params) {
		return "", errors.New("deep recall model output truncated")
	}
	return res.Text, nil
}

func refinePrompt(window string, candidates []Entry) string {
	var b strings.Builder
	b.WriteString("Recent visible turns:\n")
	b.WriteString(window)
	b.WriteString("\nCandidates:\n")
	for _, e := range candidates {
		b.WriteString(e.ID)
		b.WriteString(": ")
		b.WriteString(truncateRunes(e.Content, 180))
		b.WriteByte('\n')
	}
	return b.String()
}

func normalizeKeywords(raw []string, query string) []string {
	query = strings.TrimSpace(query)
	limit := deepRecallMaxKeywords
	if query != "" && limit > 1 {
		limit--
	}
	out := make([]string, 0, deepRecallMaxKeywords)
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || len(out) >= deepRecallMaxKeywords {
			return
		}
		if r := []rune(s); len(r) > 48 {
			s = string(r[:48])
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	for _, s := range raw {
		if len(out) >= limit {
			break
		}
		add(s)
	}
	if query != "" {
		add(query)
	}
	return out
}

func gatherDeepCandidates(ctx context.Context, repo Repository, vec VecQuerier, scope Scope, keywords []string, category string, since, until time.Time) ([]Entry, error) {
	raw, err := repo.Retrieve(ctx, Query{
		Scopes:   scopesFor(scope),
		Category: category,
		Since:    since,
		Until:    until,
		Limit:    searchCandidateLimit,
	})
	if err != nil {
		return nil, err
	}
	pool := FilterRecall(raw)
	byID := map[string]Entry{}
	for _, e := range pool {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}

	var lexIDs []string
	seenLex := map[string]struct{}{}
	for _, kw := range keywords {
		for _, p := range SelectRelevant(kw, pool, nil, deepRecallPerKeyword) {
			id := p.Entry.ID
			if id == "" {
				continue
			}
			if _, ok := seenLex[id]; ok {
				continue
			}
			seenLex[id] = struct{}{}
			lexIDs = append(lexIDs, id)
		}
	}

	var vecIDs []string
	seenVec := map[string]struct{}{}
	var missing []string
	if vec != nil && vec.Enabled() {
		for _, key := range vecScopeKeys(scope, pool) {
			for _, kw := range keywords {
				hits := vec.Search(key, kw, deepRecallPerKeyword)
				for _, h := range hits {
					if h.EntryID == "" || cosineFromDistance(h.Distance) < minCosine {
						continue
					}
					if _, ok := seenVec[h.EntryID]; ok {
						continue
					}
					seenVec[h.EntryID] = struct{}{}
					vecIDs = append(vecIDs, h.EntryID)
					if _, ok := byID[h.EntryID]; !ok {
						missing = append(missing, h.EntryID)
					}
				}
			}
		}
	}
	if len(missing) > 0 {
		fetcher, ok := repo.(EntryFetcher)
		if ok {
			got, ferr := fetcher.FetchEntries(ctx, missing)
			if ferr != nil {
				return nil, ferr
			}
			for _, e := range FilterRecall(got) {
				if e.ID == "" || !entryVisibleToScope(e, scope) {
					continue
				}
				if category != "" && e.Category != category {
					continue
				}
				if !since.IsZero() && e.CreatedAt.Before(since) {
					continue
				}
				if !until.IsZero() && e.CreatedAt.After(until) {
					continue
				}
				byID[e.ID] = e
			}
		}
	}

	var presentVec []string
	for _, id := range vecIDs {
		if _, ok := byID[id]; ok {
			presentVec = append(presentVec, id)
		}
	}
	fused := fuseRankings(lexIDs, presentVec)
	if len(fused) > deepRecallMaxCandidates {
		fused = fused[:deepRecallMaxCandidates]
	}
	out := make([]Entry, 0, len(fused))
	for _, id := range fused {
		if e, ok := byID[id]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func vecScopeKeys(scope Scope, pool []Entry) []string {
	if scope.Kind != "" && scope.Kind != ScopeAll {
		return []string{scope.Key()}
	}
	seen := map[string]struct{}{}
	var keys []string
	for _, e := range pool {
		k := e.Scope.Key()
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
		if len(keys) >= 8 {
			break
		}
	}
	return keys
}

func entryVisibleToScope(e Entry, scope Scope) bool {
	if scope.Kind == "" || scope.Kind == ScopeAll {
		return true
	}
	return e.Scope.Key() == scope.Key()
}

func vecQuerier(cfg DeepRecallConfig, repo Repository) VecQuerier {
	if cfg.Vec != nil {
		return cfg.Vec
	}
	type holder interface{ Vec() *VecIndex }
	h, ok := repo.(holder)
	if !ok || h == nil {
		return nil
	}
	idx := h.Vec()
	if idx == nil || !idx.Enabled() {
		return nil
	}
	return idx
}

// dropCoveredByRecent removes memories whose content is already stated in the
// recent visible window. The window is only those turns, not the long history
// or an older topic summary.
func dropCoveredByRecent(entries []Entry, window string) []Entry {
	window = strings.TrimSpace(window)
	if window == "" || len(entries) == 0 {
		return entries
	}
	wtok := relevanceTokens(window)
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if coveredByRecent(e.Content, window, wtok) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func coveredByRecent(content, window string, wtok map[string]struct{}) bool {
	c := strings.TrimSpace(content)
	if c == "" {
		return true
	}
	if len([]rune(c)) >= 8 && strings.Contains(window, c) {
		return true
	}
	ct := relevanceTokens(c)
	if len(ct) == 0 {
		return false
	}
	inter := 0
	for t := range ct {
		if _, ok := wtok[t]; ok {
			inter++
		}
	}
	return float64(inter)/float64(len(ct)) >= deepCoverageMin
}

func capDeepResults(entries []Entry, maxN, maxChars int) []EntryResult {
	if maxN <= 0 {
		maxN = deepRecallMaxResults
	}
	if maxChars <= 0 {
		maxChars = deepRecallMaxChars
	}
	if len(entries) > maxN {
		entries = entries[:maxN]
	}
	per := deepRecallEntryChars
	if per > maxChars {
		per = maxChars
	}
	formatted := entryResults(entries, per)
	total := 0
	out := make([]EntryResult, 0, len(formatted))
	for _, r := range formatted {
		n := len([]rune(r.Content))
		if len(out) > 0 && total+n > maxChars {
			break
		}
		if len(out) == 0 && n > maxChars {
			r.Content = truncateRunes(r.Content, maxChars)
			n = len([]rune(r.Content))
		}
		total += n
		out = append(out, r)
	}
	return out
}

// recentRecallView returns the coverage window (last n user/assistant turns)
// and the prompt shown to the keyword model. An older conversation summary is
// added to the prompt only when the recent window is too thin to show intent.
func recentRecallView(ctx context.Context, userRequest string, n int) (window, prompt string) {
	if n <= 0 {
		n = deepRecallRecentTurns
	}
	var msgs []llm.Message
	if lc := llm.LiveContextFromContext(ctx); lc != nil {
		msgs = lc.Snapshot()
	}
	type line struct{ role, text string }
	var lines []line
	var summary string
	for _, m := range msgs {
		text := strings.TrimSpace(llm.TextFromParts(m.Content))
		if text == "" {
			continue
		}
		if m.Role == llm.MessageRoleSystem && strings.Contains(text, llm.ConversationSummaryHeader) {
			summary = text
			continue
		}
		if m.Role != llm.MessageRoleUser && m.Role != llm.MessageRoleAssistant {
			continue
		}
		lines = append(lines, line{role: string(m.Role), text: text})
	}
	if ur := strings.TrimSpace(userRequest); ur != "" {
		// The live snapshot already holds this utterance when it is one of
		// the recent turns. Appending it again would push a real turn out
		// of the coverage window.
		tail := lines
		if len(tail) > n {
			tail = tail[len(tail)-n:]
		}
		seen := false
		for _, ln := range tail {
			if ln.text == ur {
				seen = true
				break
			}
		}
		if !seen {
			lines = append(lines, line{role: "user", text: ur})
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln.role)
		b.WriteString(": ")
		b.WriteString(truncateRunes(ln.text, 400))
		b.WriteByte('\n')
	}
	window = b.String()
	prompt = "Current request and recent turns:\n" + window
	// A single short turn does not show the topic. Two or more turns are
	// enough; the summary is older history and must not widen coverage.
	if summary != "" && strings.Count(window, "\n") < 2 && len([]rune(window)) < deepSummaryRunes {
		prompt = "Older topic summary (not part of the visible window):\n" +
			truncateRunes(summary, 240) + "\n\n" + prompt
	}
	return window, prompt
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func parseJSONObject(text string, dest any) error {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "```"); i >= 0 {
		text = text[i+3:]
		if strings.HasPrefix(strings.ToLower(text), "json") {
			text = text[4:]
		}
		if j := strings.Index(text, "```"); j >= 0 {
			text = text[:j]
		}
		text = strings.TrimSpace(text)
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return errors.New("no json object")
	}
	return json.Unmarshal([]byte(text[start:end+1]), dest)
}
