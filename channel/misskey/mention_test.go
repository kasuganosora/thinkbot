package misskey

import "testing"

func TestBuildReplyMentionPrefix(t *testing.T) {
	c := &MisskeyChannel{botUsername: "kanna"}

	cases := []struct {
		name string
		note *Note
		want string
	}{
		{
			name: "owner only",
			note: &Note{User: User{Username: "alice"}},
			want: "@alice ",
		},
		{
			name: "owner plus other mentions, exclude self",
			note: &Note{
				User: User{Username: "alice"},
				Text: "@bob @kanna 你好",
			},
			want: "@alice @bob ",
		},
		{
			name: "remote mention keeps host",
			note: &Note{
				User: User{Username: "alice", Host: "remote.example"},
				Text: "@bob@remote.example hi",
			},
			want: "@alice@remote.example @bob@remote.example ",
		},
		{
			name: "duplicate of owner in text is deduped",
			note: &Note{
				User: User{Username: "alice"},
				Text: "@alice 在吗",
			},
			want: "@alice ",
		},
		{
			name: "nil note",
			note: nil,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.buildReplyMentionPrefix(tc.note)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
