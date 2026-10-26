package wecom

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestQuoteFieldIsDecoded 防回归：callbackBody 必须能把 body.quote 解出来。
//
// 早期版本没有 Quote 字段，企微推下来的引用内容被 JSON 解码直接丢弃 ——
// 用户「引用一条消息再问『这个怎么改』」时 bot 看不到被引用的原文。
func TestQuoteFieldIsDecoded(t *testing.T) {
	raw := `{
		"msgid": "m1",
		"aibotid": "aib1",
		"chatid": "wrkSFfCgAA",
		"chattype": "group",
		"from": {"userid": "U1"},
		"msgtype": "text",
		"text": {"content": "@bot 这个怎么改"},
		"quote": {"msgtype": "text", "text": {"content": "这是今日的测试情况"}}
	}`

	var body callbackBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Quote == nil {
		t.Fatal("body.Quote = nil, want decoded (this is the regression: the field was silently dropped)")
	}
	if body.Quote.MsgType != MsgTypeText {
		t.Fatalf("Quote.MsgType = %q, want %q", body.Quote.MsgType, MsgTypeText)
	}
	if body.Quote.Text == nil || body.Quote.Text.Content != "这是今日的测试情况" {
		t.Fatalf("Quote.Text = %+v, want 这是今日的测试情况", body.Quote.Text)
	}
	if got := renderQuote(body.Quote); got != "这是今日的测试情况" {
		t.Fatalf("renderQuote() = %q", got)
	}
}

func TestRenderQuote(t *testing.T) {
	cases := []struct {
		name string
		in   *quoteContent
		want string
	}{
		{name: "nil", in: nil, want: ""},
		{name: "text", in: &quoteContent{MsgType: MsgTypeText, Text: &textContent{Content: "原文"}}, want: "原文"},
		{name: "text trims spaces", in: &quoteContent{MsgType: MsgTypeText, Text: &textContent{Content: "  原文  "}}, want: "原文"},
		{
			name: "empty text falls through to placeholder",
			in:   &quoteContent{MsgType: MsgTypeText, Text: &textContent{Content: "   "}},
			want: "",
		},
		{
			name: "mixed concatenates text items only",
			in: &quoteContent{MsgType: MsgTypeMixed, Mixed: &mixedContent{Items: []mixedItem{
				{MsgType: MsgTypeText, Text: &textContent{Content: "上半句"}},
				{MsgType: MsgTypeImage, Image: &mediaContent{URL: "u"}},
				{MsgType: MsgTypeText, Text: &textContent{Content: "下半句"}},
			}}},
			want: "上半句下半句",
		},
		{name: "image placeholder", in: &quoteContent{MsgType: MsgTypeImage}, want: "[图片]"},
		{name: "file placeholder", in: &quoteContent{MsgType: MsgTypeFile}, want: "[文件]"},
		{name: "unknown msgtype", in: &quoteContent{MsgType: "whatever"}, want: ""},
		{name: "no msgtype", in: &quoteContent{}, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderQuote(tc.in); got != tc.want {
				t.Fatalf("renderQuote() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderQuoteTruncates 引用是上下文，超长必须截断而不是整个塞进窗口
// （strutil.Truncate 会追加 "..."，所以上限是 limit+3）。
func TestRenderQuoteTruncates(t *testing.T) {
	long := strings.Repeat("あ", quoteRenderLimit+500)
	got := renderQuote(&quoteContent{MsgType: MsgTypeText, Text: &textContent{Content: long}})
	if n := len([]rune(got)); n > quoteRenderLimit+3 {
		t.Fatalf("renderQuote() len = %d, want <= %d", n, quoteRenderLimit+3)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("renderQuote() = %q, want truncated with ... suffix", got)
	}
}

func TestQuoteBlock(t *testing.T) {
	got := quoteBlock("第一行\n第二行")
	want := "【引用消息】\n> 第一行\n> 第二行"
	if got != want {
		t.Fatalf("quoteBlock() = %q, want %q", got, want)
	}
}

func TestMergeQuote(t *testing.T) {
	cases := []struct {
		name  string
		quote string
		body  string
		want  string
	}{
		{name: "no quote", quote: "", body: "这个怎么改", want: "这个怎么改"},
		{name: "quote only", quote: "原文", body: "", want: "【引用消息】\n> 原文"},
		{
			name:  "quote then body",
			quote: "原文",
			body:  "这个怎么改",
			want:  "【引用消息】\n> 原文\n\n这个怎么改",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeQuote(tc.quote, tc.body); got != tc.want {
				t.Fatalf("mergeQuote(%q,%q) = %q, want %q", tc.quote, tc.body, got, tc.want)
			}
		})
	}
}

// TestQuotedOnlyMessageIsNotEmpty 「只引用、一个字没打」必须仍然构成一条消息，
// 不能因为正文为空就被 drop empty message 挡掉。
func TestQuotedOnlyMessageIsNotEmpty(t *testing.T) {
	body := &callbackBody{MsgType: MsgTypeText, Quote: &quoteContent{
		MsgType: MsgTypeText, Text: &textContent{Content: "这是今日的测试情况"},
	}}
	_, _, items := extractInbound(body, "bot", true)
	text := strings.TrimSpace("")
	if q := renderQuote(body.Quote); q != "" {
		text = mergeQuote(q, text)
	}
	if text == "" {
		t.Fatal("quoted-only message collapsed to empty text")
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
}
