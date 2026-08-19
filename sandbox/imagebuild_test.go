package sandbox

import (
	"strings"
	"testing"

	botsandbox "github.com/kasuganosora/thinkbot/docker/sandbox"
)

func TestIsBuiltinImage(t *testing.T) {
	cases := map[string]bool{
		"":                              true,
		botsandbox.BuiltinImageSentinel: true,
		"alpine:latest":                 false,
		"registry/thinkbot-bot:latest":  false,
		"thinkbot-bot:abc123":           false,
	}
	for in, want := range cases {
		if got := isBuiltinImage(in); got != want {
			t.Errorf("isBuiltinImage(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuiltinTagDeterministic(t *testing.T) {
	// tag 必须稳定且格式为 thinkbot-bot:<32 hex md5>，确保「构建上下文不变 → 不重复构建」。
	tag1, err := botsandbox.EnsureTag()
	if err != nil {
		t.Fatalf("builtin tag: %v", err)
	}
	tag2, err := botsandbox.EnsureTag()
	if err != nil {
		t.Fatalf("builtin tag: %v", err)
	}
	if tag1 != tag2 {
		t.Fatalf("tag not deterministic: %q != %q", tag1, tag2)
	}
	if !strings.HasPrefix(tag1, "thinkbot-bot:") {
		t.Fatalf("unexpected tag prefix: %q", tag1)
	}
	if len(tag1) != len("thinkbot-bot:")+32 {
		t.Fatalf("tag should be repo:32-hex-md5, got %q (len %d)", tag1, len(tag1))
	}
}
