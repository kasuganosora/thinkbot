package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSanitizeTelegramHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"a < b and c > d", "a &lt; b and c &gt; d"},
		{"I <3 you", "I &lt;3 you"},
		{"R&D &amp; more", "R&amp;D &amp; more"},
		{"<b>bold</b> <i>it</i> <code>x<y</code>", "<b>bold</b> <i>it</i> <code>x&lt;y</code>"},
		{`<a href="https://e.x/?a=1">link</a>`, `<a href="https://e.x/?a=1">link</a>`},
		{"<long>wrapped</long>", "&lt;long&gt;wrapped&lt;/long&gt;"},
		{"<>", "&lt;&gt;"},
		{"5 &lt; 6 &#60; &#x3C;", "5 &lt; 6 &#60; &#x3C;"},
		{"中文<中文>", "中文&lt;中文&gt;"},
		{"<bogus attr>", "&lt;bogus attr&gt;"},
		{"<b", "&lt;b"},
	}
	for _, c := range cases {
		if got := sanitizeTelegramHTML(c.in); got != c.want {
			t.Errorf("sanitizeTelegramHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTelegramHTMLToPlain(t *testing.T) {
	in := sanitizeTelegramHTML("<b>注意</b>: a < b & <i>c</i>")
	if got, want := telegramHTMLToPlain(in), "注意: a < b & c"; got != want {
		t.Fatalf("telegramHTMLToPlain = %q, want %q", got, want)
	}
}

func TestIsParseEntitiesError(t *testing.T) {
	// Exact error text from prod 2026-09-27 18:45:37.
	prod := errors.New(`telegram sendMessage: http POST https://api.telegram.org/bot***/sendMessage -> 400: {"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Unsupported start tag \"\" at byte offset 194"}`)
	if !isParseEntitiesError(prod) {
		t.Fatal("prod parse error not recognized")
	}
	for _, e := range []error{
		nil,
		errors.New(`-> 400: {"description":"Bad Request: chat not found"}`),
		errors.New("context deadline exceeded"),
		errors.New(`-> 429: {"description":"Too Many Requests: retry after 3"}`),
	} {
		if isParseEntitiesError(e) {
			t.Errorf("isParseEntitiesError(%v) = true", e)
		}
	}
}

type recordedSend struct {
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

// newParseErrorServer fakes sendMessage/editMessageText: any request whose
// HTML text contains rejectMarker fails with Telegram's parse error.
func newParseErrorServer(t *testing.T, rejectMarker string) (*httptest.Server, func() []recordedSend) {
	t.Helper()
	var mu sync.Mutex
	var got []recordedSend
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req recordedSend
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		if req.ParseMode == "HTML" && (strings.Contains(req.Text, rejectMarker) || strings.Contains(req.Text, "<3")) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Unsupported start tag \"\" at byte offset 7"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedSend {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedSend(nil), got...)
	}
}

func newFormatTestChannel(srv *httptest.Server) *TelegramChannel {
	api := newAPIClient("TEST", 0, srv.URL)
	api.sendInterval = 0
	return &TelegramChannel{api: api, cfg: Config{ParseMode: "HTML"}}
}

// A bare "<" is escaped before sending, so the first attempt already succeeds.
func TestReply_HTMLBareLessThanIsEscaped(t *testing.T) {
	srv, got := newParseErrorServer(t, "\x00never")
	ch := newFormatTestChannel(srv)
	if err := ch.Reply(context.Background(), 1, "I <3 you, a<b", 0); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	sends := got()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want 1: %+v", len(sends), sends)
	}
	if sends[0].ParseMode != "HTML" || sends[0].Text != "I &lt;3 you, a&lt;b" {
		t.Fatalf("sent %+v", sends[0])
	}
}

// If Telegram still rejects the formatting (here: an unbalanced supported
// tag), the message is resent once as plain text instead of being lost.
func TestReply_ParseErrorFallsBackToPlainText(t *testing.T) {
	srv, got := newParseErrorServer(t, "<b>")
	ch := newFormatTestChannel(srv)
	if err := ch.Reply(context.Background(), 1, "<b>unclosed & a<b", 0); err != nil {
		t.Fatalf("Reply should succeed via plain-text fallback: %v", err)
	}
	sends := got()
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want 2: %+v", len(sends), sends)
	}
	if sends[1].ParseMode != "" {
		t.Fatalf("fallback parse_mode = %q, want empty", sends[1].ParseMode)
	}
	if sends[1].Text != "unclosed & a<b" {
		t.Fatalf("fallback text = %q", sends[1].Text)
	}

	// editMessageText gets the same treatment.
	if err := ch.EditMessage(context.Background(), 1, 7, "<b>edit"); err != nil {
		t.Fatalf("EditMessage should succeed via fallback: %v", err)
	}
	sends = got()
	if last := sends[len(sends)-1]; last.ParseMode != "" || last.Text != "edit" {
		t.Fatalf("edit fallback sent %+v", last)
	}
}

// Other 400s are not retried and still surface as errors.
func TestReply_OtherErrorsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()
	ch := newFormatTestChannel(srv)
	if err := ch.Reply(context.Background(), 1, "hi", 0); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
