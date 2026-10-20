package storage

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestParseCreatedAtFormats(t *testing.T) {
	sh := DigestLocation("")
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"2026-10-18 23:30:00.123+08:00", time.Date(2026, 10, 18, 15, 30, 0, 123e6, time.UTC)},
		{"2026-10-18 22:22:41.258290031+08:00", time.Date(2026, 10, 18, 14, 22, 41, 258290031, time.UTC)},
		{"2026-10-18T15:30:00Z", time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)},
		{"2026-10-18 17:20:57", time.Date(2026, 10, 18, 9, 20, 57, 0, time.UTC)}, // zone-less = Asia/Shanghai
	}
	for _, c := range cases {
		got, ok := parseCreatedAt(c.raw, sh)
		if !ok || !got.Equal(c.want) {
			t.Fatalf("parse %q = %v (%v), want %v", c.raw, got, ok, c.want)
		}
	}
	if _, ok := parseCreatedAt("garbage", sh); ok {
		t.Fatal("garbage must not parse")
	}
}

func TestDigestLocationNeverLocal(t *testing.T) {
	for _, name := range []string{"", "Local", "No/Such_Zone", "Asia/Shanghai"} {
		loc := DigestLocation(name)
		if loc == time.Local {
			t.Fatalf("%q -> time.Local", name)
		}
		_, off := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Zone()
		if off != 8*3600 {
			t.Fatalf("%q -> offset %d", name, off)
		}
	}
}

// Rows stored in mixed spellings (with +08:00, UTC "Z", and zone-less) must be
// bucketed by the bot day, independent of the process local zone.
func TestDayNotesMixedCreatedAt(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC // like the container: /etc/localtime=UTC
	defer func() { time.Local = saved }()

	db := testDB(t)
	ctx := context.Background()
	scope := memory.ChannelScope("test:timeline")
	ins := func(id, createdAt string) {
		if err := db.Exec(`INSERT INTO memory_entries (id, scope_kind, scope_id, source, content, metadata_json, created_at)
			VALUES (?, 'channel', 'test:timeline', 'note', ?, '{"bot_id":"b1"}', ?)`, id, "note "+id, createdAt).Error; err != nil {
			t.Fatal(err)
		}
	}
	ins("a-early", "2026-10-18 00:10:00.5+08:00")  // 10-18 local
	ins("a-utc", "2026-10-17T16:30:00Z")           // = 10-18 00:30 local
	ins("a-plain", "2026-10-18 07:00:00")          // zone-less: 10-18 07:00 local
	ins("a-late", "2026-10-18 23:59:59.999+08:00") // 10-18 local
	ins("b-prev", "2026-10-17 23:59:59")           // zone-less, 10-17 local (UTC would say the same day)
	ins("b-plain-utc", "2026-10-17 20:00:00")      // zone-less: 10-17 local, not 10-18 04:00
	ins("c-next", "2026-10-18T16:00:00Z")          // = 10-19 00:00 local

	d := NewDailyDigester(NewSQLiteRepository(db), DailyDigestConfig{Model: &llm.Model{ID: "m"}, BotID: "b1"}, zap.NewNop().Sugar())
	day := time.Date(2026, 10, 18, 0, 0, 0, 0, d.cfg.Location)
	notes, err := d.DayNotes(ctx, scope, day)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range notes {
		got = append(got, n.ID)
	}
	want := []string{"a-early", "a-utc", "a-plain", "a-late"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
