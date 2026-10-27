package wecom

// 纯函数单测：入站归一化、出站寻址、字节截断、限流。
//
// 覆盖的都是**实测踩过坑**的点，写测试的目的不是凑覆盖率，而是把那些
// 「短文本一切正常、长文本/边缘输入才炸」的行为钉死。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		chatID  string
		kind    int
		wantErr bool
	}{
		{name: "single", in: "wecom:dm:user001", chatID: "user001", kind: ChatTypeSingle},
		{name: "group", in: "wecom:chat:chat001", chatID: "chat001", kind: ChatTypeGroup},
		{name: "empty userid", in: "wecom:dm:", wantErr: true},
		{name: "empty chatid", in: "wecom:chat:", wantErr: true},
		{name: "unknown prefix", in: "user001", wantErr: true},
		{name: "wrong platform prefix", in: "misskey:dm:abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, kind, err := parseTarget(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTarget(%q) = (%q,%d,nil), want error", tc.in, id, kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTarget(%q) unexpected error: %v", tc.in, err)
			}
			if id != tc.chatID || kind != tc.kind {
				t.Fatalf("parseTarget(%q) = (%q,%d), want (%q,%d)", tc.in, id, kind, tc.chatID, tc.kind)
			}
		})
	}
}

func TestStripMention(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		botName  string
		isGroup  bool
		wantText string
		wantAt   bool
	}{
		{name: "single chat always mentioned", text: "你好", isGroup: false, wantText: "你好", wantAt: true},
		{name: "group with configured name", text: "@mybot 你好", botName: "mybot", isGroup: true, wantText: "你好", wantAt: true},
		{name: "group mention no space", text: "@mybot你好", botName: "mybot", isGroup: true, wantText: "你好", wantAt: true},
		{name: "group mention only", text: "@mybot ", botName: "mybot", isGroup: true, wantText: "", wantAt: true},
		// ⚠️ @ 在句中（实测真实输入：「群聊带 @ 的模式。@mybot 看看能否收到」）。
		// 只判 HasPrefix 会把这类消息判成未 @ → 整条群聊链路不回复。
		{name: "group mention mid-sentence", text: "群聊带 @ 的模式。@mybot 看看能否收到", botName: "mybot",
			isGroup: true, wantText: "群聊带 @ 的模式。看看能否收到", wantAt: true},
		// ⚠️ 没解析出 @ 也按被 @ 处理：企微只在群里被 @ 时才推送消息，
		// 收不到 @ 往往是客户端渲染差异，不能因此让机器人哑火。
		{name: "group plain text still counts", text: "大家好", botName: "mybot", isGroup: true, wantText: "大家好", wantAt: true},
		{name: "group fallback generic at", text: "@mybot hello", isGroup: true, wantText: "hello", wantAt: true},
		{name: "group fallback at only", text: "@mybot", isGroup: true, wantText: "", wantAt: true},
		{name: "group empty", text: "", botName: "mybot", isGroup: true, wantText: "", wantAt: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, at := stripMention(tc.text, tc.botName, tc.isGroup)
			if got != tc.wantText || at != tc.wantAt {
				t.Fatalf("stripMention(%q,%q,%v) = (%q,%v), want (%q,%v)",
					tc.text, tc.botName, tc.isGroup, got, at, tc.wantText, tc.wantAt)
			}
		})
	}
}

