package stages

import (
	"strings"
	"testing"
)

// feedAll 把 chunks 依次喂给过滤器并在结尾 Flush，返回拼接后的可见输出。
// 同时断言任何一次输出都不含分隔符的任何「可疑片段」。
func feedAll(t *testing.T, chunks []string) string {
	t.Helper()
	var f ReplyControlStreamFilter
	var b strings.Builder
	for _, c := range chunks {
		out := f.Feed(c)
		if strings.Contains(out, "REPLY_CONTROL") || strings.Contains(out, `"send"`) {
			t.Fatalf("leaked control marker in delta %q (chunks=%q)", out, chunks)
		}
		b.WriteString(out)
	}
	b.WriteString(f.Flush())
	return b.String()
}

// splitEvery 把 s 切成每段 n 字节（按 rune 边界修正）。
func splitEvery(s string, n int) []string {
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); i += n {
		j := i + n
		if j > len(r) {
			j = len(r)
		}
		out = append(out, string(r[i:j]))
	}
	return out
}

func TestReplyControlStreamFilter_SingleChunk(t *testing.T) {
	got := feedAll(t, []string{"你好，部署测试收到 ✅\n\n@@REPLY_CONTROL@@{\"send\": true}"})
	if got != "你好，部署测试收到 ✅" {
		t.Fatalf("got %q", got)
	}
}

// 核心回归：控制块被切成任意多段（包括分隔符本身被切开），都不能漏出。
func TestReplyControlStreamFilter_SplitAtEveryPosition(t *testing.T) {
	full := "部署测试收到，一切正常 ✅\n\n@@REPLY_CONTROL@@{\"send\": true}"
	want := "部署测试收到，一切正常 ✅"
	r := []rune(full)
	for i := 1; i < len(r); i++ {
		for j := i + 1; j <= len(r); j++ {
			chunks := []string{string(r[:i]), string(r[i:j]), string(r[j:])}
			if got := feedAll(t, chunks); got != want {
				t.Fatalf("split at %d/%d: got %q want %q", i, j, got, want)
			}
		}
	}
}

func TestReplyControlStreamFilter_TokenSizedChunks(t *testing.T) {
	full := "<public>早上好喵🧹</public>\n@@REPLY_CONTROL@@{\"send\": false}"
	for n := 1; n <= 8; n++ {
		if got := feedAll(t, splitEvery(full, n)); got != "<public>早上好喵🧹</public>" {
			t.Fatalf("n=%d got %q", n, got)
		}
	}
	// 真实 GLM 切法
	glm := []string{"好的", "\n\n", "@@", "REPLY", "_CONTROL", "@@", "{\"", "send", "\":", " true", "}"}
	if got := feedAll(t, glm); got != "好的" {
		t.Fatalf("glm-style got %q", got)
	}
}

// 看起来像分隔符开头、最终没凑齐：必须原样放行（不能吞正文）。
func TestReplyControlStreamFilter_FalsePrefixIsReleased(t *testing.T) {
	cases := [][]string{
		{"邮箱 a@@b.com", " 结束"},
		{"价格 @", "@RE", "PLY 不是标记"},
		{"尾巴是 @@REPLY_CON"}, // 流结束时仍未凑齐 → Flush 原样放行
		{"只有一个 @"},
	}
	for _, c := range cases {
		want := strings.Join(c, "")
		if got := feedAll(t, c); got != want {
			t.Fatalf("chunks %q: got %q want %q", c, got, want)
		}
	}
}

// 围栏包裹 / 多余空白 / JSON 字符串里含括号与转义：都要整体吞掉。
func TestReplyControlStreamFilter_FencedAndTrickyJSON(t *testing.T) {
	full := "正文\n@@REPLY_CONTROL@@\n```json\n{\"send\": true, \"note\": \"a}b\\\"{c\"}\n```\n"
	for n := 1; n <= 5; n++ {
		if got := feedAll(t, splitEvery(full, n)); got != "正文" {
			t.Fatalf("n=%d got %q", n, got)
		}
	}
}

// 控制块之后还有正文（多步输出的罕见情形）：控制块本身吞掉，后续正文放行。
func TestReplyControlStreamFilter_TextAfterControlBlock(t *testing.T) {
	full := "第一段\n@@REPLY_CONTROL@@{\"send\": true}\n第二段"
	for n := 1; n <= 6; n++ {
		if got := feedAll(t, splitEvery(full, n)); got != "第一段第二段" {
			t.Fatalf("n=%d got %q", n, got)
		}
	}
}

// 未闭合的控制块在 Flush 时丢弃，且 Flush 之后过滤器可复用。
func TestReplyControlStreamFilter_UnterminatedAndReuse(t *testing.T) {
	var f ReplyControlStreamFilter
	out := f.Feed("hi @@REPLY_CONTROL@@{\"send\": tr")
	if out != "hi" {
		t.Fatalf("got %q", out)
	}
	if rest := f.Flush(); rest != "" {
		t.Fatalf("unterminated control block leaked on flush: %q", rest)
	}
	if got := f.Feed("again"); got != "again" {
		t.Fatalf("filter not reset after flush: %q", got)
	}
}

// 普通文本（无标记）逐段输出时不应被无谓扣住（空白除外），保证流式体验。
func TestReplyControlStreamFilter_NoMarkerPassThrough(t *testing.T) {
	var f ReplyControlStreamFilter
	if got := f.Feed("hello"); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := f.Feed(" world"); got != " world" {
		t.Fatalf("got %q", got)
	}
	// 尾部空白暂扣，后续非空白到来时一起放行
	if got := f.Feed("!\n"); got != "!" {
		t.Fatalf("got %q", got)
	}
	if got := f.Feed("next"); got != "\nnext" {
		t.Fatalf("got %q", got)
	}
}

// 过滤后的流拼起来必须与最终 done 文本（StripReplyControlBlock(全文)）一致。
func TestReplyControlStreamFilter_MatchesFinalStrip(t *testing.T) {
	texts := []string{
		"普通回复\n\n@@REPLY_CONTROL@@{\"send\": true}",
		"<public>公开</public>\n@@REPLY_CONTROL@@{\"send\": false}",
		"没有控制块的回复",
	}
	for _, full := range texts {
		want := StripReplyControlBlock(full)
		for n := 1; n <= 4; n++ {
			if got := feedAll(t, splitEvery(full, n)); got != want {
				t.Fatalf("n=%d: stream %q != final %q", n, got, want)
			}
		}
	}
}
