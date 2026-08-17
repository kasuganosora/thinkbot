package stages

import "testing"

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
