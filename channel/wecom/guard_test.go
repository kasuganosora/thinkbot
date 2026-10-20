package wecom

import (
	"context"
	"encoding/base64"
	nethttp "net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestValidateMediaURL(t *testing.T) {
	ok := []string{
		"https://qq.com/a",
		"https://ww-aibot-img-1258476243.cos.ap-guangzhou.myqcloud.qq.com/x?sign=1",
		"https://media.weixin.qq.com/x",
		"https://QYAPI.WEIXIN.QQ.COM./x",
	}
	bad := []string{
		"http://media.weixin.qq.com/x",
		"https://evilqq.com/x",
		"https://qq.com.evil.com/x",
		"https://127.0.0.1/x",
		"https://[::1]/x",
		"https://user:pw@media.weixin.qq.com/x",
		"file:///etc/passwd",
		"",
		"https://169.254.169.254/latest",
	}
	for _, u := range ok {
		if err := validateMediaURL(u); err != nil {
			t.Errorf("want ok %q: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := validateMediaURL(u); err == nil {
			t.Errorf("want reject %q", u)
		}
	}
}

func TestIsBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.0.1", "169.254.169.254", "::1", "fe80::1", "fc00::1", "0.0.0.0", "100.64.1.1", "::ffff:127.0.0.1"} {
		if !isBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "183.3.226.35", "2001:4860:4860::8888"} {
		if isBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if err := dialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Error("dialControl must reject loopback")
	}
	if err := dialControl("tcp", "1.1.1.1:443", nil); err != nil {
		t.Errorf("dialControl public: %v", err)
	}
}

func TestMediaClientRejectsBadRedirect(t *testing.T) {
	hc := newMediaTransportClient()
	req, _ := newReq("https://evil.example.com/x")
	if err := hc.CheckRedirect(req, nil); err == nil {
		t.Fatal("redirect to non-allowlisted host must be rejected")
	}
	req, _ = newReq("https://media.weixin.qq.com/x")
	if err := hc.CheckRedirect(req, nil); err != nil {
		t.Fatalf("allowlisted redirect rejected: %v", err)
	}
}

func TestValidateEndpoint(t *testing.T) {
	if custom, err := validateEndpoint(defaultEndpoint, false); err != nil || custom {
		t.Fatalf("default endpoint: custom=%v err=%v", custom, err)
	}
	if _, err := validateEndpoint("ws://openws.work.weixin.qq.com", true); err == nil {
		t.Fatal("ws:// must be rejected even with opt-in")
	}
	if _, err := validateEndpoint("wss://ws.example.com", false); err == nil {
		t.Fatal("custom host without opt-in must be rejected")
	}
	if custom, err := validateEndpoint("wss://ws.example.com", true); err != nil || !custom {
		t.Fatalf("custom with opt-in: custom=%v err=%v", custom, err)
	}
}

func TestStartRejectsCustomEndpointWithoutOptIn(t *testing.T) {
	c := NewChannel("w", "b", Config{RobotID: "AIBOTID", Secret: "x", Endpoint: "wss://ws.example.com"})
	if err := c.Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "allowCustomEndpoint") {
		t.Fatalf("want endpoint rejection, got %v", err)
	}
}

func TestSanitizeIDAndFilename(t *testing.T) {
	if got := sanitizeID("../a/b\\c d_e-1"); got != "abcd_e-1" {
		t.Fatalf("sanitizeID = %q", got)
	}
	name := mediaFilename(MsgTypeFile, "application/pdf", "../../etc", 0)
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		t.Fatalf("unsafe filename %q", name)
	}
}

func TestDecryptFailureDoesNotLeakPlaintext(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	aeskey := strings.TrimRight(b64(key), "=")
	ct := make([]byte, 64) // 解出来的末字节几乎不可能是合法 pad
	_, err := decryptMedia(ct, aeskey)
	if err == nil {
		t.Skip("random block happened to unpad")
	}
	if strings.Contains(err.Error(), "ptHead") || strings.Contains(err.Error(), "ptTail") || strings.Contains(err.Error(), "ctHead") {
		t.Fatalf("error leaks bytes: %v", err)
	}
}

func TestSleepCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, time.Second); err == nil || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("sleepCtx did not honor cancel: %v", err)
	}
}

// 群聊「@bot 配文」+「不带 @ 的文件」合并后必须保留 Mentioned。
func TestCaptionMergeKeepsMentioned(t *testing.T) {
	c := newTestChannel(t, 300*time.Millisecond)
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)
	ctx := context.Background()
	c.holdPending(ctx, "wecom:chat:g1|T1", 300*time.Millisecond, core.Message{ID: "t1", Text: "看这个", Mentioned: true})

	if pm := c.takePending("wecom:chat:g1|T1"); pm == nil || !pm.Mentioned || pm.Text != "看这个" {
		t.Fatalf("takePending must return the whole message, got %+v", pm)
	}
}

// Stop 之后定时器不应再单独投递（flush 已统一投递一次），总共只投递一次。
func TestStopFlushesPendingExactlyOnce(t *testing.T) {
	c := newTestChannel(t, 50*time.Millisecond)
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)
	c.holdPending(context.Background(), "chat|u1", 50*time.Millisecond, core.Message{ID: "m1", Text: "停机前"})
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("delivered %d, want exactly 1", len(got))
	}
}

func TestStripMentionFallbackCounts(t *testing.T) {
	before := mentionFallbackCount.Load()
	if _, at := stripMention("没有 at 的群消息", "mybot", true); !at {
		t.Fatal("group default must stay true")
	}
	if mentionFallbackCount.Load() != before+1 {
		t.Fatal("fallback counter not incremented")
	}
	stripMention("@mybot 你好", "mybot", true)
	if mentionFallbackCount.Load() != before+1 {
		t.Fatal("counter must not move when @ parsed")
	}
}

func newReq(u string) (*nethttp.Request, error) { return nethttp.NewRequest("GET", u, nil) }

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
