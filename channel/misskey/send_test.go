package misskey

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestSendAddsRemoteHostToMention 验证修复：outbound Send 回复远程（联邦）用户时，
// 必须自动补全 @username@host，否则 maid.lat 会把 @username 解析成本实例同名用户，远程用户收不到。
// 复现 2026-08-12 的 bug：bot 回复 noyaoinolife@wxw.moe 时发出的是裸 @noyaoinolife。
func TestSendAddsRemoteHostToMention(t *testing.T) {
	var gotText string
	var sawNotesShow bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/notes/show"):
			sawNotesShow = true
			// 模拟一条远程用户（wxw.moe）的原帖
			_, _ = w.Write([]byte(`{"id":"remote123","text":"hi @kanna","user":{"username":"noyaoinolife","host":"wxw.moe"}}`))
		case strings.HasSuffix(r.URL.Path, "/notes/create"):
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			gotText, _ = req["text"].(string)
			_, _ = w.Write([]byte(`{"createdNote":{"id":"new123"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	c := &MisskeyChannel{
		name:        "Misskey",
		botUsername: "kanna",
		api:         newAPIClient(ts.URL, "tok"),
	}

	action := core.Action{
		Type:    core.ActionReply,
		Channel: "remote123",
		Payload: "你好呀，谢谢分享",
	}
	if err := c.Send(context.Background(), action); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if !sawNotesShow {
		t.Fatalf("Send did not call notes/show to resolve mention host")
	}
	if !strings.HasPrefix(gotText, "@noyaoinolife@wxw.moe") {
		t.Fatalf("expected remote @host prefix in sent text, got: %q", gotText)
	}
	t.Logf("sent text = %q", gotText)
}
