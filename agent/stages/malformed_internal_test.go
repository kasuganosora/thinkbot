package stages

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// screenshotLeak 复现线上流式气泡泄漏的畸形标签形态：
// 开标签是 `<internal`（没有 `>`），闭标签是 `/internal`（没有 `</` 也没有 `>`），
// 公开回复紧挨在闭标签后面。私有独白与残缺标签都不得进入流式 delta 或最终出站。
const screenshotLeak = "<internal大小姐发话了，任务系统走起。老套路：先在 GameSystemD 里捞 quest 相关的字符串和符号，再顺着线索深挖，最后出报告归档到 research/mabinogi/。/internal好嘞，最后一块拼图——任务系统，栞那这就去扒～"

const screenshotPublic = "好嘞，最后一块拼图——任务系统，栞那这就去扒～"

func tagNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
}

func leakedMalformedPrivate(s string) bool {
	for _, p := range []string{"大小姐", "GameSystemD", "mabinogi", "老套路"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	lower := strings.ToLower(s)
	for _, tok := range []string{"<internal", "/internal"} {
		from := 0
		for from < len(lower) {
			i := strings.Index(lower[from:], tok)
			if i < 0 {
				break
			}
			i += from
			end := i + len(tok)
			// `<internalize` / `/internally` 是更长的名字，不算残缺私有标签。
			if end >= len(lower) || !tagNameByte(lower[end]) {
				return true
			}
			from = i + len(tok)
		}
	}
	return false
}

// TestStripInternalTags_LooseMalformed 锁定：从 `<internal`（可以没有 `>`）一直剥到
// `</internal>`、`/internal`（可以没有 `>`）或文本末尾。标签外的公开文字留下。
func TestStripInternalTags_LooseMalformed(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"screenshot shape", screenshotLeak, screenshotPublic},
		{"missing gt open, proper close", "<internal秘密</internal>可见", "可见"},
		{"missing gt open, slash close with gt", "<internal秘密/internal>可见", "可见"},
		{"missing gt open, slash close no gt", "<internal秘密/internal可见", "可见"},
		{"well-formed open, slash close no gt", "<internal>秘密/internal可见", "可见"},
		{"well-formed open, slash close with gt", "<internal>秘密/internal>可见", "可见"},
		{"uppercase loose pair", "<INTERNAL秘密/INTERNAL可见", "可见"},
		{"unclosed loose open to EOF", "前文<internal秘密没有收尾", "前文"},
		{"half-open tag only", "<internal", ""},
		{"well-formed pair still strips content only", "<internal>秘密</internal>可见", "可见"},
		{"longer tag name is not an internal open", "<internalize>hello</internalize>", "<internalize>hello</internalize>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := memory.StripInternalTags(c.input)
			if got != c.want {
				t.Fatalf("StripInternalTags(%q) = %q, want %q", c.input, got, c.want)
			}
			if leakedMalformedPrivate(got) {
				t.Fatalf("private residue in %q", got)
			}
		})
	}
}

// TestExtractPublicReply_LooseInternal 锁定最终出站（无完整 <public> 的 emit-all 路径，
// 以及包在 <public> 里 / 后面还有 <public> 的路径）。
func TestExtractPublicReply_LooseInternal(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"screenshot with no public tag fails closed", screenshotLeak, ""},
		{"loose open no close fails closed", "<internal这是不该发出去的内心独白，后面也没有公开标签。", ""},
		{"loose open proper close fails closed", "<internal秘密</internal>可见但不该整段发出", ""},
		{"well-formed internal still fails closed", "<internal>秘密</internal>可见但不该整段发出", ""},
		{"screenshot inside public keeps the reply", "<public>" + screenshotLeak + "</public>", screenshotPublic},
		{"loose internal then a real public block", screenshotLeak + "<public>另起一段。</public>", "另起一段。"},
		{"public nested inside loose internal is not extracted", "<internal秘密<public>不该出</public>/internal<public>该出</public>", "该出"},
		{"unclosed loose internal hides a nested public", "<internal秘密<public>不该出</public>", ""},
		{"stray /internal without gt", "前面/internal后面", "前面后面"},
		{"stray /public without gt", "答案/public接下来", "答案接下来"},
		{"stray /internal> still stripped", "前面/internal>后面", "前面后面"},
		{"stray /public> still stripped", "答案/public>接下来", "答案接下来"},
		{"do not strip /internally", "see /internally now", "see /internally now"},
		{"do not strip /publication", "see /publication now", "see /publication now"},
		{"public block keeps text around stray slash close", "<public>甲/internal乙/public丙</public>", "甲乙丙"},
		{"plain text mentioning the word internal", "我们聊聊 internal API 的设计", "我们聊聊 internal API 的设计"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sharedClean(c.input)
			if got != c.want {
				t.Fatalf("sharedClean = %q, want %q", got, c.want)
			}
			if leakedMalformedPrivate(got) {
				t.Fatalf("private residue in %q", got)
			}
		})
	}
}

// TestOutputCleanStream_MalformedInternalScreenshot 锁定 Web 流式：截图原句以及
// 把畸形开/闭标签切到不同 chunk 的情况，Feed/Flush 都不得吐出私有独白或残缺标签。
func TestOutputCleanStream_MalformedInternalScreenshot(t *testing.T) {
	raws := []string{
		screenshotLeak,
		"<public>" + screenshotLeak + "</public>",
		screenshotLeak + "<public>另起一段。</public>",
		"<internal秘密<public>不该出</public>/internal<public>该出</public>",
		"前面/internal后面",
		"答案/public接下来",
		"<public>甲/internal乙/public丙</public>",
		"<public>好的</public>这部分是我的判断 <internal 没闭合，不该出去",
	}
	crossChunk := []string{
		"<inter",
		"nal大小姐发话了，任务系统走起。老套路：先在 ",
		"GameSystemD 里捞 quest 相关的字符串和符号，再顺着线索深挖，最后出报告归档到 ",
		"research/mabinogi/。/inter",
		"nal好嘞，最后一块拼图——任务系统，栞那这就去扒～",
	}

	for _, raw := range raws {
		want := sharedClean(raw)
		chunkings := allChunkings(raw)
		if raw == screenshotLeak {
			chunkings = append(chunkings, crossChunk)
		}
		for _, chunks := range chunkings {
			got, outs := streamClean(chunks, true)
			if got != want {
				t.Fatalf("raw=%q\nchunks=%q\n got %q\nwant %q", raw, chunks, got, want)
			}
			for _, o := range outs {
				if leakedMalformedPrivate(o) {
					t.Fatalf("delta leaked %q (raw=%q chunks=%q)", o, raw, chunks)
				}
			}
		}
	}

	// 单次 Feed 就超过伪标签放行上限：缺 '>' 的 `<internal` 不能被当成普通文本吐出。
	f := NewOutputCleanStreamFilter(true)
	if got := f.Feed(screenshotLeak); leakedMalformedPrivate(got) || got != "" {
		t.Fatalf("feed leaked %q", got)
	}
	if got := f.Flush(); leakedMalformedPrivate(got) || got != "" {
		t.Fatalf("flush leaked %q", got)
	}

	// 开标签、闭标签各自被切开。
	g := NewOutputCleanStreamFilter(true)
	var streamed string
	for _, c := range crossChunk {
		streamed += g.Feed(c)
	}
	streamed += g.Flush()
	if streamed != "" || leakedMalformedPrivate(streamed) {
		t.Fatalf("cross-chunk leaked %q", streamed)
	}
}
