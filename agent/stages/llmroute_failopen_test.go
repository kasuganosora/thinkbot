package stages

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestIsFailOpenOnMissingControl(t *testing.T) {
	mk := func(ct, chat string, mentioned bool) *core.Envelope {
		return &core.Envelope{Message: core.Message{
			ChatType:  chat,
			Mentioned: mentioned,
			Metadata:  map[string]any{"channel_type": ct},
		}}
	}
	cases := []struct {
		name string
		env  *core.Envelope
		want bool
	}{
		{"nil", nil, false},
		{"telegram private", mk("telegram", core.ChatPrivate, false), true},
		{"telegram group mentioned keeps fail-closed", mk("telegram", core.ChatGroup, true), false},
		{"misskey group mentioned", mk("misskey", core.ChatGroup, true), false},
		{"wecom group mentioned", mk("wecom", core.ChatGroup, true), true},
		{"wecom group not mentioned", mk("wecom", core.ChatGroup, false), false},
		{"wecom private", mk("wecom", core.ChatPrivate, false), true},
	}
	for _, c := range cases {
		if got := isFailOpenOnMissingControl(c.env); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
