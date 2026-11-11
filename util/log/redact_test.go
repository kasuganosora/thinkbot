package log

import (
	"bytes"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// fakeTGToken is a syntactically valid but fake Telegram bot token.
const fakeTGToken = "123456789:AAFakeTokenForTestsOnly_abcdefghijklmn"

func TestRedactSecrets_Patterns(t *testing.T) {
	cases := []struct{ in, mustNot, want string }{
		{`Post "https://api.telegram.org/bot` + fakeTGToken + `/getUpdates": context canceled`, "AAFakeToken", "bot***"},
		{"token is " + fakeTGToken, "AAFakeToken", "***"},
		{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123", "abcdefghijklmnop", "Bearer ***"},
		{"GET https://mk.example/api/notes?i=SeCrEtMiSsKeYtOkEn&limit=3", "SeCrEt", "?i=***&limit=3"},
		{"GET https://generativelanguage.googleapis.com/v1/models?key=AIzaFakeKeyValue123", "AIzaFake", "?key=***"},
	}
	for _, c := range cases {
		got := RedactSecrets(c.in)
		if strings.Contains(got, c.mustNot) || !strings.Contains(got, c.want) {
			t.Errorf("RedactSecrets(%q) = %q", c.in, got)
		}
	}
	// Ordinary text is untouched.
	for _, s := range []string{"session tg:76017910 boundary 996", "2026-09-26T06:48:10.495+0800", "commit a75c993 on fix/social-intent-grounding"} {
		if got := RedactSecrets(s); got != s {
			t.Errorf("benign text changed: %q -> %q", s, got)
		}
	}
}

func TestRegisterSecret(t *testing.T) {
	// 短值现在也会登记（保险库允许），但低于遮蔽下限的值不能把普通文本打码。
	RegisterSecret("a")
	RegisterSecret("short")
	if got := RedactSecrets("a short word"); got != "a short word" {
		t.Fatalf("very short credentials must not corrupt ordinary text: %q", got)
	}
	RegisterSecret("my-custom-secret-value-42")
	if got := RedactSecrets("err: my-custom-secret-value-42 rejected"); strings.Contains(got, "custom-secret") {
		t.Fatalf("registered secret not masked: %q", got)
	}
	// 6~11 字节的短值：赋值上下文遮蔽，普通单词 / 日期内部不误伤。
	RegisterSecret("pin123")
	if got := RedactSecrets("token=pin123&x=1"); !strings.Contains(got, "token=***") {
		t.Fatalf("short credential in assignment context not masked: %q", got)
	}
	if got := RedactSecrets("prefix-pin123-suffix"); got != "prefix-pin123-suffix" {
		t.Fatalf("short credential corrupted a longer token: %q", got)
	}
}

func TestRegisterSecret_Verbatim(t *testing.T) {
	const value = " spaced-test-credential "
	RegisterSecret(value)
	t.Cleanup(func() { UnregisterSecret(value) })
	if got := RedactSecrets("x" + value + "y"); !strings.Contains(got, RedactedPlaceholder) {
		t.Fatalf("value with surrounding spaces not masked verbatim: %q", got)
	}
}

func TestUnregisterSecret_RefCounted(t *testing.T) {
	const value = "ref-counted-credential"
	RegisterSecret(value)
	RegisterSecret(value)
	UnregisterSecret(value)
	if got := RedactSecrets("err: " + value); !strings.Contains(got, RedactedPlaceholder) {
		t.Fatalf("value unmasked while another owner still holds it: %q", got)
	}
	UnregisterSecret(value)
	if got := RedactSecrets("err: " + value); strings.Contains(got, RedactedPlaceholder) {
		t.Fatalf("value still masked after the last owner released it: %q", got)
	}
	UnregisterSecret(value) // 重复释放是空操作
}

func TestRedactingWriteSyncer_MasksErrorFields(t *testing.T) {
	var buf bytes.Buffer
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		NewRedactingWriteSyncer(zapcore.AddSync(&buf)),
		zapcore.DebugLevel,
	)
	l := zap.New(core).Sugar()
	l.Warnw("http request failed",
		"url", "https://api.telegram.org/bot***/getUpdates",
		"err", &testErr{"Post \"https://api.telegram.org/bot" + fakeTGToken + "/getUpdates\": context canceled"},
		"last_err", "http request failed: bot"+fakeTGToken)
	out := buf.String()
	if strings.Contains(out, "AAFakeToken") {
		t.Fatalf("token leaked to log output: %s", out)
	}
	if !strings.Contains(out, "http request failed") || !strings.Contains(out, "context canceled") {
		t.Fatalf("entry content lost: %s", out)
	}
}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }
