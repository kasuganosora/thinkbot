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
	RegisterSecret("short") // ignored (too short)
	if RedactSecrets("a short word") != "a short word" {
		t.Fatal("short secrets must be ignored")
	}
	RegisterSecret("my-custom-secret-value-42")
	if got := RedactSecrets("err: my-custom-secret-value-42 rejected"); strings.Contains(got, "custom-secret") {
		t.Fatalf("registered secret not masked: %q", got)
	}
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
