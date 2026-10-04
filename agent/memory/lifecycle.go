package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"gorm.io/gorm"
)

// Memory status. Empty metadata means active, so existing rows stay current.
const (
	StatusActive     = "active"
	StatusPending    = "pending"
	StatusSuperseded = "superseded"

	metaStatus       = "status"
	metaSupersededBy = "superseded_by"
	metaSupersededAt = "superseded_at"
	metaInferred     = "inferred"
)

const maxTombstonesPerScope = 500

// StatusOf reports the recall status of an entry. Missing metadata is active.
func StatusOf(e Entry) string {
	if e.Metadata == nil {
		return StatusActive
	}
	s, _ := e.Metadata[metaStatus].(string)
	switch s {
	case StatusPending, StatusSuperseded:
		return s
	default:
		return StatusActive
	}
}

func setMeta(e *Entry, key string, val any) {
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	e.Metadata[key] = val
}

// MarkInferred parks a system-guessed memory until something confirms it.
// User-stated facts must not go through this.
func MarkInferred(e *Entry) {
	setMeta(e, metaInferred, true)
	setMeta(e, metaStatus, StatusPending)
}

// ConfirmEntry marks a pending entry as a current fact. The inferred flag stays
// so the origin is still explainable.
func ConfirmEntry(e *Entry) {
	setMeta(e, metaStatus, StatusActive)
}

// MarkSuperseded keeps the old statement but stops injecting it as current.
func MarkSuperseded(e *Entry, byID string, at time.Time) {
	setMeta(e, metaStatus, StatusSuperseded)
	setMeta(e, metaSupersededBy, byID)
	setMeta(e, metaSupersededAt, at.UTC().Format(time.RFC3339))
}

// RecallVisible reports whether normal recall may inject the entry.
// Pending guesses and superseded history stay stored and explainable.
func RecallVisible(e Entry) bool {
	if e.Category == suppressCategory {
		return false
	}
	switch StatusOf(e) {
	case StatusPending, StatusSuperseded:
		return false
	default:
		return true
	}
}

// FilterRecall drops entries that must not be injected as current facts.
func FilterRecall(entries []Entry) []Entry {
	if len(entries) == 0 {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if RecallVisible(e) {
			out = append(out, e)
		}
	}
	return out
}

