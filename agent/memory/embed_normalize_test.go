package memory

import "testing"

func TestNormalizeForEmbedStripsBoilerplate(t *testing.T) {
	in := "[Reply to @alice@example.social] 2026-03-01 12:30:05.123+08:00 note 9h6zahxdlu status 112233445566778899 see https://example.social/notes/9h6zahxdlu   ok"
	got := normalizeForEmbed(in)
	if got != "note status see ok" {
		t.Fatalf("normalized = %q", got)
	}
	if normalizeForEmbed("@bob 2026-03-01") != "@bob 2026-03-01" {
		t.Fatalf("empty normalization must fall back to the original text")
	}
}

func TestEmbedSimilarityIgnoresSharedBoilerplate(t *testing.T) {
	a := "[Reply to @alice@example.social] 9h6zahxdlu 2026-03-01 12:30:05+08:00 https://example.social/notes/9h6zahxdlu 今天在公园里看到了一只橘猫在晒太阳"
	b := "[Reply to @alice@example.social] 9h6zb1k2mq 2026-03-01 12:31:44+08:00 https://example.social/notes/9h6zb1k2mq Rust compiler release adds new borrow checker diagnostics"
	raw := cosineRaw(a, b)
	got := EmbedSimilarity(a, b)
	if got > 0.2 {
		t.Fatalf("unrelated entries with shared boilerplate: sim=%.3f (raw %.3f), want <= 0.2", got, raw)
	}
	t.Logf("unrelated: raw=%.3f normalized=%.3f", raw, got)
	if raw-got < 0.3 {
		t.Fatalf("normalization should cut similarity a lot: raw=%.3f norm=%.3f", raw, got)
	}

	c := "[Reply to @bob@example.net] 2026-03-02 08:00 今天在公园里看到了一只橘猫在晒太阳 https://example.net/@bob/112233445566778899"
	d := "@carol 2026-03-05 21:14:09 今天在公园里看到一只橘猫在晒太阳"
	s := EmbedSimilarity(c, d)
	t.Logf("near-duplicate: raw=%.3f normalized=%.3f", cosineRaw(c, d), s)
	if s < 0.85 {
		t.Fatalf("near-duplicates should stay high: sim=%.3f", s)
	}
}

// cosineRaw is the old scheme (no normalization), for comparison.
func cosineRaw(a, b string) float64 {
	return dotVec(rawHashVec(a), rawHashVec(b))
}

func TestNormalizeForEmbedQuoteAndAuthor(t *testing.T) {
	cases := map[string]string{
		"[Reply to Alice: @alice (quoted) text, with ) and https://example.org/x]\n今天的晚饭是咖喱": "今天的晚饭是咖喱",
		"[Reply to Alice: @alice line one\n@bob line two]\nsecond line body":                 "second line body",
		"@alice（Alice Example）：今天下雨了":                                                        "今天下雨了",
		"@alice@example.social: 今天下雨了":                                                       "今天下雨了",
		"@alice 今天下雨了":                                                                       "今天下雨了",
		"觉得不错 回复@bob@example.net：同意":                                                         "觉得不错 同意",
		"【回复 Bob：原文】好的":                                                                      "好的",
	}
	for in, want := range cases {
		if got := normalizeForEmbed(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmbedSimilarityQuoteSegmentsIgnored(t *testing.T) {
	quote := "[Reply to Alice: @alice 这是一段很长的被引用原文，讲的是周末去海边露营要带的装备清单和天气预报]\n"
	a := quote + "我只带了帐篷"
	b := quote + "Rust 1.99 编译器发布"
	if s := EmbedSimilarity(a, b); s > 0.2 {
		t.Fatalf("replies to the same quote must not look alike: %.3f (raw %.3f)", s, cosineRaw(a, b))
	}
	c := "@alice（Alice）：周末去海边露营要带帐篷和睡袋"
	d := "@bob: 周末去海边露营要带帐篷和睡袋"
	if s := EmbedSimilarity(c, d); s < 0.95 {
		t.Fatalf("same text under different authors should match: %.3f", s)
	}
}
