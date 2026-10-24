package mastodon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/inbound"
)

// 2026-10-24 生产事故：status 18391 在同一天进入 LLM 四次
// （16:38 首次，17:22 / 17:41 / 18:23 三次都跟着重连）。
// 订阅是 user:notification + user + public:local + direct。
// ingress 只按 msg.ID 记 2 分钟，挡得住「刚好同时到」的副本，
// 挡不住几十分钟后的重连补发；而且裸 ID 会把收藏通知和帖子撞在一起。

func testMastodonIngress(t *testing.T) *inbound.Ingress {
	t.Helper()
	ing := inbound.NewIngress(
		inbound.IngressConfig{BufferSize: 8},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)
	t.Cleanup(ing.Close)
	return ing
}

func mustStreamUpdate(t *testing.T, stream, statusID, content string) string {
	t.Helper()
	payload, err := json.Marshal(Status{
		ID:         statusID,
		CreatedAt:  "2026-10-24T08:38:00Z",
		Content:    "<p>" + content + "</p>",
		Visibility: VisibilityPublic,
		Account:    Account{ID: "acct-other", Username: "sion", Acct: "sion"},
	})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	ev, err := json.Marshal(streamEvent{
		Stream:  []string{stream},
		Event:   "update",
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(ev)
}

func ageDedup(c *MastodonChannel, d time.Duration) {
	c.dedupMu.Lock()
	defer c.dedupMu.Unlock()
	past := time.Now().Add(-d)
	for id := range c.dedup {
		c.dedup[id] = past
	}
}

// TestSameStatusOnTwoStreamsProcessedOnce：同一条帖出现在 user 和 public:local
// （以及 direct、通知流 mention）时只进一次 LLM 队列。
func TestSameStatusOnTwoStreamsProcessedOnce(t *testing.T) {
	c := newTestChannel()
	ing := testMastodonIngress(t)
	c.ingress = ing
	ctx := context.Background()

	for _, stream := range []string{"user", "public:local", "direct"} {
		if err := c.handleStreamMessage(ctx, mustStreamUpdate(t, stream, "18391", "hello")); err != nil {
			t.Fatalf("stream %s: %v", stream, err)
		}
	}
	c.handleNotification(ctx, &Notification{
		ID:        "555",
		Type:      "mention",
		CreatedAt: "2026-10-24T08:38:00Z",
		Account:   Account{ID: "acct-other", Username: "sion", Acct: "sion"},
		Status: &Status{
			ID: "18391", CreatedAt: "2026-10-24T08:38:00Z",
			Content: "<p>hello</p>", Visibility: VisibilityPublic,
			Account: Account{ID: "acct-other", Username: "sion", Acct: "sion"},
		},
	})

	if got := ing.Len(); got != 1 {
		t.Fatalf("status 18391 arrived on user, public:local, direct and mention; ingress=%d want 1", got)
	}
}

// TestReconnectBackfillDropsAlreadySeenStatus：重连补发在断连几十分钟后
// 仍会把已见过的 status 拉回来。2 分钟去重窗口挡不住，必须丢掉。
func TestReconnectBackfillDropsAlreadySeenStatus(t *testing.T) {
	c := newTestChannel()
	live := testMastodonIngress(t)
	c.ingress = live
	ctx := context.Background()

	if err := c.handleStreamMessage(ctx, mustStreamUpdate(t, "public:local", "18391", "hello")); err != nil {
		t.Fatal(err)
	}
	if got := live.Len(); got != 1 {
		t.Fatalf("live ingest ingress=%d want 1", got)
	}

	// 16:38 → 17:22，远超渠道和 ingress 各自的 2 分钟窗口。
	// 换成一个没见过这条 msg.ID 的 ingress，等价于 ingress 去重已经过期，
	// 这样失败就落在渠道去重上，而不是被 ingress 提前挡住。
	ageDedup(c, 45*time.Minute)
	c.ingress = testMastodonIngress(t)
	c.setNotificationAnchor("100")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/notifications" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id":"200",
			"type":"mention",
			"created_at":"2026-10-24T08:38:00Z",
			"account":{"id":"acct-other","username":"sion","acct":"sion"},
			"status":{
				"id":"18391",
				"created_at":"2026-10-24T08:38:00Z",
				"content":"<p>hello</p>",
				"visibility":"public",
				"account":{"id":"acct-other","username":"sion","acct":"sion"}
			}
		}]`))
	}))
	t.Cleanup(srv.Close)
	c.api = newAPIClient(srv.URL, "token")

	c.backfillMentions(ctx)
	if got := c.ingress.Len(); got != 0 {
		t.Fatalf("reconnect backfill re-queued status 18391, ingress=%d want 0", got)
	}
	if got := c.getNotificationAnchor(); got != "200" {
		t.Fatalf("anchor=%q want 200 (dropped catch-up must still advance the anchor)", got)
	}
}

// TestFavouriteNotCollapsedWithStatusOfSameID：去重键必须带事件类型。
// 小实例的 status id 和 notification id 都是短整数，裸 ID 会把收藏通知吞掉。
func TestFavouriteNotCollapsedWithStatusOfSameID(t *testing.T) {
	c := newTestChannel()
	ing := testMastodonIngress(t)
	c.ingress = ing
	ctx := context.Background()

	if err := c.handleStreamMessage(ctx, mustStreamUpdate(t, "user", "18391", "hello")); err != nil {
		t.Fatal(err)
	}
	c.handleNotification(ctx, &Notification{
		ID:        "18391",
		Type:      "favourite",
		CreatedAt: "2026-10-24T08:40:00Z",
		Account:   Account{ID: "acct-other", Username: "sion", Acct: "sion", DisplayName: "Sion"},
		Status: &Status{
			ID: "bot-post", Content: "<p>earlier</p>",
			Account: Account{ID: "self", Username: "bot"},
		},
	})

	if got := ing.Len(); got != 2 {
		t.Fatalf("favourite whose notification id equals a seen status id was dropped, ingress=%d want 2", got)
	}
}
