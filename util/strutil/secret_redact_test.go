package strutil

import "testing"

func TestReplaceSecretValue_LengthTiers(t *testing.T) {
	cases := []struct {
		name, text, value, want string
	}{
		{"below masking floor is left intact", "status=true&a=1", "true", "status=true&a=1"},
		{"empty value", "any text", "", "any text"},
		{"short value in assignment context", "pin=123456&next=1", "123456", "pin=***&next=1"},
		{"short value inside a date is kept", "2026-123456-11", "123456", "2026-123456-11"},
		{"short value inside a field name is kept", "secret_name=ok", "secret", "secret_name=ok"},
		{"short value inside a longer token is kept", "prefix-1234567-suffix", "123456", "prefix-1234567-suffix"},
		{"unicode short value", "密码是秘密", "密码", "***是秘密"},
		{"long value replaced unconditionally", "x ABCDEFGHIJKLMN y", "ABCDEFGHIJKLMN", "x *** y"},
		{"long value inside a word still replaced", "prefixABCDEFGHIJKLMNsuffix", "ABCDEFGHIJKLMN", "prefix***suffix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReplaceSecretValue(c.text, c.value, "***"); got != c.want {
				t.Fatalf("ReplaceSecretValue(%q, %q) = %q, want %q", c.text, c.value, got, c.want)
			}
		})
	}
}

func TestReplaceSecretValueBytes_NoMatchDoesNotCopy(t *testing.T) {
	p := []byte("ordinary log line")
	got := ReplaceSecretValueBytes(p, "pin-1234", []byte("***"))
	if string(got) != "ordinary log line" {
		t.Fatalf("unexpected rewrite: %q", got)
	}
	// 无匹配时应返回原切片本身（零拷贝）。
	if &got[0] != &p[0] {
		t.Fatal("no-match path copied the buffer")
	}
}
