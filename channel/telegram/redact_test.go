package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const fakeTGToken = "123456789:AAFakeTokenForTestsOnly_abcdefghijklmn"

func TestWrapErr_RedactsToken(t *testing.T) {
	a := &apiClient{token: fakeTGToken}
	cause := &plainErr{msg: "Post \"https://proxy.example/bot" + fakeTGToken + "/getUpdates\": context canceled (token " + fakeTGToken + ")", cause: context.Canceled}
	err := a.wrapErr(cause, "telegram getUpdates")
	if strings.Contains(err.Error(), "AAFakeToken") {
		t.Fatalf("token leaked: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "telegram getUpdates: ") || !errors.Is(err, context.Canceled) {
		t.Fatalf("wrap lost context: %v", err)
	}
	if a.wrapErr(nil, "x") != nil {
		t.Fatal("nil stays nil")
	}
}

type plainErr struct {
	msg   string
	cause error
}

func (e *plainErr) Error() string { return e.msg }
func (e *plainErr) Unwrap() error { return e.cause }
