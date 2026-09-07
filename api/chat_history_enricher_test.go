package api

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestInboundSessionID(t *testing.T) {
	cases := []struct {
		name    string
		msg     core.Message
		wantSID string
		wantOK  bool
	}{
		{
			name:    "telegram private chat",
			msg:     core.Message{Channel: "12345", Metadata: map[string]any{"channel_type": "telegram"}},
			wantSID: "tg:12345",
			wantOK:  true,
		},
		{
			name:    "telegram group chat shares one session by chatID",
			msg:     core.Message{Channel: "98765", Metadata: map[string]any{"channel_type": "telegram"}},
			wantSID: "tg:98765",
			wantOK:  true,
		},
		{
			name:    "telegram empty channel -> skip",
			msg:     core.Message{Channel: "", Metadata: map[string]any{"channel_type": "telegram"}},
			wantOK:  false,
		},
		{
			name:    "web source already handled -> skip",
			msg:     core.Message{Channel: "u1", Metadata: map[string]any{"channel_type": "web"}},
			wantOK:  false,
		},
		{
			name:    "no channel_type -> skip (heartbeat/cron/etc)",
			msg:     core.Message{Channel: "x"},
			wantOK:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sid, ok := inboundSessionID(&c.msg)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if sid != c.wantSID {
				t.Fatalf("sid=%q want %q", sid, c.wantSID)
			}
		})
	}
}

func TestBuildQuoteBlock(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want string
	}{
		{
			name: "with author",
			meta: map[string]any{
				"reply_to_text":  "明天开会",
				"reply_to_from":  "露娜 (@luna)",
			},
			want: "[引用 露娜 (@luna) 的消息]\n明天开会",
		},
		{
			name: "without author",
			meta: map[string]any{"reply_to_text": "hello"},
			want: "[引用消息]\nhello",
		},
		{
			name: "empty quoted text -> no block",
			meta: map[string]any{"reply_to_text": "  ", "reply_to_from": "x"},
			want: "",
		},
		{
			name: "no quoted text -> no block",
			meta: map[string]any{"foo": "bar"},
			want: "",
		},
		{
			name: "nil meta -> no block",
			meta: nil,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildQuoteBlock(c.meta)
			if got != c.want {
				t.Fatalf("buildQuoteBlock=%q want %q", got, c.want)
			}
		})
	}
}
