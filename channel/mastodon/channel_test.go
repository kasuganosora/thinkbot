package mastodon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

// TestStatusURL 锁定 Mastodon 帖子链接形态。
//
// ⚠️ 曾经写成 `/@_/<id>`（照搬想象中的短链），实际是 404 死链：Mastodon 的
// 路由是 `/@<username>/<id>`。LLM 会把工具返回的 url 当真链接发给用户，
// 死链很难从日志里看出来，只能靠测试钉住。
func TestStatusURL(t *testing.T) {
	c := &MastodonChannel{cfg: Config{Server: "https://mastodon.social/"}}
	c.botUsername = "kanna"

	if got, want := c.statusURL("10987654321"), "https://mastodon.social/@kanna/10987654321"; got != want {
		t.Errorf("statusURL = %q, want %q", got, want)
	}
	// 身份未解析时（Start 之前）不应拼出 /@_/ 这种死链以外的怪东西
	c2 := &MastodonChannel{cfg: Config{Server: "https://mastodon.social"}}
	if got, want := c2.statusURL("1"), "https://mastodon.social/@_/1"; got != want {
		t.Errorf("statusURL (no identity) = %q, want %q", got, want)
	}
}

func TestStatusLink(t *testing.T) {
	c := &MastodonChannel{cfg: Config{Server: "https://mastodon.social"}}
	c.botUsername = "kanna"

	// 服务端返回的 url 是权威值，尤其远程帖会指向原站
	remote := &Status{ID: "1", URL: "https://mastodon.xyz/@alice/1", Account: Account{Username: "alice"}}
	if got, want := c.statusLink(remote), "https://mastodon.xyz/@alice/1"; got != want {
		t.Errorf("statusLink(remote) = %q, want %q", got, want)
	}
	// 本地帖无 url 字段时按作者 username 拼
	local := &Status{ID: "2", Account: Account{Username: "bob"}}
	if got, want := c.statusLink(local), "https://mastodon.social/@bob/2"; got != want {
		t.Errorf("statusLink(local) = %q, want %q", got, want)
	}
	// 兜底：连作者都没有时用 bot 自己的 username
	bare := &Status{ID: "3"}
	if got, want := c.statusLink(bare), "https://mastodon.social/@kanna/3"; got != want {
		t.Errorf("statusLink(bare) = %q, want %q", got, want)
	}
	if got := c.statusLink(nil); got != "" {
		t.Errorf("statusLink(nil) = %q, want empty", got)
	}
}

// ============================================================================
// 锚点推进 / 生命周期 / 并发安全
// ============================================================================

// newTestChannel 造一个不连网的 channel，供纯逻辑测试用。
// 注意 dedup 与 polls 必须是非 nil map：dedupSeen 与 CreatePollStatus 会直接写。
func newTestChannel() *MastodonChannel {
	return &MastodonChannel{
		name:         "test",
		botAccountID: "self",
		dedup:        map[string]time.Time{},
		polls:        map[string]*pollState{},
	}
}

