package api

import "testing"

func TestTitleFromFirstMessage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  hello   world  ", "hello world"},
		{"/clear", ""},
		{"[附件]", "附件"},
		{"现在首页的chat box 输入框也需要优化下感觉还是很简陋", "现在首页的chat box 输入框也需要优化下感觉还是很简陋"},
	}
	for _, c := range cases {
		if got := titleFromFirstMessage(c.in); got != c.want {
			t.Errorf("titleFromFirstMessage(%q)=%q want %q", c.in, got, c.want)
		}
	}
	long := stringsRepeat("你", 40)
	got := titleFromFirstMessage(long)
	if got != stringsRepeat("你", 30)+"…" {
		t.Errorf("truncated got %q", got)
	}
}

func TestIsPlaceholderSessionTitle(t *testing.T) {
	if !isPlaceholderSessionTitle("") || !isPlaceholderSessionTitle("新会话") || !isPlaceholderSessionTitle("默认会话") {
		t.Fatal("placeholders")
	}
	if isPlaceholderSessionTitle("修输入框") {
		t.Fatal("custom title is not placeholder")
	}
}

func stringsRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
