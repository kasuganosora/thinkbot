package mcp

import (
	"strings"
	"testing"
)

func TestStderrRingKeepsTailAndRedacts(t *testing.T) {
	const secret = "super-secret-token"
	r := newStderrRing(80, []string{secret})
	if _, err := r.Write([]byte("HEAD-" + strings.Repeat("A", 400))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("END password=" + secret + "\n")); err != nil {
		t.Fatal(err)
	}
	got := r.Tail()
	if strings.Contains(got, secret) || strings.Contains(got, "HEAD-") {
		t.Fatalf("tail leaked secret or the dropped head: %q", got)
	}
	if !strings.Contains(got, "END") || !strings.Contains(got, "password=***") {
		t.Fatalf("tail = %q", got)
	}
	if len(got) > 80 {
		t.Fatalf("tail length = %d, cap 80: %q", len(got), got)
	}
}

func TestRedactStderrPatternsWithoutExplicitList(t *testing.T) {
	got := RedactStderr("token=abcdef0123456789 Bearer abcdef0123456789 http://user:abcdef0123456789@example.test", nil)
	if strings.Contains(got, "abcdef0123456789") {
		t.Fatalf("pattern redact missed a secret: %q", got)
	}
	if !strings.Contains(got, "token=***") || !strings.Contains(got, "Bearer ***") || !strings.Contains(got, "http://user:***@example.test") {
		t.Fatalf("got %q", got)
	}
}
