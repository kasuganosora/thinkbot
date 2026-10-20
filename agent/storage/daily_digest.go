package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // day buckets must not depend on the container's /etc/localtime or zoneinfo

	"go.uber.org/zap"
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// DailyDigester — one digest entry per listen scope per local day
//
// Passive listen scopes (core.IsListenChannel, e.g. misskey:timeline) collect
// raw notes all day. Instead of rolling dedup (which snowballed: merged entries
// absorbing each other, chains 84 deep), once per day after local midnight the
// previous days' raw notes of the scope are turned into ONE digest entry
// (source/category daily_digest, bounded length) and the covered notes are
// archived (ids logged and kept in the digest metadata for recovery).
//
// One LLM call per scope per day; a day larger than MaxInputChars is split into
// chunks plus one combine call. Days without notes cost nothing. Progress is
// persisted in memory_digest_states, and the digest entry id is deterministic,
// so re-running a date never duplicates. During the day, recall injects recent
// raw notes and previous digests (both live in the scope) within the budget.
// ============================================================================

// DailyDigestSource is the source / category of daily digest entries.
const DailyDigestSource = "daily_digest"

// Defaults of the daily digest.
const (
	DefaultDigestMaxChars      = 1500
	DefaultDigestMaxInputChars = 24000
	DefaultDigestBacklogDays   = 7
)

// DailyDigestConfig configures a DailyDigester.
type DailyDigestConfig struct {
	Provider llm.Provider
	Model    *llm.Model
	// BotID: only this bot's notes (metadata bot_id) are digested; the state key
	// includes it, since several bots may listen to the same timeline.
	BotID string
	// Location: the day boundary (bot timezone; default Asia/Shanghai). It is
	// also the zone of created_at values stored without an offset.
	Location *time.Location
	// MaxChars bounds the digest text (default 1500).
	MaxChars int
	// MaxInputChars bounds one call's notes (default 24000); larger days are chunked.
	MaxInputChars int
	// BacklogDays: on the first run (no state) at most this many past days are
	// digested (default 7); older notes stay as they are.
	BacklogDays int
	MaxTokens   int
	Policy      *llm.InternalPolicy
}

// DailyDigester produces daily digests for listen scopes.
type DailyDigester struct {
	repo   *SQLiteRepository
	cfg    DailyDigestConfig
	logger *zap.SugaredLogger
	now    func() time.Time
}

// NewDailyDigester creates a digester.
func NewDailyDigester(repo *SQLiteRepository, cfg DailyDigestConfig, logger *zap.SugaredLogger) *DailyDigester {
	if cfg.Location == nil {
		cfg.Location = DigestLocation("")
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = DefaultDigestMaxChars
	}
	if cfg.MaxInputChars <= 0 {
		cfg.MaxInputChars = DefaultDigestMaxInputChars
	}
	if cfg.BacklogDays <= 0 {
		cfg.BacklogDays = DefaultDigestBacklogDays
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &DailyDigester{repo: repo, cfg: cfg, logger: logger.With("component", "daily_digest"), now: time.Now}
}

// Run digests every completed day of every listen scope until ctx ends,
// checking every interval (default 15 min).
func (d *DailyDigester) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d.RunDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunDue digests all completed days not digested yet, for every listen scope.
func (d *DailyDigester) RunDue(ctx context.Context) {
	for _, id := range core.ListenChannels() {
		if ctx.Err() != nil {
			return
		}
		if err := d.DigestScope(ctx, memory.ChannelScope(id)); err != nil {
			d.logger.Warnw("daily digest failed", "scope", "channel:"+id, "err", err)
		}
	}
}

func (d *DailyDigester) stateKey(scope memory.Scope) string { return d.cfg.BotID + "|" + scope.Key() }

// DigestScope digests the completed days of one scope.
func (d *DailyDigester) DigestScope(ctx context.Context, scope memory.Scope) error {
	loc := d.cfg.Location
	today := dayStart(d.now().In(loc))
	var st dao.MemoryDigestState
	_ = d.repo.db.WithContext(ctx).Where("scope_key = ?", d.stateKey(scope)).Limit(1).Find(&st).Error

	var from time.Time
	if st.LastDate != "" {
		last, err := time.ParseInLocation("2006-01-02", st.LastDate, loc)
		if err != nil {
			return err
		}
		from = last.AddDate(0, 0, 1)
	} else {
		from = today.AddDate(0, 0, -d.cfg.BacklogDays)
	}
	for day := from; day.Before(today); day = day.AddDate(0, 0, 1) {
		if err := d.digestDay(ctx, scope, day); err != nil {
			return err // state not advanced: retried next round
		}
		d.saveState(ctx, scope, day.Format("2006-01-02"))
	}
	return nil
}

func (d *DailyDigester) saveState(ctx context.Context, scope memory.Scope, date string) {
	_ = d.repo.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_date", "updated_at"}),
	}).Create(&dao.MemoryDigestState{ScopeKey: d.stateKey(scope), LastDate: date, UpdatedAt: time.Now()}).Error
}