// TestHandleNotificationMentionAdvancesAnchorOnSkipPaths 锁定 A4：
// mention 的三条提前 return 路径（status 为 nil / 自己发的 / 去重命中）**都必须**推进锚点。
//
// 否则锚点停在旧值，重连时 backfill 用旧 since_id 把这条通知重新拉回，
// 2 分钟的 dedup 窗口挡不住几十分钟后的重放 → 同一条 mention 被二次处理并二次外发
// （Misskey 2026-09-01 重复外发事故的同款根因）。
func TestHandleNotificationMentionAdvancesAnchorOnSkipPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("status 为 nil", func(t *testing.T) {
		c := newTestChannel()
		c.handleNotification(ctx, &Notification{ID: "10", Type: "mention"})
		if got := c.getNotificationAnchor(); got != "10" {
			t.Fatalf("anchor = %q, want %q（nil status 也必须推进锚点）", got, "10")
		}
	})

	t.Run("自己发的", func(t *testing.T) {
		c := newTestChannel()
		c.handleNotification(ctx, &Notification{
			ID: "20", Type: "mention",
			Account: Account{ID: "self"},
			Status:  &Status{ID: "s1", Content: "<p>hi</p>"},
		})
		if got := c.getNotificationAnchor(); got != "20" {
			t.Fatalf("anchor = %q, want %q（自己发的也必须推进锚点）", got, "20")
		}
	})

	t.Run("去重命中", func(t *testing.T) {
		c := newTestChannel()
		// 首次：正常处理，锚点推进到 30
		c.handleNotification(ctx, &Notification{
			ID: "30", Type: "mention",
			Account: Account{ID: "other"},
			Status:  &Status{ID: "s1", Content: "<p>hi</p>"},
		})
		if got := c.getNotificationAnchor(); got != "30" {
			t.Fatalf("first anchor = %q, want %q", got, "30")
		}
		// 重放：dedup 命中会提前 return，锚点仍须推进到 31
		c.handleNotification(ctx, &Notification{
			ID: "31", Type: "mention",
			Account: Account{ID: "other"},
			Status:  &Status{ID: "s1", Content: "<p>hi</p>"},
		})
		if got := c.getNotificationAnchor(); got != "31" {
			t.Fatalf("anchor after dup = %q, want %q（去重命中也必须推进锚点）", got, "31")
		}
	})

	t.Run("favourite 不动锚点", func(t *testing.T) {
		// backfill 的 types[] 只有 mention；被 favourite 顶高锚点会跳过更早的 mention。
		c := newTestChannel()
		c.handleNotification(ctx, &Notification{ID: "40", Type: "favourite", Account: Account{ID: "other"}})
		if got := c.getNotificationAnchor(); got != "" {
			t.Fatalf("anchor = %q, want empty（favourite 不应推进锚点）", got)
		}
	})
}

// TestStopCleansUpPollTimers 锁定 A7：Stop 必须停掉所有在途投票的定时器，
// 否则 channel 停止后 expire 定时器仍会触发 expirePoll / resolvePollVote。
func TestStopCleansUpPollTimers(t *testing.T) {
	c := newTestChannel()
	expired := make(chan struct{})
	c.polls["s1"] = &pollState{
		QuestionID: "q1",
		PollID:     "p1",
		OptionN:    2,
		expire:     time.AfterFunc(50*time.Millisecond, func() { close(expired) }),
		timer:      time.AfterFunc(50*time.Millisecond, func() {}),
	}

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(c.polls) != 0 {
		t.Fatalf("polls 未清空，仍剩 %d 条", len(c.polls))
	}
	if !c.isStopped() {
		t.Fatal("Stop 后 isStopped() 应为 true")
	}
	select {
	case <-expired:
		t.Fatal("expire 定时器在 Stop 之后仍然触发了")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestIdentityAccessorsConcurrent 覆盖 A6 的数据竞争：身份字段在 Start（持 c.mu）
// 里写，但被工具执行 goroutine / 轮询 goroutine / streaming 读循环并发读。
// 必须 `go test -race` 跑，裸读会被 race detector 直接报出来。
func TestIdentityAccessorsConcurrent(t *testing.T) {
	c := newTestChannel()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.selfAccountID()
				_ = c.selfAcct()
				_ = c.selfUsername()
				_ = c.selfMentionRe()
				_ = c.getIngress()
				_ = c.isStopped()
			}
		}()
	}

	// 模拟 Start 里的写入（必须在锁内）
	for i := 0; i < 200; i++ {
		c.mu.Lock()
		c.botAccountID = fmt.Sprintf("id-%d", i)
		c.botUsername = fmt.Sprintf("bot%d", i)
		c.botAcct = fmt.Sprintf("bot%d@example.social", i)
		c.mentionRe = buildMentionRe(c.botUsername, c.botAcct)
		c.mu.Unlock()
	}
	close(stop)
	wg.Wait()
}

