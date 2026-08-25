package memory

import (
	"strings"
	"testing"
)

func TestCompactJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOK  bool
		wantSub string // 期望出现在结果中的子串
		notSub  string // 期望不出现在结果中的子串（如缩进/多余空白）
	}{
		{
			name:   "pretty json -> compact",
			in:     "{\n  \"a\": 1,\n  \"b\": 2\n}",
			wantOK: true,
			notSub: "  ",
		},
		{
			name:    "non-json stays",
			in:      "just some text with no json",
			wantOK:  false,
			wantSub: "just some text with no json",
		},
		{
			name:    "oversized array sampled with total",
			in:      `[` + strings.Repeat(`{"x":1},`, 60) + `{"x":1}]`,
			wantOK:  true,
			wantSub: `"_total":61`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := compactJSON(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, c.wantOK, got)
			}
			if c.wantSub != "" && !strings.Contains(got, c.wantSub) {
				t.Fatalf("result %q missing %q", got, c.wantSub)
			}
			if c.notSub != "" && strings.Contains(got, c.notSub) {
				t.Fatalf("result %q should not contain %q", got, c.notSub)
			}
		})
	}
}

func TestDedupeRepeatedLines(t *testing.T) {
	in := "line1\nretry...\nretry...\nretry...\nline2"
	got := dedupeRepeatedLines(in)
	if strings.Contains(got, "retry...\nretry...") {
		t.Fatalf("repeated lines not merged: %q", got)
	}
	if !strings.Contains(got, "重复行×") {
		t.Fatalf("missing collapsed marker: %q", got)
	}
	// 单行不变
	if got := dedupeRepeatedLines("solo"); got != "solo" {
		t.Fatalf("single line should be unchanged, got %q", got)
	}
}

func TestExtractMustKeep(t *testing.T) {
	in := "user 550e8400-e29b-41d4-a716-446655440000 ran /var/lib/app/run.sh and hit ERR_TIMEOUT_504 paying ¥12.50"
	got := extractMustKeep(in)
	must := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"/var/lib/app/run.sh",
		"ERR_TIMEOUT_504",
		"¥12.50",
	}
	for _, m := range must {
		found := false
		for _, g := range got {
			if g == m {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("must-keep %q not extracted from %q (got %v)", m, in, got)
		}
	}
	// 去重：重复 UUID 只出现一次
	dup := "id 550e8400-e29b-41d4-a716-446655440000 and 550e8400-e29b-41d4-a716-446655440000"
	if g := extractMustKeep(dup); count(g, "550e8400-e29b-41d4-a716-446655440000") != 1 {
		t.Fatalf("duplicate must-keep not deduplicated: %v", g)
	}
}

func count(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

func TestPreprocessContent_RollbackWhenUseless(t *testing.T) {
	// 已经很紧凑的短文本：压缩后 token 不应反增，应回退为原文
	in := "short note"
	got := preprocessContent(in, true)
	if got != in {
		t.Fatalf("compact text should roll back to original, got %q", got)
	}
}

func TestPreprocessContent_CompressesJSONAndKeepsFragile(t *testing.T) {
	// 构造足够大的 JSON，使去空白的结构收益覆盖 must-keep 开销，
	// 从而既压缩又附加 must-keep 段（net_mutation_gain 为正才附加）。
	line := "  \"field\": \"550e8400-e29b-41d4-a716-446655440000 long padding text that wastes tokens padding padding\",\n"
	in := "{\n" + strings.Repeat(line, 40) +
		"  \"path\": \"/Users/sion/Documents/thinkbot/agent/memory/compactor.go\",\n  \"error\": \"ERR_TIMEOUT_504\"\n}"
	got := preprocessContent(in, true)
	if strings.Contains(got, "\n  ") {
		t.Fatalf("JSON not compacted: %q", got)
	}
	if !strings.Contains(got, "550e8400-e29b-41d4-a716-446655440000") {
		t.Fatalf("must-keep UUID dropped: %q", got)
	}
	if !strings.Contains(got, "[保留项-摘要不得丢失]") {
		t.Fatalf("must-keep section missing: %q", got)
	}
}

func TestPreprocessBatch_RespectsToggle(t *testing.T) {
	entries := []TieredEntry{{Entry: Entry{Content: "{\n  \"a\": 1\n}"}}}
	on := &SemanticCompactor{config: CompactionConfig{Precompress: true}}
	off := &SemanticCompactor{config: CompactionConfig{Precompress: false}}
	if on.preprocessBatch(entries)[0].Content == entries[0].Content {
		t.Fatalf("expected compression when enabled")
	}
	if off.preprocessBatch(entries)[0].Content != entries[0].Content {
		t.Fatalf("expected no change when disabled")
	}
}
