package misskey

import "testing"

func newExplicitTestChannel() *MisskeyChannel {
	return &MisskeyChannel{
		cfg:         Config{Host: "https://social.example"},
		botUserID:   "bot-id",
		botUsername: "helperbot",
		mentionRe:   testMentionRe("helperbot"),
	}
}

func TestExplicitlyAddressed(t *testing.T) {
	c := newExplicitTestChannel()
	otherParent := &Note{ID: "p1", User: User{ID: "alice-id", Username: "alice"}}
	inheritParent := &Note{ID: "p2", User: User{ID: "alice-id", Username: "alice"},
		Mentions: []string{"bot-id"}, Text: "@helperbot what's up"}
	botParent := &Note{ID: "p3", User: User{ID: "bot-id", Username: "helperbot"}}

	cases := []struct {
		name string
		note Note
		want bool
	}{
		{"plain note explicit", Note{Text: "@helperbot hello"}, true},
		{"plain note own host", Note{Text: "hi @helperbot@social.example"}, true},
		{"plain note other host same name", Note{Text: "hi @helperbot@elsewhere.example"}, false},
		{"plain note no mention", Note{Text: "hello world"}, false},
		{"email-like not a mention", Note{Text: "mail x@helperbot please"}, false},
		{"reply to bot's own note, no @", Note{ReplyID: "p3", Reply: botParent, Text: "thanks!"}, true},
		{"inherited only (auto-prefix)", Note{ReplyID: "p2", Reply: inheritParent,
			Mentions: []string{"alice-id", "bot-id"}, Text: "@alice @helperbot I agree with you"}, false},
		{"inherited but typed again in body", Note{ReplyID: "p2", Reply: inheritParent,
			Text: "@alice @helperbot I agree, @helperbot thoughts?"}, true},
		{"typed @bot into a reply where bot was not a participant", Note{ReplyID: "p1", Reply: otherParent,
			Text: "@alice @helperbot can you check this?"}, true},
		{"mention in body after prefix", Note{ReplyID: "p1", Reply: otherParent,
			Text: "@alice nice, asking @helperbot too"}, true},
		{"reply, parent not hydrated, only leading @bot", Note{ReplyID: "p9",
			Text: "@alice @helperbot ok"}, false},
		{"mentions array only", Note{ReplyID: "p1", Reply: otherParent,
			Mentions: []string{"bot-id"}, Text: "@alice ok"}, false},
	}
	for _, tc := range cases {
		if got := c.explicitlyAddressed(tc.note); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