func TestExtractInbound(t *testing.T) {
	t.Run("mixed has no top-level text", func(t *testing.T) {
		body := &callbackBody{
			MsgType: MsgTypeMixed,
			Mixed: &mixedContent{Items: []mixedItem{
				{MsgType: MsgTypeText, Text: &textContent{Content: "@mybot "}},
				{MsgType: MsgTypeImage, Image: &mediaContent{URL: "https://x/y", AESKey: "k"}},
			}},
		}
		text, at, items := extractInbound(body, "mybot", true)
		if len(items) != 1 || items[0].Type != MsgTypeImage {
			t.Fatalf("items = %+v, want one image", items)
		}
		if !at {
			t.Fatal("mentioned = false, want true")
		}
		if strings.TrimSpace(text) != "" {
			t.Fatalf("text = %q, want empty (图片消息正文为空)", text)
		}
	})

	t.Run("mixed with several images keeps them all", func(t *testing.T) {
		// ⚠️ 一条 mixed 可能带多张图；只留第一个会变成「发三张收到一张」。
		body := &callbackBody{
			MsgType: MsgTypeMixed,
			Mixed: &mixedContent{Items: []mixedItem{
				{MsgType: MsgTypeImage, Image: &mediaContent{URL: "u1", AESKey: "k1"}},
				{MsgType: MsgTypeImage, Image: &mediaContent{URL: "u2", AESKey: "k2"}},
				{MsgType: MsgTypeImage, Image: &mediaContent{URL: "u3", AESKey: "k3"}},
			}},
		}
		_, _, items := extractInbound(body, "mybot", true)
		if len(items) != 3 {
			t.Fatalf("items = %d, want 3", len(items))
		}
	})

	t.Run("plain text group", func(t *testing.T) {
		body := &callbackBody{MsgType: MsgTypeText, Text: &textContent{Content: "@mybot 今天天气"}}
		text, at, items := extractInbound(body, "mybot", true)
		if text != "今天天气" || !at || len(items) != 0 {
			t.Fatalf("got (%q,%v,%d), want (\"今天天气\",true,0)", text, at, len(items))
		}
	})

	t.Run("single file", func(t *testing.T) {
		body := &callbackBody{MsgType: MsgTypeFile, File: &mediaContent{URL: "https://x/f", AESKey: "k"}}
		text, at, items := extractInbound(body, "mybot", false)
		if len(items) != 1 || items[0].Type != MsgTypeFile {
			t.Fatalf("items = %+v, want one file", items)
		}
		if !at {
			t.Fatal("single chat must always be mentioned=true")
		}
		if text != "" {
			t.Fatalf("text = %q, want empty", text)
		}
	})
}

func TestTruncateForMarkdown(t *testing.T) {
	t.Run("short text untouched", func(t *testing.T) {
		s := "hello"
		if got := truncateForMarkdown(s); got != s {
			t.Fatalf("got %q, want %q", got, s)
		}
	})

	t.Run("ascii truncated to byte limit", func(t *testing.T) {
		s := strings.Repeat("a", 30000)
		got := truncateForMarkdown(s)
		if len(got) != wecomMarkdownLimit {
			t.Fatalf("len = %d, want %d", len(got), wecomMarkdownLimit)
		}
		if !strings.HasSuffix(got, markdownEllipsis) {
			t.Fatalf("missing ellipsis suffix: %q", got[len(got)-8:])
		}
	})

	// ⚠️ 这条是核心：中文 3 字节/rune，按 rune 截断会静默超限。
	t.Run("cjk truncated by bytes not runes", func(t *testing.T) {
		s := strings.Repeat("汉", 10000) // 30000 字节
		got := truncateForMarkdown(s)
		if len(got) > wecomMarkdownLimit {
			t.Fatalf("len = %d, exceeds byte limit %d", len(got), wecomMarkdownLimit)
		}
		if !strings.HasSuffix(got, markdownEllipsis) {
			t.Fatalf("missing ellipsis suffix")
		}
		if !strings.HasPrefix(got, "汉") {
			t.Fatalf("unexpected prefix: %q", got[:8])
		}
	})

	// 截断边界正好落在多字节 rune 中间时，不能产出非法 UTF-8。
	t.Run("never splits a rune", func(t *testing.T) {
		// 1 个 ASCII + N 个汉字，让第 20477 字节落在某个汉字的第 2 字节上。
		s := "a" + strings.Repeat("汉", 10000)
		got := truncateForMarkdown(s)
		if len(got) > wecomMarkdownLimit {
			t.Fatalf("len = %d, exceeds limit", len(got))
		}
		for _, r := range got {
			if r == '\uFFFD' {
				t.Fatalf("produced replacement char: illegal UTF-8 after truncation")
			}
		}
	})
}

func TestSendLimiter(t *testing.T) {
	l := newSendLimiter()
	// 桶容量 5：前 5 次立即返回，之后必须等待。
	for i := 0; i < wecomSendBurst; i++ {
		if _, ok := l.tryAcquire("wecom:dm:u1"); !ok {
			t.Fatalf("acquire #%d should succeed (burst capacity)", i+1)
		}
	}
	if _, ok := l.tryAcquire("wecom:dm:u1"); ok {
		t.Fatal("acquire after burst exhaustion should fail")
	}
	// 其他会话不受影响。
	if _, ok := l.tryAcquire("wecom:chat:g1"); !ok {
		t.Fatal("different chat should have its own bucket")
	}

	// 等待后应能再次取得（补充速率 0.5/s → 2s 一个）。
	start := time.Now()
	if err := l.wait(context.Background(), "wecom:dm:u1"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("wait returned too early (%s); rate limiter not actually limiting", elapsed)
	}
}

