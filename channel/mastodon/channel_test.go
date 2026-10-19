package mastodon

import (
	"encoding/json"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
)

// 接口实现断言：MastodonChannel 必须同时满足这些契约，否则心跳发帖、
// 工具注册、出站分发会静默失效（编译期就该炸，别等运行时才发现）。
var (
	_ bot.Channel                    = (*MastodonChannel)(nil)
	_ bot.Sender                     = (*MastodonChannel)(nil)
	_ core.TimelinePoster            = (*MastodonChannel)(nil)
	_ agenttools.ChannelToolProvider = (*MastodonChannel)(nil)
)

func TestMapVisibility(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Mastodon 原生取值原样通过
		{"public", VisibilityPublic},
		{"unlisted", VisibilityUnlisted},
		{"private", VisibilityPrivate},
		{"direct", VisibilityDirect},
		// Misskey 语义取值映射到 Mastodon
		{"home", VisibilityUnlisted},
		{"followers", VisibilityPrivate},
		{"specified", VisibilityDirect},
		// 大小写与空白容错
		{"  PUBLIC  ", VisibilityPublic},
		// 未知值回落 unlisted（保守，不误发公开帖）
		{"", VisibilityUnlisted},
		{"bogus", VisibilityUnlisted},
	}
	for _, tt := range tests {
		if got := mapVisibility(tt.in); got != tt.want {
			t.Errorf("mapVisibility(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNewerID(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"空 a 恒为 false", "", "100", false},
		{"空 b 恒为 true", "100", "", true},
		{"同长度字典序更新", "200", "100", true},
		{"同长度字典序更旧", "100", "200", false},
		{"相等不算更新", "100", "100", false},
		{"位数多者更大", "1000", "999", true},
		{"位数少者更小", "999", "1000", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newerID(tt.a, tt.b); got != tt.want {
				t.Errorf("newerID(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestSetNotificationAnchorMonotonic 覆盖锚点单调递增守卫。
// 背景：Misskey 曾因锚点被乱序旧值拉回，导致断连 backfill 把整晚 mention 重放两轮。
// Mastodon 复用同一守卫，这里固化行为。
func TestSetNotificationAnchorMonotonic(t *testing.T) {
	c := &MastodonChannel{name: "t"}
	c.setNotificationAnchor("100")
	if c.getNotificationAnchor() != "100" {
		t.Fatalf("锚点未设置: %q", c.getNotificationAnchor())
	}
	// 更新的 ID 推进
	c.setNotificationAnchor("200")
	if got := c.getNotificationAnchor(); got != "200" {
		t.Errorf("更新的 ID 应推进锚点, got %q", got)
	}
	// 乱序到达的旧 ID 不得拉回
	c.setNotificationAnchor("150")
	if got := c.getNotificationAnchor(); got != "200" {
		t.Errorf("旧 ID 不得拉回锚点, got %q", got)
	}
	// 空 ID 不更新
	c.setNotificationAnchor("")
	if got := c.getNotificationAnchor(); got != "200" {
		t.Errorf("空 ID 不得更新锚点, got %q", got)
	}
}

func TestMastodonIngressChannelID(t *testing.T) {
	tests := []struct {
		name       string
		eventType  string
		mentioned  bool
		visibility string
		userID     string
		want       string
	}{
		{"时间线普通帖共享空间", "timeline", false, VisibilityPublic, "u1", "mastodon:timeline"},
		{"时间线上被 @ 仍按用户隔离", "timeline", true, VisibilityPublic, "u1", "u1"},
		{"时间线上的 direct 按用户隔离", "timeline", false, VisibilityDirect, "u1", "u1"},
		{"mention 事件按用户隔离", "mention", true, VisibilityPublic, "u1", "u1"},
		{"direct 流按用户隔离", "direct", true, VisibilityDirect, "u1", "u1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mastodonIngressChannelID(tt.eventType, tt.mentioned, tt.visibility, tt.userID)
			if got != tt.want {
				t.Errorf("= %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name string
		st   *Status
		want string
	}{
		{"原创帖", &Status{Content: "<p>hi</p>"}, "status"},
		{"回复", &Status{Content: "<p>hi</p>", InReplyToID: "p1"}, "reply"},
		{"纯转发", &Status{Content: "", Reblog: &Status{Content: "<p>x</p>"}}, "reblog"},
		{"带评引用转发", &Status{Content: "<p>评论</p>", Reblog: &Status{Content: "<p>x</p>"}}, "quote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyStatus(tt.st); got != tt.want {
				t.Errorf("classifyStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildMentionRe(t *testing.T) {
	// 本地账号：只匹配 @bot
	re := buildMentionRe("bot", "bot")
	if !re.MatchString("@bot 你好") {
		t.Error("@bot 应匹配")
	}
	if !re.MatchString("@bot@example.social 你好") {
		t.Error("@bot@域 应匹配")
	}
	if re.MatchString("@botuser 你好") {
		t.Error("@botuser 不应误匹配 @bot")
	}
	// 远程账号：username 与 acct 都要覆盖
	re2 := buildMentionRe("kanna", "kanna@maid.lat")
	if !re2.MatchString("@kanna hi") {
		t.Error("@kanna 应匹配")
	}
	if !re2.MatchString("@kanna@maid.lat hi") {
		t.Error("@kanna@maid.lat 应匹配")
	}
	if re2.MatchString("@kanna2 hi") {
		t.Error("@kanna2 不应误匹配")
	}
	// 空输入返回 nil，调用方已做 nil 检查
	if buildMentionRe("", "") != nil {
		t.Error("空用户名/acct 应返回 nil")
	}
}

func TestStreamKind(t *testing.T) {
	tests := []struct {
		name          string
		streams       []string
		wantKind      string
		wantMentioned bool
	}{
		{"通知流不在 timeline 判定内", []string{"user:notification"}, "timeline", false},
		{"关注时间线", []string{"user"}, "timeline", false},
		{"联邦时间线", []string{"public"}, "timeline", false},
		{"本站时间线", []string{"public:local"}, "timeline", false},
		{"私信视为被点名", []string{"direct"}, "direct", true},
		{"未知流回落 timeline", []string{"list:1"}, "timeline", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, mentioned := streamKind(tt.streams)
			if kind != tt.wantKind || mentioned != tt.wantMentioned {
				t.Errorf("streamKind(%v) = (%q, %v), want (%q, %v)",
					tt.streams, kind, mentioned, tt.wantKind, tt.wantMentioned)
			}
		})
	}
}

func TestDecodePayload(t *testing.T) {
	// 形态 1：标准 Mastodon —— payload 是 JSON 编码的字符串
	raw1, _ := json.Marshal(`{"id":"111","content":"<p>hi</p>"}`)
	var s1 Status
	if err := decodePayload(raw1, &s1); err != nil {
		t.Fatalf("形态 1 解析失败: %v", err)
	}
	if s1.ID != "111" || s1.Content != "<p>hi</p>" {
		t.Errorf("形态 1 解析结果错误: %+v", s1)
	}

	// 形态 2：部分兼容实现 —— payload 直接是 JSON 对象
	raw2 := json.RawMessage(`{"id":"222","content":"<p>yo</p>"}`)
	var s2 Status
	if err := decodePayload(raw2, &s2); err != nil {
		t.Fatalf("形态 2 解析失败: %v", err)
	}
	if s2.ID != "222" {
		t.Errorf("形态 2 解析结果错误: %+v", s2)
	}

	// 空 payload 返回哨兵错误（ping 心跳走这条路径）
	var s3 Status
	if err := decodePayload(nil, &s3); err == nil {
		t.Error("空 payload 应返回错误")
	}

	// payloadID：delete 事件是纯文本 ID
	rawID, _ := json.Marshal("333")
	if got := payloadID(rawID); got != "333" {
		t.Errorf("payloadID() = %q, want %q", got, "333")
	}
}

func TestNormalizeStreams(t *testing.T) {
	got := normalizeStreams(Config{Streams: []string{"user", "public", "user", "bogus", "public:local"}})
	want := []string{"user", "public", "public:local"}
	if len(got) != len(want) {
		t.Fatalf("= %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildReplyMentionPrefix(t *testing.T) {
	c := &MastodonChannel{botUsername: "bot", botAcct: "bot@example.social"}
	parent := &Status{
		Account:  Account{ID: "a1", Username: "alice", Acct: "alice@example.social"},
		Mentions: []Mention{{ID: "c1", Acct: "carol@example.social"}},
	}

	got := c.buildReplyMentionPrefix(parent)
	// 父帖作者 + 父帖里被提及的第三方，各出现一次且不重复
	if got != "@alice@example.social @carol@example.social " {
		t.Errorf("= %q", got)
	}

	// Bot 自身被排除：父帖作者就是 bot 时前缀为空
	selfParent := &Status{Account: Account{ID: "self", Username: "bot", Acct: "bot@example.social"}}
	if p := c.buildReplyMentionPrefix(selfParent); p != "" {
		t.Errorf("自身帖不应产生 @ 前缀, got %q", p)
	}
	if c.buildReplyMentionPrefix(nil) != "" {
		t.Error("nil 父帖应返回空")
	}
}

func TestApplyPollVote(t *testing.T) {
	// 单选：首票立即结算
	st := &pollState{OptionN: 3}
	if act := applyPollVote(st, 1); act != pollVoteResolveNow {
		t.Errorf("单选首票应立刻结算, got %v", act)
	}
	if len(st.Selected) != 1 || st.Selected[0] != 1 {
		t.Errorf("单选 Selected = %v", st.Selected)
	}

	// 单选越界忽略
	if act := applyPollVote(&pollState{OptionN: 2}, 5); act != pollVoteIgnore {
		t.Error("越界 choice 应忽略")
	}

	// 多选：累计 + debounce，全选满后立刻结算
	multi := &pollState{Multiple: true, OptionN: 3}
	if act := applyPollVote(multi, 0); act != pollVoteDebounce {
		t.Errorf("多选首票应 debounce, got %v", act)
	}
	if act := applyPollVote(multi, 0); act != pollVoteIgnore {
		t.Error("重复 choice 应忽略")
	}
	if act := applyPollVote(multi, 1); act != pollVoteDebounce {
		t.Error("新 choice 应继续 debounce")
	}
	if act := applyPollVote(multi, 2); act != pollVoteResolveNow {
		t.Error("选满应立刻结算")
	}
}

func TestPollVotes(t *testing.T) {
	p := &Poll{Options: []PollOption{{VotesCount: 1}, {VotesCount: 5}, {VotesCount: 0}}}
	got := pollVotes(p)
	want := []int{1, 5, 0}
	if len(got) != len(want) {
		t.Fatalf("= %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %d, want %d", i, got[i], want[i])
		}
	}
	if pollVotes(nil) != nil {
		t.Error("nil poll 应返回 nil")
	}
}

func TestStatusAttachments(t *testing.T) {
	medias := []MediaAttachment{
		{Type: "image", URL: "https://x/1.png"},
		{Type: "video", URL: "https://x/2.mp4"},
		{Type: "gifv", URL: "https://x/3.mp4"},
		{Type: "audio", URL: "https://x/4.mp3"},
		{Type: "unknown", URL: "https://x/5.bin"},
	}
	atts := statusAttachments(medias)
	if len(atts) != 5 {
		t.Fatalf("= %d, want 5", len(atts))
	}
	wantTypes := []string{
		core.AttachmentTypeImage,
		core.AttachmentTypeVideo,
		core.AttachmentTypeVideo,
		core.AttachmentTypeAudio,
		core.AttachmentTypeFile,
	}
	for i, wt := range wantTypes {
		if atts[i].Type != wt {
			t.Errorf("[%d] Type = %q, want %q", i, atts[i].Type, wt)
		}
		if atts[i].URL != medias[i].URL {
			t.Errorf("[%d] URL = %q, want %q", i, atts[i].URL, medias[i].URL)
		}
	}
	if statusAttachments(nil) != nil {
		t.Error("nil 媒体列表应返回 nil")
	}
}

func TestAccountHandle(t *testing.T) {
	// acct 优先（含域）
	if got := (Account{Username: "alice", Acct: "alice@example.social"}).handle(); got != "@alice@example.social" {
		t.Errorf("= %q", got)
	}
	// acct 缺失时退回 username
	if got := (Account{Username: "alice"}).handle(); got != "@alice" {
		t.Errorf("= %q", got)
	}
}
