package http

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
)

const fakeTGToken = "123456789:AAFakeTokenForTestsOnly_abcdefghijklmn"

func TestSanitizeError_URLError(t *testing.T) {
	raw := &url.Error{Op: "Post", URL: "https://api.telegram.org/bot" + fakeTGToken + "/getUpdates", Err: context.Canceled}
	got := SanitizeError(raw)
	if strings.Contains(got.Error(), "AAFakeToken") {
		t.Fatalf("token still in error: %s", got)
	}
	if !strings.Contains(got.Error(), "/bot***/getUpdates") || !errors.Is(got, context.Canceled) {
		t.Fatalf("sanitized error lost information: %v", got)
	}
	var ue *url.Error
	if !errors.As(got, &ue) || ue.Op != "Post" {
		t.Fatalf("must stay a *url.Error: %T", got)
	}
	// Generic errors carrying a token are wrapped, chain preserved.
	wrapped := SanitizeError(&wrapErr{msg: "boom bot" + fakeTGToken, cause: context.DeadlineExceeded})
	if strings.Contains(wrapped.Error(), "AAFakeToken") || !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatalf("generic sanitize: %v", wrapped)
	}
	plain := errors.New("nothing secret here")
	if SanitizeError(plain) != plain {
		t.Fatal("clean errors must be returned as-is")
	}
	if SanitizeError(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

// TestDoOnce_TransportErrorDoesNotLeakToken reproduces the production leak:
// a transport failure against a Telegram-style base URL.
func TestDoOnce_TransportErrorDoesNotLeakToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens → connection refused
	c := New(WithBaseURL("http://" + addr + "/bot" + fakeTGToken))
	_, err = c.Post("getUpdates").SetContext(context.Background()).Do()
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "AAFakeToken") {
		t.Fatalf("token leaked in returned error: %v", err)
	}
	if !strings.Contains(err.Error(), "bot***") {
		t.Fatalf("expected sanitized URL in error, got %v", err)
	}
}

type wrapErr struct {
	msg   string
	cause error
}

func (e *wrapErr) Error() string { return e.msg }
func (e *wrapErr) Unwrap() error { return e.cause }
