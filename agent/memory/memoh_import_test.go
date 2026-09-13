package memory

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const memohFixtureDaily = `# Memory 2026-04-26

## Entry mem_keep_notes

` + "```yaml" + `
id: mem_keep_notes
created_at: "2026-04-26T08:23:18Z"
metadata:
    channel: misskey
    profile_ref: Luna
    topic: Notes
    type: group
` + "```" + `

大小姐在 Telegram 和 Misskey 上密集 debug @kanna 回复问题，确认根因是模型不调 send 工具，还教栞娜应该用 reply。

## Entry mem_heartbeat_idle

` + "```yaml" + `
id: mem_heartbeat_idle
created_at: "2026-05-25T13:55:53+08:00"
metadata:
    topic: Heartbeat Check 13:55 HKT
` + "```" + `

13:55 心跳检查——自12:55以来，大小姐無新消息，週一上班中。無人@栞娜，無消息。无事。

## Entry mem_idle_notes

` + "```yaml" + `
id: mem_idle_notes
created_at: "2026-05-06T13:53:00+08:00"
metadata:
    topic: Notes
` + "```" + `

13:53 从上次检查以来无新消息。大小姐午餐后无活动。无事。

## Entry mem_short

` + "```yaml" + `
id: mem_short
created_at: "2026-04-26T12:00:00Z"
metadata:
    topic: Notes
` + "```" + `

用户冷

## Entry mem_user_profile

` + "```yaml" + `
id: mem_user_profile
created_at: "2026-05-25T07:57:19Z"
metadata:
    type: user_profile
    profile_ref: Luna
` + "```" + `

User personality profile Traits: - communication_style: direct (strength: 0.9, evidence: 单句提问)
`

const memohFixtureProfiles = `## Luna (露娜大小姐)
- Name: Luna
- Channel: web
- Type: private (主人的专属)
- First contact: 2026-04-26
- Notes: 主人设定了栞娜的人物设定和核心规则。

## Sion Reitsuki (Telegram alias of Luna)
- Name: Sion Reitsuki
- Channel: telegram
- Target: 76017910
- Notes: 自称露娜大小姐，确认为主人。
`

func TestMemohIsJunk(t *testing.T) {
	cases := []struct {
		name string
		e    memohRawEntry
		junk bool
	}{
		{name: "heartbeat topic", e: memohRawEntry{Topic: "Heartbeat / Bug Monitor", Body: strings.Repeat("x", 40)}, junk: true},
		{name: "idle 无事", e: memohRawEntry{Topic: "Notes", Body: "从上次检查以来无新消息。大小姐午餐后无活动。无事。"}, junk: true},
		{name: "short", e: memohRawEntry{Topic: "Notes", Body: "用户冷"}, junk: true},
		{name: "user_profile", e: memohRawEntry{Type: "user_profile", Body: strings.Repeat("画像内容很长很长很长", 4)}, junk: true},
		{name: "profile prefix", e: memohRawEntry{Body: "User personality profile Traits: - foo bar baz qux"}, junk: true},
		{name: "useful notes", e: memohRawEntry{Topic: "Notes", Body: "大小姐在 Telegram 聊了俄罗斯旅游，并教栞娜用 reply 工具而不是 send。"}, junk: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := memohIsJunk(tc.e); got != tc.junk {
				t.Fatalf("junk=%v want %v body=%q topic=%q", got, tc.junk, tc.e.Body, tc.e.Topic)
			}
		})
	}
}

func TestImportMemohDir_FiltersAndScopes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", "2026-04-26.md"), []byte(memohFixtureDaily), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PROFILES.md"), []byte(memohFixtureProfiles), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("1. [profile] should be ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewTieredStore(nil)
	ctx := context.Background()
	rep, err := ImportMemohDir(ctx, store, "bot-kanna", dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SkippedJunk < 3 {
		t.Fatalf("expected >=3 junk skips, got %+v", rep)
	}
	if rep.Profiles != 1 {
		t.Fatalf("expected 1 merged profile, got %+v", rep)
	}
	if rep.Imported != 2 { // 1 L3 profile + 1 useful L1
		t.Fatalf("expected imported=2 (profile+notes), got %+v", rep)
	}

	uid := "76017910"
	l3, err := store.Retrieve(ctx, Tier3Profile, []Scope{UserScope(uid)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(l3) != 1 || !strings.Contains(l3[0].Content, "Luna") || !strings.Contains(l3[0].Content, "76017910") {
		t.Fatalf("L3 profile: %+v", l3)
	}
	l1, err := store.Retrieve(ctx, Tier1LongTerm, []Scope{UserScope(uid)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(l1) != 1 || !strings.Contains(l1[0].Content, "debug") {
		t.Fatalf("L1 notes: %+v", l1)
	}
	if l1[0].CreatedAt.Year() != 2026 || l1[0].CreatedAt.Month() != 4 {
		t.Fatalf("preserve created_at, got %v", l1[0].CreatedAt)
	}

	rep2, err := ImportMemohDir(ctx, store, "bot-kanna", dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Imported != 0 || rep2.Skipped < 2 {
		t.Fatalf("second import should skip existing, got %+v", rep2)
	}
}

func TestImportMemohArchive_GzipTar(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	add := func(name, body string) {
		t.Helper()
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("memory/2026-04-26.md", memohFixtureDaily)
	add("PROFILES.md", memohFixtureProfiles)
	add("MEMORY.md", "ignored")
	add("media/00/x.jpg", "not-a-memory")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	store := NewTieredStore(nil)
	rep, err := ImportMemohArchive(context.Background(), store, "bot-kanna", &gz)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 2 {
		t.Fatalf("gzip tar import: %+v", rep)
	}
}

func TestMemohEntryID_FitsDAO(t *testing.T) {
	id := memohEntryID("61f5f54c-0858-4b1f-95c0-546cfe264bee:mem_1777204945070820161")
	if len(id) > 64 {
		t.Fatalf("id too long: %s (%d)", id, len(id))
	}
	if !strings.HasPrefix(id, "memoh_") {
		t.Fatalf("prefix: %s", id)
	}
}

func TestImportMemoh_SampleArchive(t *testing.T) {
	const sample = `e:\ai\bot-kanna-61f5f54c-0858-4b1f-95c0-546cfe264bee-data-2026-06-14T06-18-56.559Z.tar.gz`
	f, err := os.Open(sample)
	if err != nil {
		t.Skip("sample archive not present")
	}
	defer f.Close()

	store := NewTieredStore(nil)
	rep, err := ImportMemohArchive(context.Background(), store, "bot-kanna", f)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SkippedJunk == 0 {
		t.Fatalf("sample should drop heartbeat/idle junk, got %+v", rep)
	}
	if rep.Imported == 0 {
		t.Fatalf("sample should keep some notes/profiles, got %+v", rep)
	}
	if rep.Profiles != 1 {
		t.Fatalf("Luna/Sion should merge to one L3, profiles=%d", rep.Profiles)
	}
	if rep.SkippedJunk <= rep.Imported {
		t.Logf("warning: junk (%d) not clearly majority vs imported (%d)", rep.SkippedJunk, rep.Imported)
	}
	l3, err := store.Retrieve(context.Background(), Tier3Profile, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sample import: imported=%d junk=%d skipped=%d profiles=%d l3=%d errors=%d",
		rep.Imported, rep.SkippedJunk, rep.Skipped, rep.Profiles, len(l3), len(rep.Errors))
	for _, e := range l3 {
		t.Logf("L3 %s %s: %s", e.Scope.Key(), e.ID, e.Content)
	}
}