func TestSendLimiterPrune(t *testing.T) {
	l := newSendLimiter()
	l.tryAcquire("wecom:dm:old")
	l.buckets["wecom:dm:old"].last = time.Now().Add(-bucketIdleTTL - time.Minute)
	l.prune()
	if _, ok := l.buckets["wecom:dm:old"]; ok {
		t.Fatal("idle bucket should be pruned")
	}
}

// ============================================================================
// caption 合并窗口（docs §9.2.4）
//
// 企微发文件时把配文拆成另一条**独立回调**（两个 msgid、无关联字段），
// 不合并的话 bot 会对「文字」和「文件」各回一次 —— 2026-10-20 实测每发一次
// 文件用户就收到两条回复。这里锁住三条不变量：
//   ① 窗口内的媒体消息能取到配文并合并
//   ② 窗口内没等到媒体 → 文本照常投递（不能被吞）
//   ③ 合并只发生在**同人同会话**，跨用户/跨群不串
// ============================================================================

func TestMergeCaption(t *testing.T) {
	cases := []struct{ caption, body, want string }{
		{"大文件测试", "[收到文件 a.bin]", "大文件测试\n[收到文件 a.bin]"},
		{"", "[收到文件 a.bin]", "[收到文件 a.bin]"},
		{"大文件测试", "", "大文件测试"},
		{"  两边有空格  ", "  body  ", "两边有空格\nbody"},
	}
	for _, tc := range cases {
		if got := mergeCaption(tc.caption, tc.body); got != tc.want {
			t.Fatalf("mergeCaption(%q,%q) = %q, want %q", tc.caption, tc.body, got, tc.want)
		}
	}
}

// TestCaptionWindowMergesThenConsumesOnce 验证 ①：取过一次就没了，
// 连着来两个文件不会把同一句配文贴两遍。
func TestCaptionWindowMergesThenConsumesOnce(t *testing.T) {
	c := &WeComChannel{cfg: Config{CaptionWindow: time.Second}}
	c.pend = make(map[string]*pendingCaption)

	c.holdPending(context.Background(), "chat|u1", time.Minute, core.Message{Text: "大文件测试"})
	if got := pendText(c.takePending("chat|u1")); got != "大文件测试" {
		t.Fatalf("takePending = %q, want %q", got, "大文件测试")
	}
	// ⚠️ 取走后必须为空：同一句配文不能被第二个文件再用一次。
	if got := pendText(c.takePending("chat|u1")); got != "" {
		t.Fatalf("second takePending = %q, want empty (caption must be consumed once)", got)
	}
}

// TestCaptionWindowExpiryDeliversPending 验证 ②：窗口超时后暂存文本仍要投递。
// 这是「合并窗口」最大的风险点 —— 吞消息比重复回复严重得多。
func TestCaptionWindowExpiryDeliversPending(t *testing.T) {
	c := newTestChannel(t, 80*time.Millisecond)
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)

	c.holdPending(context.Background(), "chat|u1", 80*time.Millisecond, core.Message{ID: "m1", Text: "纯文本没有文件"})
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Text != "纯文本没有文件" {
		t.Fatalf("expired caption not delivered: %+v", got)
	}
}

// TestCaptionWindowIsPerUserPerChat 验证 ③：跨用户/跨群不串。
func TestCaptionWindowIsPerUserPerChat(t *testing.T) {
	c := &WeComChannel{cfg: Config{CaptionWindow: time.Second}}
	c.pend = make(map[string]*pendingCaption)

	c.holdPending(context.Background(), "dm:u1|u1", time.Minute, core.Message{Text: "A 的话"})
	c.holdPending(context.Background(), "dm:u2|u2", time.Minute, core.Message{Text: "B 的话"})

	if got := pendText(c.takePending("dm:u1|u1")); got != "A 的话" {
		t.Fatalf("u1 = %q", got)
	}
	if got := pendText(c.takePending("dm:u2|u2")); got != "B 的话" {
		t.Fatalf("u2 = %q", got)
	}
}

// TestCaptionWindowFlushPending 停机前必须冲刷，否则用户那句配文凭空消失。
func TestCaptionWindowFlushPending(t *testing.T) {
	c := newTestChannel(t, time.Minute)
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)

	c.holdPending(context.Background(), "chat|u1", time.Minute, core.Message{ID: "m1", Text: "待投递"})
	c.flushPending()
	waitDelivered(t, &mu, &got, 1)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Text != "待投递" {
		t.Fatalf("flushPending did not deliver: %+v", got)
	}
	// 冲刷后不应再有残留（Stop 之后定时器再触发也不能重复投递）
	if len(c.pend) != 0 {
		t.Fatalf("pend not cleared after flush: %d", len(c.pend))
	}
}

