package mastodon

import "testing"

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "空输入",
			in:   "",
			want: "",
		},
		{
			name: "单段落",
			in:   "<p>hello</p>",
			want: "hello",
		},
		{
			name: "多段落之间补换行",
			in:   "<p>第一段</p><p>第二段</p>",
			want: "第一段\n第二段",
		},
		{
			name: "br 转换行",
			in:   "<p>第一行<br>第二行<br />第三行</p>",
			want: "第一行\n第二行\n第三行",
		},
		{
			name: "实体反转义",
			in:   "<p>a &amp; b &lt; c &gt; d</p>",
			want: "a & b < c > d",
		},
		{
			name: "转义文本不会被当成标签",
			in:   "<p>&lt;p&gt;fake&lt;/p&gt;</p>",
			want: "<p>fake</p>",
		},
		{
			name: "mention 的 @acct 保留为文本",
			in:   `<p><span class="h-card"><a href="https://example.social/@alice" class="u-url mention">@alice</a></span> 你好</p>`,
			want: "@alice 你好",
		},
		{
			name: "链接只保留文本",
			in:   `<p>见 <a href="https://example.com">这个链接</a></p>`,
			want: "见 这个链接",
		},
		{
			name: "列表项换行",
			in:   "<ul><li>A</li><li>B</li></ul>",
			want: "A\nB",
		},
		{
			name: "连续空段落压缩为单个空行",
			in:   "<p>a</p><p></p><p></p><p></p><p>b</p>",
			want: "a\n\nb",
		},
		{
			name: "行尾空白被去掉",
			in:   "<p>hello   </p><p>world  </p>",
			want: "hello\nworld",
		},
		{
			name: "纯 boost 的空正文",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := htmlToText(tt.in); got != tt.want {
				t.Errorf("htmlToText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsBlankContent(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"<p></p>", true},
		{"<p>   </p>", true},
		{"<p>x</p>", false},
	}
	for _, tt := range tests {
		if got := isBlankContent(tt.in); got != tt.want {
			t.Errorf("isBlankContent(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestStatusIsPureReblog(t *testing.T) {
	// 纯 boost：reblog 非空、自己无正文
	pure := &Status{ID: "1", Reblog: &Status{ID: "0", Content: "<p>原文</p>"}}
	if !pure.IsPureReblog() {
		t.Error("纯 boost 应判定为 IsPureReblog")
	}
	// 带评引用转发：自己有正文
	quote := &Status{ID: "2", Content: "<p>我的评论</p>", Reblog: &Status{ID: "0", Content: "<p>原文</p>"}}
	if quote.IsPureReblog() {
		t.Error("带评引用转发不应判定为 IsPureReblog")
	}
	// 普通帖
	if (&Status{ID: "3", Content: "<p>hi</p>"}).IsPureReblog() {
		t.Error("普通帖不应判定为 IsPureReblog")
	}
}
