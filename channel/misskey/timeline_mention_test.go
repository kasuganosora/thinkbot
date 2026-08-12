package misskey

import (
	"regexp"
	"testing"
)

// 与 channel.go 初始化时编译 mentionRe 的逻辑保持一致：@username 或 @username@host。
func testMentionRe(username string) *regexp.Regexp {
	return regexp.MustCompile(`@` + regexp.QuoteMeta(username) + `(?:@[\w.-]+)?\b`)
}

func newTestChannel(username string) *MisskeyChannel {
	return &MisskeyChannel{mentionRe: testMentionRe(username)}
}

func TestTimelineMentioned(t *testing.T) {
	c := newTestChannel("kanna")
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"local mention", "@kanna 回复我下 我测试回复功能", true},
		{"remote mention", "@kanna@maid.lat 你好", true},
		{"mention with punctuation", "@kanna, 在吗?", true},
		{"no mention", "今天的天气真好", false},
		{"mention other user", "@luna 你好", false},
		// 边界：更长用户名不应被误判为 @kanna
		{"longer username not matched", "@kannab 不是 kanna", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.timelineMentioned(Note{Text: tc.text})
			if got != tc.want {
				t.Errorf("timelineMentioned(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
