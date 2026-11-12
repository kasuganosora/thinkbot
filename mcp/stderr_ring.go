package mcp

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// stderrTailBytes 是握手失败或子进程退出时附带的 stderr 尾部上限。
const stderrTailBytes = 4096

// stderrSecretPatterns 遮盖常见的凭据写法。显式 redact 列表之外的兜底。
var stderrSecretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/\s:@]+:)[^@\s/]+@`), "${1}***@"},
	{regexp.MustCompile(`(?i)\b(bearer\s+)\S+`), "${1}***"},
	{regexp.MustCompile(`(?i)\b((?:api[_-]?key|access[_-]?token|token|secret|password|passwd|pwd)\s*[=:]\s*)\S+`), "${1}***"},
}

// stderrRing 保留子进程 stderr 的尾部，供握手失败或进程退出时打日志。
type stderrRing struct {
	mu     sync.Mutex
	buf    []byte
	max    int
	redact []string
}

func newStderrRing(max int, redact []string) *stderrRing {
	if max <= 0 {
		max = stderrTailBytes
	}
	return &stderrRing{
		max:    max,
		redact: append([]string(nil), redact...),
	}
}

func (r *stderrRing) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.max:]...)
		for len(r.buf) > 0 && r.buf[0]&0xC0 == 0x80 {
			r.buf = r.buf[1:]
		}
	}
	return len(p), nil
}

// Tail 返回已打码的尾部文本。
func (r *stderrRing) Tail() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return RedactStderr(string(r.buf), r.redact)
}

// RedactStderr 遮盖显式密文以及 token/password/bearer/URL 用户信息这类常见写法。
// 握手失败时的 stderr 尾部和实时 stderr 日志行都走这里。
func RedactStderr(s string, extra []string) string {
	secrets := append([]string(nil), extra...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, "***")
	}
	for _, pat := range stderrSecretPatterns {
		s = pat.re.ReplaceAllString(s, pat.repl)
	}
	return s
}