// TestTruncateForStatusBudget 锁定 A2：截断预算必须扣掉 `...` 省略号。
//
// strutil.Truncate 超长时返回 `maxRunes + 3` 个字符。若直接把实例上限
// （如 500）传进去，所有**需要被截断**的长文本都会变成 503 字符，
// 被 Mastodon 判 422 `Text exceeds the maximum of 500 characters`。
// 偏偏最长的回复最容易触发，短文本一切正常，自测时极容易漏掉。
func TestTruncateForStatusBudget(t *testing.T) {
	c := &MastodonChannel{api: &apiClient{}}
	c.api.maxChars.Store(500)

	long := strings.Repeat("あ", 600)
	got := c.truncateForStatus(long)
	if n := utf8.RuneCountInString(got); n != 500 {
		t.Fatalf("截断后长度 = %d, want 500（上限本身）", n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("截断结果应以 ... 结尾, got %q", got[len(got)-12:])
	}
	// 短文本原样返回，不被动手脚
	short := strings.Repeat("あ", 100)
	if got := c.truncateForStatus(short); got != short {
		t.Fatalf("短文本被误改: len=%d", utf8.RuneCountInString(got))
	}
	// 上限极小（< 省略号长度）时不能崩、不能返回空串以外的怪东西
	c.api.maxChars.Store(2)
	if got := c.truncateForStatus(long); utf8.RuneCountInString(got) > 2 {
		t.Fatalf("极小上限下超长: len=%d", utf8.RuneCountInString(got))
	}
}

// TestClampPollExpiration 锁定投票存活时长的双向 clamp（Mastodon 默认 300~2629746 秒）。
func TestClampPollExpiration(t *testing.T) {
	a := &apiClient{}
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultPollMinExpiration},  // 未指定 → 下限
		{-5, defaultPollMinExpiration}, // 负值 → 下限
		{60, defaultPollMinExpiration}, // 低于下限 → 下限
		{300, 300},
		{9999999, defaultPollMaxExpiration}, // 高于上限 → 上限
	}
	for _, tc := range cases {
		if got := a.clampPollExpiration(tc.in); got != tc.want {
			t.Errorf("clampPollExpiration(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestTruncateFor 锁定「截断后绝不超限」这一不变式（含省略号预算）。
// 帖子正文与投票选项都走它，两边犯过同一个错。
func TestTruncateFor(t *testing.T) {
	cases := []struct {
		name  string
		s     string
		limit int
		want  int // 期望的最大 rune 数
	}{
		{"短于上限原样", "hello", 25, 5},
		{"刚好等于上限", strings.Repeat("x", 25), 25, 25},
		{"超长扣掉省略号", strings.Repeat("x", 100), 25, 25},
		{"多字节字符", strings.Repeat("あ", 100), 25, 25},
		{"上限小于省略号", strings.Repeat("x", 100), 2, 2},
		{"上限为 0", "hello", 0, 0},
		{"上限为负", "hello", -1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateFor(tc.s, tc.limit)
			if n := utf8.RuneCountInString(got); n > tc.want {
				t.Fatalf("truncateFor(limit=%d) 得到 %d rune，超过上限 %d", tc.limit, n, tc.want)
			}
		})
	}
	// 不需要截断时绝不改动原文
	if got := truncateFor("hello", 25); got != "hello" {
		t.Fatalf("短文本被改动: %q", got)
	}
}

func TestClampVisibilityKeepsDMsDirect(t *testing.T) {
	cases := []struct{ req, parent, want string }{
		{VisibilityUnlisted, VisibilityDirect, VisibilityDirect},
		{VisibilityPublic, VisibilityPrivate, VisibilityPrivate},
		{VisibilityDirect, VisibilityPublic, VisibilityDirect},
		{VisibilityUnlisted, VisibilityPublic, VisibilityUnlisted},
		{VisibilityPublic, "", VisibilityPublic},
	}
	for _, tc := range cases {
		if got := clampVisibility(tc.req, tc.parent); got != tc.want {
			t.Errorf("clampVisibility(%q,%q)=%q want %q", tc.req, tc.parent, got, tc.want)
		}
	}
}

func TestSafeStreamingURL(t *testing.T) {
	cases := []struct {
		server, stream string
		ok             bool
	}{
		{"https://mastodon.social", "wss://streaming.mastodon.social", true},
		{"https://c.hime.at", "wss://c.hime.at", true},
		{"https://c.hime.at", "wss://evil.example", false},
		{"https://c.hime.at", "ws://c.hime.at", false},
		{"https://c.hime.at", "wss://169.254.169.254", false},
		{"https://c.hime.at", "wss://evilc.hime.at", false},
		{"https://c.hime.at", "file:///etc/passwd", false},
	}
	for _, tc := range cases {
		if got := safeStreamingURL(tc.server, tc.stream); got != tc.ok {
			t.Errorf("safeStreamingURL(%q,%q)=%v want %v", tc.server, tc.stream, got, tc.ok)
		}
	}
}