// TestCaptionWindowSupersedesOldPending 用户连发两句话：旧的那条必须立即放行，
// 不能被新的一条挤掉（否则只回一次）。
func TestCaptionWindowSupersedesOldPending(t *testing.T) {
	c := newTestChannel(t, time.Minute)
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)

	c.holdPending(context.Background(), "chat|u1", time.Minute, core.Message{ID: "m1", Text: "第一句"})
	c.holdPending(context.Background(), "chat|u1", time.Minute, core.Message{ID: "m2", Text: "第二句"})
	waitDelivered(t, &mu, &got, 1)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Text != "第一句" {
		t.Fatalf("old pending should be delivered immediately, got %+v", got)
	}
	if held := pendText(c.takePending("chat|u1")); held != "第二句" {
		t.Fatalf("newest pending = %q, want %q", held, "第二句")
	}
}

// TestCaptionMixedClaimsPendingToo 群聊发图走 mixed，早期版本把 mixed 排除在合并之外，
// 于是「配文 + 图片」照样双回复。现在 mixed 也要认领配文：
//   - 正文里已经内联了同一句 → 认领后丢弃（不拼两遍）
//   - 最终只投递**一条**消息
//
// ⚠️ 媒体项的 url/aeskey 故意留空：fetchMedia 会直接走「缺少解密密钥」占位，
// 不触发真实下载，测试因此不依赖网络。
func TestCaptionMixedClaimsPendingToo(t *testing.T) {
	c := newTestChannel(t, 300*time.Millisecond)
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)

	ctx := context.Background()
	// 先来一句裸文本（会被暂存）。
	c.holdPending(ctx, "wecom:chat:g1|T1", 300*time.Millisecond, core.Message{ID: "t1", Text: "看图"})

	body := &callbackBody{
		MsgID:    "m1",
		ChatType: "group",
		ChatID:   "g1",
		MsgType:  MsgTypeMixed,
		From: struct {
			UserID string `json:"userid"`
		}{UserID: "T1"},
		Mixed: &mixedContent{Items: []mixedItem{
			{MsgType: MsgTypeText, Text: &textContent{Content: "看图"}},
			{MsgType: MsgTypeImage, Image: &mediaContent{}},
		}},
	}
	c.handleMsgCallback(ctx, body)

	// 等过整个窗口：若配文没被认领，它会在窗口到期时单独投递成第二条。
	time.Sleep(600 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1 (caption must be claimed by the mixed message): %+v", len(got), got)
	}
	if strings.Count(got[0].Text, "看图") != 1 {
		t.Fatalf("caption duplicated in body: %q", got[0].Text)
	}
}

// TestHoldPendingConcurrentSameKey 同一 key 并发暂存时，被顶掉的旧消息**每一条**都要投递，
// 不能因为新消息插进来就少一条（早期版本在临界区中间放锁去投递，会静默吞掉一条）。
//
// 用「慢消费者」把 Ingress 的缓冲打满，让 deliver 真的阻塞，从而把并发窗口放大到必现。
func TestHoldPendingConcurrentSameKey(t *testing.T) {
	c := newTestChannel(t, time.Minute)
	var mu sync.Mutex
	var got []core.Message
	ing := newSlowRecordingIngress(t, &mu, &got, 20*time.Millisecond, 1)
	c.ingress = ing

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.holdPending(context.Background(), "chat|u1", time.Minute,
				core.Message{ID: fmt.Sprintf("m%d", i), Text: fmt.Sprintf("第%d句", i)})
		}(i)
	}
	wg.Wait()

	// 前 n-1 条被逐条顶掉并投递，最后一条仍在暂存里。
	waitDelivered(t, &mu, &got, n-1)
	mu.Lock()
	delivered := len(got)
	mu.Unlock()

	held := pendText(c.takePending("chat|u1"))
	if delivered != n-1 || held == "" {
		t.Fatalf("concurrent holdPending lost a message: delivered=%d held=%q, want %d delivered + 1 held",
			delivered, held, n-1)
	}
}

// ---- 测试辅助 ----

func newTestChannel(t *testing.T, w time.Duration) *WeComChannel {
	t.Helper()
	// ⚠️ 必须自己补齐 NewChannel 里初始化的那些 map：直接造结构体的话
	// dedup 是 nil，handleMsgCallback 一走 markSeen 就 panic。
	c := &WeComChannel{cfg: Config{CaptionWindow: w}}
	c.pend = make(map[string]*pendingCaption)
	c.dedup = make(map[string]time.Time)
	return c
}

