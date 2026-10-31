package api

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/secret"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatSecretFormOwnsValueWithoutChatFallback(t *testing.T) {
	v, _ := secret.New("")
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
	const raw = "fictional-private-card-credential"
	register := func(id string, timeout int) {
		t.Helper()
		_, err := interaction.Default().RegisterQuestion(interaction.Question{ID: id, BotID: "bot", ChatID: interaction.SecretReplyChatID("web:42", "session"), Question: "test", Kind: interaction.KindSecret, SecretForm: true, SecretName: id, TimeoutSecs: timeout})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { interaction.Default().Cancel(id); interaction.Default().CleanupFinal(id) })
	}
	call := func(id, bot, session string, user uint, cancel bool) *httptest.ResponseRecorder {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"botId": bot, "sessionId": session, "value": raw, "cancel": cancel})
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(string(payload)))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "questionId", Value: id}}
		if user > 0 {
			c.Set(ContextKeyUser, &dao.User{ID: user})
		}
		// No chat history or bot service is provided: this path must never write or inject raw text.
		(&Server{}).handleSubmitChatSecret(c)
		if strings.Contains(rec.Body.String(), raw) {
			t.Fatal("credential reflected in response")
		}
		return rec
	}
	register("form-owner", 300)
	for _, tc := range []struct {
		bot, session string
		user         uint
	}{{"bot", "session", 0}, {"other", "session", 42}, {"bot", "other", 42}, {"bot", "session", 43}} {
		if r := call("form-owner", tc.bot, tc.session, tc.user, false); r.Code < 400 {
			t.Fatalf("unauthorized form accepted: %v", tc)
		}
	}
	r := call("form-owner", "bot", "session", 42, false)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"stored"`) || !strings.Contains(r.Body.String(), `"userText"`) {
		t.Fatalf("bad response %d %s", r.Code, r.Body)
	}
	if got, _ := v.Get("bot", "form-owner"); got != raw {
		t.Fatal("value not registered")
	}
	if r := call("form-owner", "bot", "session", 42, false); r.Code != 404 {
		t.Fatal("request reused")
	}
	// Simulates a persisted UI card whose registry state disappeared on restart.
	if r := call("missing-after-restart", "bot", "session", 42, false); r.Code != 404 {
		t.Fatal("unknown request accepted")
	}
	register("form-cancel", 300)
	if r := call("form-cancel", "bot", "session", 42, true); r.Code != 200 {
		t.Fatal("cancel failed")
	}
	if _, ok := v.Get("bot", "form-cancel"); ok {
		t.Fatal("cancel stored credential")
	}
	register("form-expired", 1)
	time.Sleep(1100 * time.Millisecond)
	if r := call("form-expired", "bot", "session", 42, false); r.Code != 409 {
		t.Fatalf("expired accepted: %d", r.Code)
	}
	if _, ok := v.Get("bot", "form-expired"); ok {
		t.Fatal("expired value stored")
	}
}
