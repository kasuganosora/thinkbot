package memory

import "testing"

// TestStripInternalState_StripsMarkers 锁定内部指标仍会被剥离，且删除点两侧的空白被折叠。
func TestStripInternalState_StripsMarkers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"当前记忆已接近容量上限(2,206/2,200 字符)", ""},
		{"提醒：记忆容量上限快到了", "提醒：快到了"},
		{"好的 (2,206/2,200 字符) 我记住了", "好的 我记住了"},
		{"memory 2,206/2,200 chars used", "memory used"},
		{"line1\n(10/20 字符) line2", "line1\nline2"},
		{"a(10/20 字符)b", "ab"},
		{"  (10/20 字符)  ", ""},
	}
	for _, c := range cases {
		if got := StripInternalState(c.in); got != c.want {
			t.Errorf("StripInternalState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStripInternalState_PreservesFormatting 锁定修复：没有内部指标的正常回复，
// 段落空行与代码缩进必须原样保留（原先 \s{2,} → " " 会把它们压扁，所有渠道丢排版）。
func TestStripInternalState_PreservesFormatting(t *testing.T) {
	in := "第一段\n\n第二段\n```go\nfunc a() {\n    x := 1\n}\n```\n\n- 列表  项"
	if got := StripInternalState(in); got != in {
		t.Fatalf("formatting changed:\n got %q\nwant %q", got, in)
	}
	// 有指标时也只影响删除点，其余段落保持不变。
	in2 := "第一段\n\n好的 (2,206/2,200 字符) 收到\n\n    缩进"
	want2 := "第一段\n\n好的 收到\n\n    缩进"
	if got := StripInternalState(in2); got != want2 {
		t.Fatalf("got %q want %q", got, want2)
	}
}
