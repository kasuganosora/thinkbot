package stages

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// TestParseReplyControl 锁定回复控制门控的解析语义（fail-closed）：
//   - send:true  → 返回干净正文，控制块被剥离
//   - send:false → 模型显式不回复，调用方应抑制出站
//   - 缺失分隔符 / JSON 解析失败 → ok=false，调用方 fail-closed 不出站
func TestParseReplyControl(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantSend  bool
		wantOK    bool
		wantClean string
	}{
		{
			name:      "send true with reply body",
			input:     "收到，这就去查一下缓存配置。\n\n@@REPLY_CONTROL@@{\"send\": true}",
			wantSend:  true,
			wantOK:    true,
			wantClean: "收到，这就去查一下缓存配置。",
		},
		{
			name:      "send false with private monologue before control",
			input:     "这条内容涉及规避广告审核，我不适合扩散，保持沉默。\n@@REPLY_CONTROL@@{\"send\": false}",
			wantSend:  false,
			wantOK:    true,
			wantClean: "这条内容涉及规避广告审核，我不适合扩散，保持沉默。",
		},
		{
			name:      "missing delimiter -> fail-closed",
			input:     "就不做互动了，这种内容不扩散。",
			wantSend:  false,
			wantOK:    false,
			wantClean: "就不做互动了，这种内容不扩散。",
		},
		{
			name:      "malformed json -> fail-closed",
			input:     "正常回复正文\n@@REPLY_CONTROL@@{send: oops}",
			wantSend:  false,
			wantOK:    false,
			wantClean: "正常回复正文\n@@REPLY_CONTROL@@{send: oops}",
		},
		{
			name:      "code fence around json",
			input:     "好的\n@@REPLY_CONTROL@@\n```json\n{\"send\": true}\n```",
			wantSend:  true,
			wantOK:    true,
			wantClean: "好的",
		},
		{
			name:      "send true but empty body -> clean empty",
			input:     "@@REPLY_CONTROL@@{\"send\": true}",
			wantSend:  true,
			wantOK:    true,
			wantClean: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			send, clean, ok := parseReplyControl(c.input)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v want %v (input=%q)", ok, c.wantOK, c.input)
			}
			if send != c.wantSend {
				t.Fatalf("send: got %v want %v (input=%q)", send, c.wantSend, c.input)
			}
			if clean != c.wantClean {
				t.Fatalf("clean: got %q want %q", clean, c.wantClean)
			}
		})
	}
}

// TestStripInternalTags 锁定私有心里话标签的剥离语义：
// <internal>...</internal> 及其内容（含多行/未闭合）必须从出站文本中彻底移除。
func TestStripInternalTags(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "internal block stripped, public kept",
			input: "<internal>我其实觉得这内容很低质</internal>谢谢分享，已收藏。",
			want:  "谢谢分享，已收藏。",
		},
		{
			name:  "multiline internal stripped",
			input: "<internal>\n这条线索不值得追，\n先放一放。\n</internal>收到，我来跟进。",
			want:  "收到，我来跟进。",
		},
		{
			name:  "unclosed internal stripped to end",
			input: "公开回复在这里。<internal>没写完的心里话",
			want:  "公开回复在这里。",
		},
		{
			name:  "no internal tags -> unchanged",
			input: "纯公开回复，没有任何心里话。",
			want:  "纯公开回复，没有任何心里话。",
		},
		{
			name:  "only internal -> empty after strip",
			input: "<internal>全是心里话没有公开内容</internal>",
			want:  "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := memory.StripInternalTags(c.input)
			if got != c.want {
				t.Fatalf("StripInternalTags: got %q want %q", got, c.want)
			}
		})
	}
}

// TestExtractPublicReply 锁定「同时输出心里话和想说的话」场景的提取语义（取巧三态）：
//   - 有 <public> 标签 → 只发 <public> 内文，<internal> 心里话丢弃（最防泄漏）
//   - 含 <internal> 但无 <public> → 整段不发（返回空，fail-closed）
//   - 纯文本无标签 → 原样返回，由上层 control 块决定
func TestExtractPublicReply(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "public tag wins, internal dropped",
			input: "<internal>这人又在水文，懒得理</internal><public>收到，多谢分享~</public>",
			want:  "收到，多谢分享~",
		},
		{
			name:  "internal but no public -> dropped entirely (fail-closed)",
			input: "<internal>其实我不太认同</internal>好的，我来帮你看看。",
			want:  "",
		},
		{
			name:  "internal only no public -> empty",
			input: "<internal>全是心里话没有公开内容</internal>",
			want:  "",
		},
		{
			name:  "plain text no tags -> unchanged",
			input: "这是一段纯公开回复，没有标签。",
			want:  "这是一段纯公开回复，没有标签。",
		},
		{
			name:  "public wraps multiple blocks concatenated",
			input: "<public>第一段公开。</public><internal>私下吐槽</internal><public>第二段公开。</public>",
			want:  "第一段公开。\n第二段公开。",
		},
		{
			name:  "public with nested internal -> internal also stripped",
			input: "<public>公开说：<internal>但我不爽</internal>还是帮你吧</public>",
			want:  "公开说：还是帮你吧",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractPublicReply(c.input)
			if got != c.want {
				t.Fatalf("extractPublicReply: got %q want %q", got, c.want)
			}
		})
	}
}