// waitDelivered 等投递落地。投递是异步的（Ingress 内部 channel + 记录 goroutine），
// 断言前必须等，否则测的是时序而不是逻辑。
func waitDelivered(t *testing.T, mu *sync.Mutex, got *[]core.Message, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		cnt := len(*got)
		mu.Unlock()
		if cnt >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	cnt := len(*got)
	mu.Unlock()
	t.Fatalf("timeout waiting for %d delivered message(s), got %d", n, cnt)
}

// newRecordingIngress 造一个只记录、不跑 pipeline 的 Ingress，
// 并把投递出来的 Envelope 的 Message 收进 got。
func newRecordingIngress(t *testing.T, mu *sync.Mutex, got *[]core.Message) *inbound.Ingress {
	t.Helper()
	return newSlowRecordingIngress(t, mu, got, 0, 16)
}

// newSlowRecordingIngress 同上，但每条消息消费前先睡 delay。
// buffer 取小值时，投递会真的阻塞 —— 用来把并发窗口放大成必现。
func newSlowRecordingIngress(t *testing.T, mu *sync.Mutex, got *[]core.Message, delay time.Duration, buffer int) *inbound.Ingress {
	t.Helper()
	ing := inbound.NewIngress(inbound.IngressConfig{BufferSize: buffer}, zap.NewNop().Sugar(), noop.NewTracerProvider())
	go func() {
		for env := range ing.C() {
			if env == nil {
				continue
			}
			if delay > 0 {
				time.Sleep(delay)
			}
			mu.Lock()
			*got = append(*got, env.Message)
			mu.Unlock()
		}
	}()
	return ing
}

func pendText(m *core.Message) string {
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m.Text)
}

// TestHandleMsgCallback_DeclaresLinkedMemoryChannel 覆盖跨会话召回的写入侧声明：
// 群里发言的人，其私聊（wecom:dm:<userid>）要被声明为记忆同源会话，
// 否则私聊沉淀的长期记忆在群里永远调不出来。私聊本身不需要声明（它自己就在 scopes 里）。
func TestHandleMsgCallback_DeclaresLinkedMemoryChannel(t *testing.T) {
	ctx := context.Background()

	// 群聊：应声明发言者本人的私聊
	c := newTestChannel(t, 10*time.Millisecond)
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)
	c.handleMsgCallback(ctx, &callbackBody{
		MsgID:    "m-group",
		ChatType: "group",
		ChatID:   "g1",
		MsgType:  MsgTypeText,
		From: struct {
			UserID string `json:"userid"`
		}{UserID: "U1"},
		Text: &textContent{Content: "群里问个问题"},
	})
	waitDelivered(t, &mu, &got, 1)
	mu.Lock()
	msg := got[0]
	mu.Unlock()

	if msg.Channel != PrefixChat+"g1" {
		t.Fatalf("channel = %q", msg.Channel)
	}
	raw, ok := msg.Metadata[core.MetaMemoryLinkedChannels]
	if !ok {
		t.Fatalf("group message must declare %s, metadata=%+v", core.MetaMemoryLinkedChannels, msg.Metadata)
	}
	ids, ok := raw.([]string)
	if !ok || len(ids) != 1 || ids[0] != PrefixDM+"U1" {
		t.Fatalf("linked channels = %#v, want [%s]", raw, PrefixDM+"U1")
	}

	// 私聊：不需要声明（当前会话本身就在召回作用域里）
	var mu2 sync.Mutex
	var got2 []core.Message
	c2 := newTestChannel(t, 10*time.Millisecond)
	c2.ingress = newRecordingIngress(t, &mu2, &got2)
	c2.handleMsgCallback(ctx, &callbackBody{
		MsgID:    "m-dm",
		ChatType: "single",
		MsgType:  MsgTypeText,
		From: struct {
			UserID string `json:"userid"`
		}{UserID: "U1"},
		Text: &textContent{Content: "私聊问个问题"},
	})
	waitDelivered(t, &mu2, &got2, 1)
	mu2.Lock()
	msg2 := got2[0]
	mu2.Unlock()
	if msg2.Channel != PrefixDM+"U1" {
		t.Fatalf("dm channel = %q", msg2.Channel)
	}
	if _, ok := msg2.Metadata[core.MetaMemoryLinkedChannels]; ok {
		t.Fatalf("dm must not declare linked channels, metadata=%+v", msg2.Metadata)
	}
}
