package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

func smartNotification() Notification {
	return Notification{
		Source: "maid/smartd", Level: LevelInfo,
		Title: "SMART Health on /dev/sda",
		Body: "host: maid\ndevice: /dev/sda [SAT]\nmodel/serial: WDC WD40EFRX-68N32N0, S/N:WD-WCC7K1234567\n" +
			"cache 65536KB, 7200RPM, buffer 512MIB",
		At: time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC),
	}
}

func TestBuildPlaceholders(t *testing.T) {
	p := BuildPlaceholders(smartNotification(), time.UTC)
	want := map[string]string{
		"device": "/dev/sda",
		"serial": "WD-WCC7K1234567",
		"host":   "maid",
		"source": "maid/smartd",
		"title":  "SMART Health on /dev/sda",
		"time":   "2026-09-28 07:00:00 UTC",
	}
	for k, v := range want {
		if p.Values[k] != v {
			t.Errorf("%s = %q, want %q", k, p.Values[k], v)
		}
	}
	if !strings.HasPrefix(p.Values["raw"], "host: maid") {
		t.Errorf("raw = %q", p.Values["raw"])
	}
	// Indexed ids cover every extracted identifier, and quantities are not identifiers.
	ids := map[string]bool{}
	for k, v := range p.Values {
		if strings.HasPrefix(k, "id") {
			ids[v] = true
		}
	}
	for _, v := range []string{"maid", "/dev/sda", "WD40EFRX-68N32N0", "WD-WCC7K1234567"} {
		if !ids[v] {
			t.Errorf("identifier %q has no {{idN}}: %v", v, p.Values)
		}
	}
	for _, v := range []string{"65536KB", "7200RPM", "512MIB"} {
		if ids[v] {
			t.Errorf("quantity %q must not be an identifier", v)
		}
	}
	pm := p.PromptMap()
	if pm["{{device}}"] != "/dev/sda" || strings.Contains(pm["{{raw}}"], "host: maid") {
		t.Errorf("prompt map = %v", pm)
	}
}

func TestFillPlaceholders(t *testing.T) {
	p := BuildPlaceholders(smartNotification(), time.UTC)

	r := FillPlaceholders("Disk {{device}} ({{ SERIAL }}) on {{host}} is fine.", p)
	if r.Text != "Disk /dev/sda (WD-WCC7K1234567) on maid is fine." || r.Malformed || len(r.Unknown) != 0 {
		t.Fatalf("%+v", r)
	}
	if strings.Join(r.Used, ",") != "device,serial,host" {
		t.Fatalf("used = %v", r.Used)
	}

	r = FillPlaceholders("Disk {{disk}} is fine.", p)
	if len(r.Unknown) != 1 || r.Unknown[0] != "disk" || !strings.Contains(r.Text, placeholderUnresolved) {
		t.Fatalf("unknown placeholder: %+v", r)
	}

	r = FillPlaceholders("Disk {{device} is fine.", p)
	if !r.Malformed {
		t.Fatalf("unbalanced braces must be malformed: %+v", r)
	}

	r = FillPlaceholders("Disk {device} is fine.", p)
	if !r.Malformed || r.Text != "Disk /dev/sda is fine." {
		t.Fatalf("single-brace placeholder: %+v", r)
	}

	// Plain braces that are not placeholders are left alone.
	r = FillPlaceholders(`config {"a": 1} ok`, p)
	if r.Malformed || r.Text != `config {"a": 1} ok` {
		t.Fatalf("%+v", r)
	}

	// Substituted external text is not re-parsed.
	n := smartNotification()
	n.Title = "weird {{device}} title"
	r = FillPlaceholders("{{title}}", BuildPlaceholders(n, time.UTC))
	if r.Text != "weird {{device}} title" {
		t.Fatalf("substitution must be single-pass: %q", r.Text)
	}
}

// The 2026-09-26 false positive: "65536KB" was treated as a serial the bot
// had to copy, so a correct info message still got the raw block.
func TestComposeBot_NumberWithUnitIsNotAMissingIdentifier(t *testing.T) {
	n := smartNotification()
	text, rep := ComposeBot("Ojou-sama, {{device}} ({{id3}}, S/N {{serial}}) on {{host}} is healthy; the cache is 64 MB.", n, time.UTC)
	if rep.NeedsRaw() {
		t.Fatalf("no raw block expected: %+v\n%s", rep, text)
	}
	if strings.Contains(text, "原始通知") || !strings.Contains(text, "/dev/sda") || !strings.Contains(text, "WD-WCC7K1234567") {
		t.Fatalf("%s", text)
	}
	if !strings.HasSuffix(text, "\n\n— maid/smartd · SMART Health on /dev/sda") {
		t.Fatalf("info should end with the footer:\n%s", text)
	}
}

func TestComposeBot_BadPlaceholdersKeepRawFallback(t *testing.T) {
	n := smartNotification()
	for _, bot := range []string{
		"Ojou-sama, {{disk}} on {{host}} is healthy.",            // unknown
		"Ojou-sama, {{device} on {{host}} is healthy.",           // malformed
		"Ojou-sama, the disk on the server is healthy.",          // identifiers missing
		"Ojou-sama, /dev/sdb (WD-WCC7K1234567) on maid is fine.", // mistyped
	} {
		text, rep := ComposeBot(bot, n, time.UTC)
		if !rep.NeedsRaw() || !strings.Contains(text, "—— 原始通知 ——") || !strings.Contains(text, "S/N:WD-WCC7K1234567") {
			t.Errorf("%q: raw block expected (%+v):\n%s", bot, rep, text)
		}
	}
}

func TestComposeBot_CriticalWithRawPlaceholderDoesNotDuplicateBody(t *testing.T) {
	n := smartNotification()
	n.Level = LevelCritical
	text, rep := ComposeBot("Ojou-sama, urgent! {{device}} on {{host}}:\n{{raw}}", n, time.UTC)
	if rep.NeedsRaw() {
		t.Fatalf("%+v", rep)
	}
	if strings.Count(text, "S/N:WD-WCC7K1234567") != 1 || strings.Contains(text, "原始告警") {
		t.Fatalf("body must appear exactly once:\n%s", text)
	}
	if !strings.Contains(text, "🔴 CRITICAL · maid/smartd") {
		t.Fatalf("critical footer missing:\n%s", text)
	}
	// Without {{raw}} critical still always gets the raw block.
	text, _ = ComposeBot("Ojou-sama, urgent! {{device}} on {{host}}.", n, time.UTC)
	if !strings.Contains(text, "—— 原始告警 ——") {
		t.Fatalf("%s", text)
	}
}

// End to end through the service: the model writes placeholders, the owner
// receives the exact device name, and the history keeps the filled text.
func TestNotifyBotPlaceholdersFilledEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.cfg.DefaultMode = ModeBot
	h.prov.text = "Ojou-sama, mdadm sent a test for {{device}} on {{host}}; {{id3}} is [UU]."
	req := mdTestReq("info")
	res := h.svc.Notify(context.Background(), "bot-a", caller, req)
	if !res.Delivered || !res.BotUsed {
		t.Fatalf("%+v", res)
	}
	got := h.del.sent[0].text
	want := "Ojou-sama, mdadm sent a test for /dev/md/md-test on maid; md127 is [UU].\n\n— maid/mdadm · " + req.Title
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	user := msgText(h.prov.params[0].Messages[len(h.prov.params[0].Messages)-1])
	if !strings.Contains(user, `"placeholders"`) || !strings.Contains(user, `"{{device}}":"/dev/md/md-test"`) {
		t.Fatalf("prompt must carry the placeholder table:\n%s", user)
	}
}