// DefaultDigestTimezone is the digest day zone when none is configured.
const DefaultDigestTimezone = "Asia/Shanghai"

// DigestLocation loads name (default Asia/Shanghai). It never returns
// time.Local: containers often run with /etc/localtime=UTC regardless of TZ.
// If loading fails, Asia/Shanghai falls back to a fixed +08:00 zone and other
// names to the default.
func DigestLocation(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "local") {
		name = DefaultDigestTimezone
	}
	if loc, err := time.LoadLocation(name); err == nil && loc != time.Local {
		return loc
	}
	if name != DefaultDigestTimezone {
		return DigestLocation(DefaultDigestTimezone)
	}
	return time.FixedZone("CST", 8*3600)
}

// createdAtLayouts are the created_at spellings found in memory_entries:
// the driver's "2006-01-02 15:04:05.999999999-07:00", RFC3339 and zone-less.
var createdAtLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 -0700 MST",
}

var createdAtLocalLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseCreatedAt parses a stored created_at. Values without a zone are read
// in loc (the bot timezone), not UTC and not the process local zone.
func parseCreatedAt(raw string, loc *time.Location) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, l := range createdAtLayouts {
		if t, err := time.Parse(l, raw); err == nil {
			return t, true
		}
	}
	for _, l := range createdAtLocalLayouts {
		if t, err := time.ParseInLocation(l, raw, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// DayNotes returns the active raw notes of scope written by this bot on day
// (day boundaries in cfg.Location). created_at is stored as text in mixed
// zones/formats, so SQL only pre-selects by date prefix (±1 day) and the exact
// bucket is decided after parsing each value.
func (d *DailyDigester) DayNotes(ctx context.Context, scope memory.Scope, day time.Time) ([]memory.Entry, error) {
	loc := d.cfg.Location
	start := dayStart(day.In(loc))
	end := start.AddDate(0, 0, 1)
	lo := start.AddDate(0, 0, -1).Format("2006-01-02")
	hi := start.AddDate(0, 0, 2).Format("2006-01-02")
	q := d.repo.scopeQuery(ctx, scope).Model(&dao.EntryModel{}).
		Where("source = ?", "note").
		Where("substr(CAST(created_at AS TEXT), 1, 10) >= ? AND substr(CAST(created_at AS TEXT), 1, 10) < ?", lo, hi)
	if d.cfg.BotID != "" {
		q = q.Where("metadata_json LIKE ?", "%\"bot_id\":\""+d.cfg.BotID+"\"%")
	}
	var cands []struct {
		ID        string `gorm:"column:id"`
		CreatedAt string `gorm:"column:created_at_text"`
	}
	if err := q.Select("id, CAST(created_at AS TEXT) AS created_at_text").Scan(&cands).Error; err != nil {
		return nil, err
	}
	at := make(map[string]time.Time, len(cands))
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		t, ok := parseCreatedAt(c.CreatedAt, loc)
		if !ok || t.Before(start) || !t.Before(end) {
			continue
		}
		at[c.ID] = t
		ids = append(ids, c.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var models []dao.EntryModel
	for i := 0; i < len(ids); i += 500 {
		j := i + 500
		if j > len(ids) {
			j = len(ids)
		}
		var part []dao.EntryModel
		if err := d.repo.db.WithContext(ctx).Where("id IN ?", ids[i:j]).Find(&part).Error; err != nil {
			return nil, err
		}
		models = append(models, part...)
	}
	for i := range models {
		if t, ok := at[models[i].ID]; ok {
			models[i].CreatedAt = t
		}
	}
	sort.SliceStable(models, func(i, j int) bool {
		if !models[i].CreatedAt.Equal(models[j].CreatedAt) {
			return models[i].CreatedAt.Before(models[j].CreatedAt)
		}
		return models[i].ID < models[j].ID
	})
	return modelsToActiveEntries(models), nil
}

func digestID(botID string, scope memory.Scope, date string) string {
	return "digest-" + strings.NewReplacer(":", "_", "|", "_").Replace(botID+"-"+scope.Key()) + "-" + date
}

func (d *DailyDigester) digestDay(ctx context.Context, scope memory.Scope, day time.Time) error {
	date := day.Format("2006-01-02")
	id := digestID(d.cfg.BotID, scope, date)
	var exists int64
	d.repo.db.WithContext(ctx).Model(&dao.EntryModel{}).Where("id = ?", id).Count(&exists)
	if exists > 0 {
		return nil // already digested (idempotent)
	}
	notes, err := d.DayNotes(ctx, scope, day)
	if err != nil {
		return err
	}
	if len(notes) == 0 {
		return nil // no notes, no call
	}
	events, err := d.summarize(ctx, scope, date, notes)
	if err != nil {
		return err
	}
	content, meta := buildDigest(date, events, notes, d.cfg.MaxChars)
	entry := memory.Entry{
		ID:         id,
		Scope:      scope,
		Content:    content,
		Category:   DailyDigestSource,
		Source:     DailyDigestSource,
		Importance: 0.6,
		Metadata:   meta,
		CreatedAt:  day.Add(24*time.Hour - time.Second), // sorts at the end of its day
	}
	if err := d.repo.Append(ctx, entry); err != nil {
		return err
	}
	ids := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.ID
		d.repo.ArchiveByID(ctx, scope, n.ID)
	}
	d.logger.Infow("daily digest written",
		"scope", scope.Key(), "date", date, "digest_id", id,
		"source_count", len(ids), "source_ids", ids, "digest_chars", len([]rune(content)))
	return nil
}

// digestEvent is one item of a digest; SourceIDs trace it back to the notes.
type digestEvent struct {
	Text      string   `json:"text"`
	SourceIDs []string `json:"source_ids"`
}

// buildDigest renders the injected text (events only, no ids) and the
// structured metadata (per-event source ids, for traceback; not injected).
func buildDigest(date string, events []digestEvent, notes []memory.Entry, maxChars int) (string, map[string]any) {
	known := map[string]bool{}
	all := make([]string, len(notes))
	for i, n := range notes {
		known[n.ID] = true
		all[i] = n.ID
	}
	var sb strings.Builder
	header := fmt.Sprintf("[%s 时间线日报]", date)
	sb.WriteString(header)
	metaEvents := []map[string]any{}
	for _, ev := range events {
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			continue
		}
		line := "\n- " + text
		if len([]rune(sb.String()))+len([]rune(line)) > maxChars {
			break // bounded length: later (less important) events dropped
		}
		sb.WriteString(line)
		var ids []string
		for _, id := range ev.SourceIDs {
			if known[id] {
				ids = append(ids, id)
			}
		}
		metaEvents = append(metaEvents, map[string]any{"index": len(metaEvents), "source_ids": ids})
	}
	meta := map[string]any{
		"digest_date":  date,
		"source_count": len(all),
		"source_ids":   all,
		"events":       metaEvents,
		"digested_at":  time.Now(),
	}
	return sb.String(), meta
}

const digestSystemPrompt = `You write the daily digest of a social timeline the bot passively follows.
Input: the day's posts, each prefixed by its id. Output: the most notable events/topics of the day as a JSON object
{"events":[{"text":"...","source_ids":["id1","id2"]}]} ordered by importance.
- Each text is one short self-contained sentence; keep names, @handles, URLs, numbers verbatim when they matter.
- source_ids lists the ids of the posts the event is based on.
- Write in the language most posts use. Skip trivial chatter. Output the JSON only.`

func (d *DailyDigester) summarize(ctx context.Context, scope memory.Scope, date string, notes []memory.Entry) ([]digestEvent, error) {
	chunks := chunkNotes(notes, d.cfg.MaxInputChars)
	limit := fmt.Sprintf("\nTotal length of all texts at most %d characters.", d.cfg.MaxChars)
	if len(chunks) == 1 {
		return d.call(ctx, scope, date, "day", len(notes), renderNotes(chunks[0])+limit)
	}
	var partial []digestEvent
	for i, ch := range chunks {
		evs, err := d.call(ctx, scope, date, fmt.Sprintf("chunk%d", i+1), len(ch), renderNotes(ch)+limit)
		if err != nil {
			return nil, err
		}
		partial = append(partial, evs...)
	}
	// combine: events of all chunks → final list (ids carried through)
	var sb strings.Builder
	sb.WriteString("Partial digests of the same day. Merge them into the final digest, merging duplicate events and uniting their source_ids:\n")
	for _, ev := range partial {
		fmt.Fprintf(&sb, "- %s (source_ids: %s)\n", ev.Text, strings.Join(ev.SourceIDs, ","))
	}
	return d.call(ctx, scope, date, "combine", len(partial), sb.String()+limit)
}

func (d *DailyDigester) call(ctx context.Context, scope memory.Scope, date, phase string, n int, prompt string) ([]digestEvent, error) {
	params := llm.GenerateParams{
		Model:    d.cfg.Model,
		System:   digestSystemPrompt,
		Messages: []llm.Message{llm.UserMessage(prompt)},
	}
	mt := d.cfg.Policy.MaxTokens(llm.PurposeDailyDigest, d.cfg.MaxTokens, 8192)
	params.MaxTokens = &mt
	d.cfg.Policy.Apply(llm.PurposeDailyDigest, &params)
	d.logger.Infow("daily digest request", "scope", scope.Key(), "date", date, "phase", phase, "items", n,
		"system_tokens_est", llm.EstimateTokens(digestSystemPrompt), "input_tokens_est", llm.EstimateTokens(prompt))
	resp, err := d.cfg.Provider.DoGenerate(llm.WithStatsFeature(ctx, llm.PurposeDailyDigest), params)
	if err != nil {
		return nil, err
	}
	if resp.FinishReason == llm.FinishReasonLength {
		return nil, errors.New("daily digest: answer cut off by the output limit")
	}
	var out struct {
		Events []digestEvent `json:"events"`
	}
	if err := strutil.ExtractJSON(resp.Text, &out); err != nil {
		return nil, fmt.Errorf("daily digest: unparsable answer: %w", err)
	}
	return out.Events, nil
}

func renderNotes(notes []memory.Entry) string {
	var sb strings.Builder
	sb.WriteString("## Posts\n")
	for _, n := range notes {
		fmt.Fprintf(&sb, "[%s] %s\n", n.ID, memory.StripThinking(n.Content))
	}
	return sb.String()
}

// chunkNotes splits notes into chunks of at most maxChars (a single longer note
// is its own chunk).
func chunkNotes(notes []memory.Entry, maxChars int) [][]memory.Entry {
	var out [][]memory.Entry
	var cur []memory.Entry
	size := 0
	for _, n := range notes {
		l := len([]rune(n.Content)) + len(n.ID) + 4
		if len(cur) > 0 && size+l > maxChars {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, n)
		size += l
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
