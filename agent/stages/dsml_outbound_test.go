package stages

import (
	"strings"
	"testing"
)

// 生产日志里真实外泄的两段 DSML（第二段参数闭合、第一段残缺），用作回归样本。
const (
	dsmlLeakTruncated = "<｜｜DSML｜｜ calls>\n" +
		"<｜｜DSML｜｜ invoke name=\"exec\">\n" +
		"<｜｜DSML｜｜ parameter name=\"command\" string=\"true\">cd \n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>"
	dsmlLeakFull = "<｜｜DSML｜｜ calls>\n" +
		"<｜｜DSML｜｜ invoke name=\"exec\">\n" +
		"<｜｜DSML｜｜ parameter name=\"command\" string=\"true\">cd &1</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>"
)

func TestCleanOutboundText_DSML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"full", dsmlLeakFull, ""},
		{"truncated", dsmlLeakTruncated, ""},
		{"keeps reply before block", "已经查完了。\n" + dsmlLeakFull, "已经查完了。"},
		{"plain reply untouched", "已经改好了，验证通过。", "已经改好了，验证通过。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CleanOutboundText(tc.in); got != tc.want {
				t.Fatalf("CleanOutboundText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// 出站最终回复（渠道发送 + 落库的 canonical）绝不能含 DSML。
func TestExtractPublicReply_DSML(t *testing.T) {
	for _, raw := range []string{dsmlLeakFull, dsmlLeakTruncated, "先说结论：已完成。\n" + dsmlLeakFull} {
		got := extractPublicReply(CleanOutboundText(raw))
		if strings.Contains(got, "DSML") || strings.Contains(got, "cd &1") {
			t.Fatalf("DSML leaked into public reply: %q", got)
		}
	}
	if got := explicitPublicReply("<public>" + dsmlLeakFull + "</public>"); got != "" {
		t.Fatalf("DSML inside <public> block must be stripped, got %q", got)
	}
	// 正常回复不受影响。
	if got := extractPublicReply(CleanOutboundText("已经改好了。")); got != "已经改好了。" {
		t.Fatalf("normal reply changed: %q", got)
	}
}

func TestLooksLikeInternalThinking_DSML(t *testing.T) {
	if !looksLikeInternalThinking(dsmlLeakFull) {
		t.Fatal("DSML must be a fail-closed signal on the bare-text path")
	}
	if looksLikeInternalThinking("DSML 是内部格式，不是协议") {
		t.Fatal("bare DSML wording must not be treated as internal thinking")
	}
}

// 流式逐字节喂入：模拟 SSE chunk 从多字节全角竖线中间切开。
// 两个 replyTags 模式都必须拦截，因为清理与回复控制门控开关无关。
func TestOutputCleanStreamFilter_DSML(t *testing.T) {
	for _, replyTags := range []bool{false, true} {
		for _, raw := range []string{dsmlLeakFull, dsmlLeakTruncated} {
			f := NewOutputCleanStreamFilter(replyTags)
			var got strings.Builder
			for i := 0; i < len(raw); i++ {
				got.WriteString(f.Feed(raw[i : i+1]))
			}
			got.WriteString(f.Flush())
			if out := got.String(); strings.Contains(out, "DSML") || strings.Contains(out, "cd &1") {
				t.Fatalf("replyTags=%v: DSML leaked into stream: %q", replyTags, out)
			}
		}
	}
}

// 流式前缀之前的正文要保留，且流式拼接 == 出站 payload（三处一致）。
func TestOutputCleanStreamFilter_DSMLParity(t *testing.T) {
	raw := "已经查完了。\n" + dsmlLeakFull
	want := CleanOutboundText(raw)
	for _, replyTags := range []bool{false, true} {
		f := NewOutputCleanStreamFilter(replyTags)
		var got strings.Builder
		for i := 0; i < len(raw); i++ {
			got.WriteString(f.Feed(raw[i : i+1]))
		}
		got.WriteString(f.Flush())
		if out := got.String(); out != want {
			t.Fatalf("replyTags=%v: stream %q != outbound %q", replyTags, out, want)
		}
	}
}

// 流结束时仍停在 "<" / "<|" 上的普通文本必须发出。这些是 DSML 前缀的真前缀，
// 但还不是开标签；扣住后丢掉会让流式拼接短于最终出站正文。
func TestOutputCleanStreamFilter_TrailingAngleParity(t *testing.T) {
	samples := []string{
		"compare a < b, then stop <",
		"token ends <|",
		"a < b",
	}
	for _, raw := range samples {
		want := CleanOutboundText(raw)
		for _, replyTags := range []bool{false, true} {
			f := NewOutputCleanStreamFilter(replyTags)
			var got strings.Builder
			for i := 0; i < len(raw); i++ {
				got.WriteString(f.Feed(raw[i : i+1]))
			}
			got.WriteString(f.Flush())
			if out := got.String(); out != want {
				t.Fatalf("replyTags=%v raw %q: stream %q != outbound %q", replyTags, raw, out, want)
			}
		}
	}
}

// DSML 混在 <public> 块里：标签被剥离，块内 DSML 之前的正文仍可发出。
func TestCleanPublicBlock_DSML(t *testing.T) {
	if got := cleanPublicBlock(dsmlLeakFull); got != "" {
		t.Fatalf("cleanPublicBlock must strip DSML, got %q", got)
	}
	if got := cleanPublicBlock("结论没问题。" + dsmlLeakFull); got != "结论没问题。" {
		t.Fatalf("cleanPublicBlock = %q", got)
	}
	// 普通技术讨论不受影响。
	plain := "DSML 只是模型内部写法，不是传输协议。"
	if got := cleanPublicBlock(plain); got != plain {
		t.Fatalf("plain text changed: %q", got)
	}
}
