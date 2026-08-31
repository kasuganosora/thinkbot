package errs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/util/log"
)

func TestMain(m *testing.M) {
	_ = log.Init()
	m.Run()
}

// --- 基本构造 ---

func TestNew(t *testing.T) {
	e := New("something failed")
	if e.Error() != "something failed" {
		t.Errorf("unexpected message: %q", e.Error())
	}
	if e.StackTrace() == "" {
		t.Error("expected non-empty stack trace")
	}
	if e.Unwrap() != nil {
		t.Error("expected nil cause")
	}
}

func TestNewf(t *testing.T) {
	e := Newf("user %d not found", 42)
	if e.Error() != "user 42 not found" {
		t.Errorf("got %q", e.Error())
	}
}

func TestWrap(t *testing.T) {
	base := errors.New("db connection refused")
	e := Wrap(base, "failed to save user")

	if e.Error() != "failed to save user: db connection refused" {
		t.Errorf("got %q", e.Error())
	}
	if !errors.Is(e, base) {
		t.Error("errors.Is should match base")
	}
	if e.StackTrace() == "" {
		t.Error("expected stack trace")
	}
}

func TestWrapf(t *testing.T) {
	base := errors.New("timeout")
	e := Wrapf(base, "query %s failed", "SELECT 1")
	want := "query SELECT 1 failed: timeout"
	if e.Error() != want {
		t.Errorf("got %q, want %q", e.Error(), want)
	}
}

func TestWrapNil(t *testing.T) {
	if Wrap(nil, "msg") != nil {
		t.Error("Wrap(nil, ...) should return nil")
	}
	if Wrapf(nil, "msg %d", 1) != nil {
		t.Error("Wrapf(nil, ...) should return nil")
	}
}

// --- Cause / Is / As ---

func TestCause(t *testing.T) {
	base := errors.New("root")
	e := Wrap(base, "layer1")
	e2 := Wrap(e, "layer2")

	if Cause(e2) != base {
		t.Error("Cause should return root error")
	}
}

func TestIs(t *testing.T) {
	custom := New("custom")
	e := Wrap(custom, "wrapper")
	if !Is(e, custom) {
		t.Error("Is should find custom in chain")
	}
}

func TestAs(t *testing.T) {
	e := Wrap(New("inner"), "outer")
	var target *Error
	if !As(e, &target) {
		t.Fatal("As should match *Error")
	}
	if target.message != "outer" {
		t.Errorf("got message %q", target.message)
	}
}

// --- HTTP 错误 ---

func TestHTTPError(t *testing.T) {
	e := HTTPError(http.StatusNotFound, "user not found")
	if e.Code() != http.StatusNotFound {
		t.Errorf("expected 404, got %d", e.Code())
	}
	if e.Error() != "user not found" {
		t.Errorf("got %q", e.Error())
	}
}

func TestHTTPErrorDefaultMessage(t *testing.T) {
	e := HTTPError(http.StatusBadRequest, "")
	if !strings.Contains(e.Error(), "Bad Request") {
		t.Errorf("expected default message, got %q", e.Error())
	}
}

func TestHTTPErrorf(t *testing.T) {
	e := HTTPErrorf(http.StatusInternalServerError, "internal %s", "failure")
	if e.Code() != 500 {
		t.Errorf("expected 500, got %d", e.Code())
	}
	if e.Error() != "internal failure" {
		t.Errorf("got %q", e.Error())
	}
}

func TestBadRequest(t *testing.T) {
	e := BadRequest("invalid input")
	if e.Code() != 400 {
		t.Errorf("expected 400, got %d", e.Code())
	}
}

func TestNotFound(t *testing.T) {
	e := NotFound("missing")
	if e.Code() != 404 {
		t.Errorf("expected 404, got %d", e.Code())
	}
}

func TestInternal(t *testing.T) {
	e := Internal("oops")
	if e.Code() != 500 {
		t.Errorf("expected 500, got %d", e.Code())
	}
}

func TestUnauthorized(t *testing.T) {
	if Unauthorized("").Code() != 401 {
		t.Error("expected 401")
	}
}

func TestForbidden(t *testing.T) {
	if Forbidden("").Code() != 403 {
		t.Error("expected 403")
	}
}

func TestConflict(t *testing.T) {
	if Conflict("").Code() != 409 {
		t.Error("expected 409")
	}
}

func TestServiceUnavailable(t *testing.T) {
	if ServiceUnavailable("").Code() != 503 {
		t.Error("expected 503")
	}
}

// --- GetCode / GetStackTrace ---

func TestGetCode(t *testing.T) {
	e := NotFound("test")
	wrapped := Wrap(e, "outer")

	if got := GetCode(wrapped); got != 404 {
		t.Errorf("expected 404, got %d", got)
	}
}

func TestGetCodeNoCode(t *testing.T) {
	e := errors.New("plain error")
	if GetCode(e) != 0 {
		t.Error("expected 0 for plain error")
	}
}

func TestGetStackTrace(t *testing.T) {
	e := New("test")
	wrapped := Wrap(e, "outer")
	stack := GetStackTrace(wrapped)

	if stack == "" {
		t.Error("expected non-empty stack")
	}
	if !strings.Contains(stack, "TestGetStackTrace") {
		t.Errorf("stack should contain test function, got:\n%s", stack)
	}
}

// --- With / WithCode ---

func TestWith(t *testing.T) {
	e := New("base").With("user_id", 123).With("action", "create")
	ctxFields := e.Context()

	if len(ctxFields) != 2 {
		t.Fatalf("expected 2 context fields, got %d", len(ctxFields))
	}
	if ctxFields[0].key != "user_id" || ctxFields[0].value != 123 {
		t.Errorf("unexpected field: %+v", ctxFields[0])
	}
}

func TestWithCode(t *testing.T) {
	e := New("base").WithCode(http.StatusForbidden)
	if e.Code() != 403 {
		t.Errorf("expected 403, got %d", e.Code())
	}
}

func TestWithImmutable(t *testing.T) {
	e := New("base")
	e2 := e.With("key", "val")

	// 原始对象不应被修改
	if len(e.Context()) != 0 {
		t.Error("original should be unmodified")
	}
	if len(e2.Context()) != 1 {
		t.Error("new instance should have field")
	}
}

// --- 堆栈 ---

func TestStackTraceFormat(t *testing.T) {
	e := New("test")
	stack := e.StackTrace()

	if !strings.Contains(stack, ".go:") {
		t.Errorf("stack should contain file:line, got:\n%s", stack)
	}
}

// --- 日志 ---

func TestLogNoPanic(t *testing.T) {
	// 不应 panic
	Log(nil)
	Log(New("test error"))
	Log(NotFound("not found"))
	Log(Internal("server error"))
}

func TestLogAndReturn(t *testing.T) {
	e := New("test")
	got := LogAndReturn(e)
	if got != e {
		t.Error("should return same error")
	}
}

func TestLogWithFields(t *testing.T) {
	e := New("test").With("key", "val")
	LogWith(context.Background(), e, "extra", "data")
}

// --- 链式用法 ---

func TestChainUsage(t *testing.T) {
	err := fmt.Errorf("connection refused")
	e := Wrap(err, "db error").
		WithCode(http.StatusServiceUnavailable).
		With("db", "postgres").
		With("query", "SELECT 1")

	if e.Code() != 503 {
		t.Errorf("expected 503, got %d", e.Code())
	}
	if !strings.Contains(e.Error(), "connection refused") {
		t.Error("should contain base error")
	}
	if len(e.Context()) != 2 {
		t.Errorf("expected 2 context fields, got %d", len(e.Context()))
	}
}