// MemoryFingerprint hashes a normalized statement. Tombstones store this, not
// a second copy of the deleted text.
func MemoryFingerprint(content string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(content) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func tombstoneTopic(content string) string {
	if t := BanTopic(content); t != "" {
		return t
	}
	r := []rune(strings.TrimSpace(content))
	if len(r) > 24 {
		r = r[:24]
	}
	return string(r)
}

// Tombstone is a rejection of one statement in one scope.
type Tombstone struct {
	ScopeKey    string
	Topic       string
	Fingerprint string
	Source      string
	CreatedAt   time.Time
}

// tombstoneBook is the in-memory view of memory_tombstones.
// A nil db is enough for tests. When a db is set, lookups also hit the table
// so a delete through another store on the same database is visible.
type tombstoneBook struct {
	mu      sync.Mutex
	db      *gorm.DB
	byScope map[string][]Tombstone
}

func newTombstoneBook(db *gorm.DB) *tombstoneBook {
	b := &tombstoneBook{db: db, byScope: map[string][]Tombstone{}}
	if db == nil {
		return b
	}
	if err := db.AutoMigrate(&dao.MemoryTombstoneModel{}); err != nil {
		return b
	}
	var rows []dao.MemoryTombstoneModel
	if err := db.Order("created_at ASC").Find(&rows).Error; err != nil {
		return b
	}
	for _, row := range rows {
		b.byScope[row.ScopeKey] = append(b.byScope[row.ScopeKey], Tombstone{
			ScopeKey:    row.ScopeKey,
			Topic:       row.Topic,
			Fingerprint: row.Fingerprint,
			Source:      row.Source,
			CreatedAt:   row.CreatedAt,
		})
	}
	return b
}

func (b *tombstoneBook) add(scope Scope, content, source string) {
	if b == nil {
		return
	}
	fp := MemoryFingerprint(content)
	if fp == "" {
		return
	}
	t := Tombstone{
		ScopeKey:    scope.Key(),
		Topic:       tombstoneTopic(content),
		Fingerprint: fp,
		Source:      source,
		CreatedAt:   time.Now().UTC(),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.byScope[t.ScopeKey]
	for _, old := range list {
		if old.Fingerprint == fp {
			return
		}
	}
	list = append(list, t)
	if len(list) > maxTombstonesPerScope {
		list = list[len(list)-maxTombstonesPerScope:]
	}
	b.byScope[t.ScopeKey] = list
	if b.db == nil {
		return
	}
	row := dao.MemoryTombstoneModel{
		ID:          idgen.New("tomb"),
		ScopeKey:    t.ScopeKey,
		Fingerprint: t.Fingerprint,
		Topic:       t.Topic,
		Source:      t.Source,
		CreatedAt:   t.CreatedAt,
	}
	_ = b.db.Create(&row).Error
	var n int64
	_ = b.db.Model(&dao.MemoryTombstoneModel{}).Where("scope_key = ?", t.ScopeKey).Count(&n).Error
	if n > maxTombstonesPerScope {
		var oldest dao.MemoryTombstoneModel
		if err := b.db.Where("scope_key = ?", t.ScopeKey).Order("created_at ASC").First(&oldest).Error; err == nil {
			_ = b.db.Delete(&dao.MemoryTombstoneModel{}, "id = ?", oldest.ID).Error
		}
	}
}

func (b *tombstoneBook) has(scopeKey, fingerprint string) bool {
	if b == nil || fingerprint == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.byScope[scopeKey] {
		if t.Fingerprint == fingerprint {
			return true
		}
	}
	if b.db == nil {
		return false
	}
	var n int64
	err := b.db.Model(&dao.MemoryTombstoneModel{}).
		Where("scope_key = ? AND fingerprint = ?", scopeKey, fingerprint).
		Count(&n).Error
	return err == nil && n > 0
}

// RecordExternalTombstone writes a rejection from a store that is not the
// tiered book (memory_entries deletes). The tiered store sees it on the next
// lookup because both share the table.
func RecordExternalTombstone(db *gorm.DB, scope Scope, content, source string) {
	if db == nil {
		return
	}
	book := newTombstoneBook(nil)
	book.db = db
	_ = db.AutoMigrate(&dao.MemoryTombstoneModel{})
	book.add(scope, content, source)
}

// Forgotten reports whether this statement was deleted in the scope.
func (s *TieredStore) Forgotten(scope Scope, content string) bool {
	if s == nil || s.tomb == nil {
		return false
	}
	return s.tomb.has(scope.Key(), MemoryFingerprint(content))
}

// BlocksNewFact is true when writing the statement as a current L1 fact would
// undo a delete or revive a contradicted belief. A different statement is allowed,
// so a superseding fact is not blocked by the tombstone of the old one.
func (s *TieredStore) BlocksNewFact(scope Scope, content string) bool {
	if s.Forgotten(scope, content) {
		return true
	}
	fp := MemoryFingerprint(content)
	if fp == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.buckets[tierScopeKey(Tier1LongTerm, scope)] {
		if StatusOf(e.Entry) == StatusSuperseded && MemoryFingerprint(e.Content) == fp {
			return true
		}
	}
	return false
}

// Supersede keeps the old L1 row as history and stores the replacement as the
// current fact. It does not tombstone the old text.
func (s *TieredStore) Supersede(ctx context.Context, scope Scope, oldID string, replacement TieredEntry) error {
	if replacement.ID == "" {
		replacement.ID = idgen.New("mem")
	}
	replacement.Tier = Tier1LongTerm
	replacement.Scope = scope
	s.mu.Lock()
	key := tierScopeKey(Tier1LongTerm, scope)
	bucket := s.buckets[key]
	found := -1
	for i := range bucket {
		if bucket[i].ID == oldID {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return errNotFound(oldID)
	}
	MarkSuperseded(&bucket[found].Entry, replacement.ID, time.Now())
	s.buckets[key] = bucket
	old := bucket[found]
	s.mu.Unlock()
	if s.db != nil {
		s.persistUpsert(ctx, old)
	}
	if s.Forgotten(scope, replacement.Content) || sameStatement(old.Content, replacement.Content) {
		return nil
	}
	return s.Append(ctx, replacement)
}

func sameStatement(a, b string) bool {
	fa, fb := MemoryFingerprint(a), MemoryFingerprint(b)
	return fa != "" && fa == fb
}

type notFoundError string

func (e notFoundError) Error() string { return "memory: entry not found: " + string(e) }

func errNotFound(id string) error { return notFoundError(id) }

// Confirm promotes a pending entry to active. Missing entries return false.
func (s *TieredStore) Confirm(ctx context.Context, tier MemoryTier, scope Scope, id string) (bool, error) {
	s.mu.Lock()
	key := tierScopeKey(tier, scope)
	bucket := s.buckets[key]
	found := -1
	for i := range bucket {
		if bucket[i].ID == id {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return false, nil
	}
	ConfirmEntry(&bucket[found].Entry)
	s.buckets[key] = bucket
	updated := bucket[found]
	s.mu.Unlock()
	if s.db != nil {
		s.persistUpsert(ctx, updated)
	}
	return true, nil
}

// Remove drops an entry without recording a tombstone.
// Profile refresh and expiry use this. A user delete uses Delete.
func (s *TieredStore) Remove(ctx context.Context, tier MemoryTier, scope Scope, entryID string) error {
	return s.remove(ctx, tier, scope, entryID, false)
}
