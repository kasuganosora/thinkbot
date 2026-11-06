package memory

import "testing"

// 生产日志里真实外泄的两段 DSML 文本（第二段参数未闭合，第一段连 parameter
// 闭标签都缺）。用作回归样本，确保残缺块同样被拒收。
const (
	dsmlSampleTruncated = "<｜｜DSML｜｜ calls>\n" +
		"<｜｜DSML｜｜ invoke name=\"exec\">\n" +
		"<｜｜DSML｜｜ parameter name=\"command\" string=\"true\">cd \n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>"
	dsmlSampleFull = "<｜｜DSML｜｜ calls>\n" +
		"<｜｜DSML｜｜ invoke name=\"exec\">\n" +
		"<｜｜DSML｜｜ parameter name=\"command\" string=\"true\">cd &1</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>"
)

func TestStripDSMLBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"full block", dsmlSampleFull, ""},
		{"truncated block", dsmlSampleTruncated, ""},
		{"keeps text before block", "已经查完了。\n" + dsmlSampleFull, "已经查完了。\n"},
		{"discards everything after block", dsmlSampleFull + "\n这就是结论", ""},
		{"halfwidth pipes", "<||DSML|| calls>\n<||DSML|| invoke name=\"exec\">\ncd &1\n</||DSML|| calls>", ""},
		{"lowercase token", "<｜｜dsml｜｜ calls>\ncd &1\n</｜｜dsml｜｜ calls>", ""},
		// 只出现 DSML 字样、没有开标签前缀：绝不能误伤正常技术讨论。
		{"bare word is not a block", "DSML 是模型内部格式，不是传输协议。", "DSML 是模型内部格式，不是传输协议。"},
		{"mentions tag without prefix", "用户问 |DSML| 标签是什么", "用户问 |DSML| 标签是什么"},
		{"plain reply", "已经改好了，验证通过。", "已经改好了，验证通过。"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripDSMLBlocks(tc.in); got != tc.want {
				t.Fatalf("StripDSMLBlocks() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDSMLPrefixIndex(t *testing.T) {
	if got := DSMLPrefixIndex("ok\n" + dsmlSampleFull); got != 3 {
		t.Fatalf("prefix index = %d, want 3", got)
	}
	if got := DSMLPrefixIndex("no DSML here"); got != -1 {
		t.Fatalf("prefix index = %d, want -1", got)
	}
	if !HasDSMLMark(dsmlSampleTruncated) {
		t.Fatal("truncated sample must be detected")
	}
}

func TestDSMLOpenPrefixAt(t *testing.T) {
	const prefix = "<｜｜DSML｜｜"
	cases := []struct {
		in         string
		match      bool
		incomplete bool
		n          int
	}{
		{prefix + " calls>", true, false, len(prefix)},
		{prefix, true, false, len(prefix)},
		{"<｜｜DS", false, true, 0},
		{"<｜", false, true, 0},
		{"<", false, true, 0},
		{"<｜｜dsml｜｜ calls>", true, false, len(prefix)},
		{"<||DSML|| calls>", true, false, len("<||DSML||")},
		{"<public>", false, false, 0},
		{"</｜｜DSML｜｜ calls>", false, false, 0}, // 闭标签不触发开块
	}
	for _, tc := range cases {
		n, match, incomplete := DSMLOpenPrefixAt(tc.in)
		if match != tc.match || incomplete != tc.incomplete || n != tc.n {
			t.Errorf("DSMLOpenPrefixAt(%q) = (%d,%v,%v), want (%d,%v,%v)",
				tc.in, n, match, incomplete, tc.n, tc.match, tc.incomplete)
		}
	}
}

// 前缀逐字节喂入：模拟流式 chunk 从多字节全角竖线中间切开，
// 每一步都必须报告 incomplete，直到前缀齐备才 match。
func TestDSMLOpenPrefixAt_AcrossChunks(t *testing.T) {
	full := "<｜｜DSML｜｜ calls>"
	for i := 1; i < len("<｜｜DSML｜｜"); i++ {
		n, match, incomplete := DSMLOpenPrefixAt(full[:i])
		if match || !incomplete || n != 0 {
			t.Fatalf("split at %d: got (%d,%v,%v), want incomplete", i, n, match, incomplete)
		}
	}
	if n, match, incomplete := DSMLOpenPrefixAt(full); !match || incomplete || n != len("<｜｜DSML｜｜") {
		t.Fatalf("full prefix: got (%d,%v,%v), want match", n, match, incomplete)
	}
}
